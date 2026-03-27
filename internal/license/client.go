// Package license manages the SSC license token lifecycle for a torSync instance.
// It fetches a signed JWT from SSC on startup and refreshes it periodically via heartbeat.
// If SSC is unreachable, the last valid token is used until it expires (tolerance window).
// If the token status is "expired", the service must stop.
package license

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Claims represents the license JWT payload for a torSync instance.
type Claims struct {
	InstanceID     string    `json:"sub"`
	OperatorID     string    `json:"operator_id"`
	Service        string    `json:"service"`
	SubscriptionID string    `json:"subscription_id"`
	Status         string    `json:"status"`       // "active", "grace", "expired"
	GraceUntil     time.Time `json:"grace_until_time"`
	ExpiresAt      time.Time `json:"exp_time"`

	// Raw numeric fields for parsing from the JWT payload.
	rawExp        int64
	rawIat        int64
	rawGraceUntil int64
}

// rawClaims is used to unmarshal the JWT payload before converting timestamps.
type rawClaims struct {
	Sub            string `json:"sub"`
	OperatorID     string `json:"operator_id"`
	Service        string `json:"service"`
	SubscriptionID string `json:"subscription_id"`
	Status         string `json:"status"`
	GraceUntil     int64  `json:"grace_until"`
	Exp            int64  `json:"exp"`
	Iat            int64  `json:"iat"`
}

// tokenResponse is the JSON body returned by POST /v1/license/token.
type tokenResponse struct {
	Token string `json:"token"`
}

// Client manages the SSC license token lifecycle for a torSync instance.
type Client struct {
	sscURL string
	apiKey string

	mu     sync.RWMutex
	token  string
	claims *Claims

	httpClient *http.Client
}

// New creates a Client configured to communicate with the given SSC URL.
func New(sscURL, apiKey string) *Client {
	return &Client{
		sscURL: sscURL,
		apiKey: apiKey,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

// Start fetches a license token from SSC, parses its claims, and launches the
// background heartbeat loop. Returns an error immediately if the token status
// is "expired" so the caller can stop the service.
func (c *Client) Start(ctx context.Context) error {
	token, claims, err := c.fetchToken(ctx)
	if err != nil {
		return fmt.Errorf("license: fetch token: %w", err)
	}

	c.mu.Lock()
	c.token = token
	c.claims = claims
	c.mu.Unlock()

	if claims.Status == "expired" {
		return fmt.Errorf("license: token status is expired")
	}

	// Derive heartbeat interval from token TTL (exp - iat) / 2.
	interval := 30 * time.Minute
	if claims.rawExp > 0 && claims.rawIat > 0 {
		ttl := time.Duration(claims.rawExp-claims.rawIat) * time.Second
		if ttl > 0 {
			interval = ttl / 2
		}
	}

	// Create a cancellable child context so the heartbeat loop can stop the
	// service if SSC reports the subscription as expired.
	hbCtx, hbCancel := context.WithCancel(ctx)

	go c.heartbeatLoop(hbCtx, hbCancel, interval)

	return nil
}

// heartbeatLoop ticks at the given interval and refreshes the license token.
func (c *Client) heartbeatLoop(ctx context.Context, cancel context.CancelFunc, interval time.Duration) {
	defer cancel()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			token, claims, err := c.doHeartbeat(ctx)
			if err != nil {
				// Network or transient error — keep using the last valid token
				// until it naturally expires.
				log.Printf("license: heartbeat failed (using cached token): %v", err)

				// If the HTTP error was a 403, the subscription has been revoked.
				if isForbiddenErr(err) {
					log.Printf("license: SSC returned 403 — subscription expired, stopping service")
					cancel()
					return
				}
				continue
			}

			c.mu.Lock()
			c.token = token
			c.claims = claims
			c.mu.Unlock()

			if claims.Status == "expired" {
				log.Printf("license: token status is expired — stopping service")
				cancel()
				return
			}
		}
	}
}

// IsActive returns true if the current token is "active" or "grace" and has
// not yet expired. Returns false if the status is "expired" or the token's
// expiry is in the past.
func (c *Client) IsActive() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.claims == nil {
		return false
	}

	if c.claims.Status == "expired" {
		return false
	}

	if !c.claims.ExpiresAt.IsZero() && time.Now().After(c.claims.ExpiresAt) {
		return false
	}

	return c.claims.Status == "active" || c.claims.Status == "grace"
}

// Status returns the current license status string, or "unknown" if no token
// has been fetched yet.
func (c *Client) Status() string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if c.claims == nil {
		return "unknown"
	}
	return c.claims.Status
}

// fetchToken calls POST {sscURL}/v1/license/token and parses the JWT.
func (c *Client) fetchToken(ctx context.Context) (string, *Claims, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.sscURL+"/v1/license/token", nil)
	if err != nil {
		return "", nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("X-API-Key", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden {
		return "", nil, &forbiddenError{fmt.Errorf("ssc returned 403")}
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	var tr tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", nil, fmt.Errorf("decode response: %w", err)
	}

	claims, err := parseJWTClaims(tr.Token)
	if err != nil {
		return "", nil, fmt.Errorf("parse claims: %w", err)
	}

	return tr.Token, claims, nil
}

// doHeartbeat calls POST {sscURL}/v1/license/heartbeat and returns the
// refreshed token and parsed claims.
func (c *Client) doHeartbeat(ctx context.Context) (string, *Claims, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.sscURL+"/v1/license/heartbeat", nil)
	if err != nil {
		return "", nil, fmt.Errorf("build heartbeat request: %w", err)
	}
	req.Header.Set("X-API-Key", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden {
		return "", nil, &forbiddenError{fmt.Errorf("ssc returned 403")}
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	var tr tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", nil, fmt.Errorf("decode heartbeat response: %w", err)
	}

	claims, err := parseJWTClaims(tr.Token)
	if err != nil {
		return "", nil, fmt.Errorf("parse heartbeat claims: %w", err)
	}

	return tr.Token, claims, nil
}

// parseJWTClaims base64-decodes the middle segment of a JWT and unmarshals
// the JSON payload into Claims. Signature verification is not performed here —
// that is SSC's responsibility.
func parseJWTClaims(token string) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid jwt format: expected 3 segments, got %d", len(parts))
	}

	// JWT uses base64url encoding without padding.
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("base64 decode payload: %w", err)
	}

	var rc rawClaims
	if err := json.Unmarshal(payload, &rc); err != nil {
		return nil, fmt.Errorf("unmarshal claims: %w", err)
	}

	claims := &Claims{
		InstanceID:     rc.Sub,
		OperatorID:     rc.OperatorID,
		Service:        rc.Service,
		SubscriptionID: rc.SubscriptionID,
		Status:         rc.Status,
		rawExp:         rc.Exp,
		rawIat:         rc.Iat,
		rawGraceUntil:  rc.GraceUntil,
	}

	if rc.Exp > 0 {
		claims.ExpiresAt = time.Unix(rc.Exp, 0)
	}
	if rc.GraceUntil > 0 {
		claims.GraceUntil = time.Unix(rc.GraceUntil, 0)
	}

	return claims, nil
}

// forbiddenError wraps an HTTP 403 response so callers can detect it.
type forbiddenError struct {
	err error
}

func (e *forbiddenError) Error() string { return e.err.Error() }

// isForbiddenErr reports whether err is (or wraps) a forbiddenError.
func isForbiddenErr(err error) bool {
	if err == nil {
		return false
	}
	_, ok := err.(*forbiddenError)
	return ok
}
