// Package license manages the SSC license lifecycle for a torSync node.
//
// Two modes:
//
//  1. Free tier — no license key; allows up to 2 nodes in a swarm, 1 sync dir.
//     No SSC connection required.
//
//  2. Licensed — operator imports a .key file (JSON {"token":"<JWT>"}).
//     The JWT contains the raw API key, limits, and subscription expiry.
//     On startup, claims are decoded locally (no signature verification — the JWT
//     was received from SSC and its contents are trusted until exp).
//     A heartbeat loop refreshes a short-lived operational token at token_ttl/2
//     intervals; if SSC is unreachable the cached operational token is used until
//     it expires, then the node stops.
package license

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// NodeKeyClaims are the claims decoded from the long-lived node key JWT
// downloaded from SSC. No signature verification is performed locally.
type NodeKeyClaims struct {
	Sub               string `json:"sub"`                // instance_id
	APIKey            string `json:"api_key"`             // raw SSC API key
	OperatorID        string `json:"operator_id"`
	SwarmID           string `json:"swarm_id"`
	Service           string `json:"service"`
	SubscriptionID    string `json:"subscription_id"`
	Status            string `json:"status"`
	RegistrationUntil int64  `json:"registration_until"` // Unix timestamp
	MaxNodes          int    `json:"max_nodes"`
	Exp               int64  `json:"exp"` // Unix: business expiry
	Iat               int64  `json:"iat"`
}

// OperationalClaims are the claims in the short-lived token returned by the
// SSC license heartbeat endpoint.
type OperationalClaims struct {
	Sub        string `json:"sub"`
	Service    string `json:"service"`
	Status     string `json:"status"`
	GraceUntil int64  `json:"grace_until"`
	Exp        int64  `json:"exp"`
	Iat        int64  `json:"iat"`
}

// Peer is a torSync peer returned by the SSC heartbeat.
type Peer struct {
	InstanceID    string `json:"instance_id"`
	PublicAddress string `json:"public_address"`
}

// heartbeatResponse is the JSON body returned by POST /v1/license/heartbeat.
type heartbeatResponse struct {
	Token string `json:"token"`
	Peers []Peer `json:"peers,omitempty"`
}

// keyFile is the JSON structure of a downloaded .key file.
type keyFile struct {
	Token string `json:"token"`
}

// Client manages license enforcement for a torSync node.
type Client struct {
	sscURL string
	apiKey string // extracted from the node key JWT

	mu          sync.RWMutex
	nodeKey     *NodeKeyClaims
	operational *OperationalClaims
	peers       []Peer
	cachePath   string

	httpClient *http.Client
}

// NewFree returns a Client in free-tier mode (no SSC, no key).
// Only 2-node swarms and 1 sync dir are allowed.
func NewFree() *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

// NewFromKeyFile reads a .key file, decodes the JWT, and returns a Client
// ready to connect to SSC. Returns an error if the key is expired or the
// registration window has passed.
func NewFromKeyFile(keyPath, cachePath string) (*Client, error) {
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("license: read key file: %w", err)
	}

	var kf keyFile
	if err := json.Unmarshal(raw, &kf); err != nil {
		return nil, fmt.Errorf("license: parse key file: %w", err)
	}
	if kf.Token == "" {
		return nil, fmt.Errorf("license: key file has no token")
	}

	claims, err := decodeJWT[NodeKeyClaims](kf.Token)
	if err != nil {
		return nil, fmt.Errorf("license: decode node key: %w", err)
	}

	now := time.Now().Unix()

	// Enforce registration window — if the key has never been activated (no
	// cached operational token) and the registration window has closed, refuse.
	if claims.RegistrationUntil > 0 && now > claims.RegistrationUntil {
		// Only block if there is no cached operational token (first-time activation).
		cached, _ := loadCachedToken(cachePath)
		if cached == nil {
			return nil, fmt.Errorf("license: registration window closed %s ago — contact your administrator",
				time.Since(time.Unix(claims.RegistrationUntil, 0)).Round(time.Hour))
		}
	}

	// Enforce business expiry.
	if claims.Exp > 0 && now > claims.Exp {
		return nil, fmt.Errorf("license: node key has expired")
	}

	if claims.APIKey == "" {
		return nil, fmt.Errorf("license: node key contains no api_key — re-generate the key in SSC")
	}

	c := &Client{
		sscURL:     "", // set by Start via SetSSCURL
		apiKey:     claims.APIKey,
		nodeKey:    claims,
		cachePath:  cachePath,
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}

	// Load cached operational token if available.
	if op, err := loadCachedToken(cachePath); err == nil && op != nil {
		c.operational = op
	}

	return c, nil
}

// SetSSCURL configures the SSC base URL (may differ from the config file after import).
func (c *Client) SetSSCURL(url string) {
	c.sscURL = url
}

// Start fetches the first operational token from SSC and launches the heartbeat loop.
// In free-tier mode (c.apiKey == ""), this is a no-op.
func (c *Client) Start(ctx context.Context) error {
	if c.apiKey == "" {
		return nil // free tier — no SSC
	}
	if c.sscURL == "" {
		return fmt.Errorf("license: ssc_url not configured")
	}

	// Try online first; fall back to cache.
	if err := c.refresh(ctx); err != nil {
		log.Printf("[license] SSC unreachable (%v) — trying cached token", err)
		if c.operational == nil {
			return fmt.Errorf("license: cannot reach SSC and no cached token: %w", err)
		}
		if c.operational.Exp > 0 && time.Now().Unix() >= c.operational.Exp {
			return fmt.Errorf("license: cached token has expired and SSC is unreachable")
		}
		log.Printf("[license] Using cached operational token (expires %s)", time.Unix(c.operational.Exp, 0).Format(time.RFC3339))
	}

	// Derive heartbeat interval.
	interval := 30 * time.Minute
	c.mu.RLock()
	if c.operational != nil && c.operational.Exp > 0 && c.operational.Iat > 0 {
		ttl := time.Duration(c.operational.Exp-c.operational.Iat) * time.Second
		if ttl > 0 {
			interval = ttl / 2
			if interval < time.Minute {
				interval = time.Minute
			}
		}
	}
	c.mu.RUnlock()

	go c.heartbeatLoop(ctx, interval)
	return nil
}

// IsActive reports whether the license allows the service to run.
// Free tier is always active.
func (c *Client) IsActive() bool {
	if c.nodeKey == nil {
		return true // free tier
	}
	c.mu.RLock()
	defer c.mu.RUnlock()

	// Check business expiry from node key.
	if c.nodeKey.Exp > 0 && time.Now().Unix() >= c.nodeKey.Exp {
		return false
	}
	// Check operational token status.
	if c.operational != nil {
		if c.operational.Status == "expired" {
			return false
		}
		if c.operational.Exp > 0 && time.Now().Unix() >= c.operational.Exp {
			return false
		}
	}
	return true
}

// IsFree reports whether this node is running in free tier (no license key).
func (c *Client) IsFree() bool {
	return c.nodeKey == nil
}

// MaxNodes returns the node limit from the license, or 2 for free tier.
func (c *Client) MaxNodes() int {
	if c.nodeKey == nil {
		return 2
	}
	return c.nodeKey.MaxNodes
}

// Status returns a human-readable license status string.
func (c *Client) Status() string {
	if c.nodeKey == nil {
		return "free"
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.operational != nil {
		return c.operational.Status
	}
	return c.nodeKey.Status
}

// Peers returns the last known peer list from SSC (torSync only).
func (c *Client) Peers() []Peer {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Peer, len(c.peers))
	copy(out, c.peers)
	return out
}

// InstanceID returns the node's instance ID from the license key.
func (c *Client) InstanceID() string {
	if c.nodeKey == nil {
		return ""
	}
	return c.nodeKey.Sub
}

// ExpiresAt returns the subscription business expiry time, or zero for free tier.
func (c *Client) ExpiresAt() time.Time {
	if c.nodeKey == nil || c.nodeKey.Exp == 0 {
		return time.Time{}
	}
	return time.Unix(c.nodeKey.Exp, 0)
}

// ─── internal ─────────────────────────────────────────────────────────────────

func (c *Client) heartbeatLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.refresh(ctx); err != nil {
				log.Printf("[license] heartbeat failed: %v (using cached token)", err)
			}
		}
	}
}

func (c *Client) refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.sscURL+"/v1/license/heartbeat", nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("X-API-Key", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ssc 403 — subscription expired or revoked: %s", string(body))
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ssc %d: %s", resp.StatusCode, string(body))
	}

	var hbResp heartbeatResponse
	if err := json.NewDecoder(resp.Body).Decode(&hbResp); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}

	op, err := decodeJWT[OperationalClaims](hbResp.Token)
	if err != nil {
		return fmt.Errorf("decode operational token: %w", err)
	}

	c.mu.Lock()
	c.operational = op
	c.peers = hbResp.Peers
	c.mu.Unlock()

	_ = saveCachedToken(c.cachePath, hbResp.Token)
	return nil
}

// ─── cache helpers ────────────────────────────────────────────────────────────

type cachedToken struct {
	Token string `json:"token"`
}

func saveCachedToken(path, token string) error {
	if path == "" {
		return nil
	}
	data, err := json.Marshal(cachedToken{Token: token})
	if err != nil {
		return err
	}
	return os.WriteFile(path+".cache", data, 0600)
}

func loadCachedToken(path string) (*OperationalClaims, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path + ".cache")
	if err != nil {
		return nil, err
	}
	var ct cachedToken
	if err := json.Unmarshal(raw, &ct); err != nil {
		return nil, err
	}
	return decodeJWT[OperationalClaims](ct.Token)
}

// ─── JWT decode ───────────────────────────────────────────────────────────────

// decodeJWT base64url-decodes the payload segment of a JWT and unmarshals it
// into the target type. No signature verification is performed — the JWT is
// trusted because it was received from (or stored after a successful call to) SSC.
func decodeJWT[T any](token string) (*T, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid jwt: expected 3 segments")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("base64 decode: %w", err)
	}
	var out T
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, fmt.Errorf("unmarshal claims: %w", err)
	}
	return &out, nil
}
