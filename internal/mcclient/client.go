package mcclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// PeerInfo mirrors the MC model for peer descriptors.
type PeerInfo struct {
	NodeID          string `json:"node_id"`
	PublicAddr      string `json:"public_addr"`
	CertFingerprint string `json:"cert_fingerprint"`
}

// HeartbeatRequest is the payload sent on each heartbeat.
type HeartbeatRequest struct {
	AgentVersion string `json:"agent_version"`
	PublicAddr   string `json:"public_addr"`
	PeersVersion int    `json:"peers_version"`
}

// HeartbeatResponse is what the MC returns on a successful heartbeat.
type HeartbeatResponse struct {
	Status             string     `json:"status"`
	GracePeriodSeconds int        `json:"grace_period_seconds"`
	PeersVersion       int        `json:"peers_version"`
	Peers              []PeerInfo `json:"peers,omitempty"`
}

// RegisterRequest is the payload for node registration.
type RegisterRequest struct {
	InviteToken     string `json:"invite_token"`
	Name            string `json:"name"`
	PublicAddr      string `json:"public_addr"`
	CertFingerprint string `json:"cert_fingerprint"`
}

// RegisterResponse is returned once on successful node registration.
type RegisterResponse struct {
	NodeID             string `json:"node_id"`
	APIKey             string `json:"api_key"`
	GracePeriodSeconds int    `json:"grace_period_seconds"`
}

// Client communicates with the torsync management center.
type Client struct {
	mcURL      string
	apiKey     string
	nodeID     string
	httpClient *http.Client
}

// New creates a Client configured to communicate with the given MC URL.
func New(mcURL, apiKey, nodeID string) *Client {
	return &Client{
		mcURL:  mcURL,
		apiKey: apiKey,
		nodeID: nodeID,
		httpClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

// Register registers a new node with the MC using an invite token.
// This is called once during node provisioning.
func Register(ctx context.Context, mcURL, inviteToken, name, publicAddr, certFingerprint string) (*RegisterResponse, error) {
	payload := RegisterRequest{
		InviteToken:     inviteToken,
		Name:            name,
		PublicAddr:      publicAddr,
		CertFingerprint: certFingerprint,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("mcclient: marshal register request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, mcURL+"/v1/nodes/register", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("mcclient: build register request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcclient: register http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("mcclient: register failed %d: %s", resp.StatusCode, string(errBody))
	}

	var result RegisterResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("mcclient: decode register response: %w", err)
	}

	return &result, nil
}

// Heartbeat sends a heartbeat to the MC and returns the response.
func (c *Client) Heartbeat(ctx context.Context, req HeartbeatRequest) (*HeartbeatResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("mcclient: marshal heartbeat: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		fmt.Sprintf("%s/v1/nodes/%s/heartbeat", c.mcURL, c.nodeID),
		bytes.NewReader(body),
	)
	if err != nil {
		return nil, fmt.Errorf("mcclient: build heartbeat request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-API-Key", c.apiKey)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("mcclient: heartbeat http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("mcclient: heartbeat failed %d: %s", resp.StatusCode, string(errBody))
	}

	var result HeartbeatResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("mcclient: decode heartbeat response: %w", err)
	}

	return &result, nil
}

// StartHeartbeatLoop sends heartbeats at the given interval.
// onPeersUpdate is called when the MC sends a new peer list.
// onDisabled is called when the node or swarm is disabled, or when the MC has
// been unreachable for longer than the grace period.
func (c *Client) StartHeartbeatLoop(
	ctx context.Context,
	interval time.Duration,
	currentPeersVersion func() int,
	currentPublicAddr func() string,
	getLastMCSuccess func() time.Time,
	setLastMCSuccess func(time.Time),
	getGracePeriod func() int,
	onPeersUpdate func([]PeerInfo),
	onDisabled func(),
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			req := HeartbeatRequest{
				AgentVersion: "1.0.0",
				PublicAddr:   currentPublicAddr(),
				PeersVersion: currentPeersVersion(),
			}

			resp, err := c.Heartbeat(ctx, req)
			if err != nil {
				// Check if we have exceeded the grace period.
				last := getLastMCSuccess()
				grace := getGracePeriod()
				if !last.IsZero() && grace > 0 && time.Since(last) > time.Duration(grace)*time.Second {
					onDisabled()
				}
				continue
			}

			// Record successful contact time.
			setLastMCSuccess(time.Now())

			if resp.Status == "disabled" {
				onDisabled()
				return
			}

			if len(resp.Peers) > 0 {
				onPeersUpdate(resp.Peers)
			}
		}
	}
}
