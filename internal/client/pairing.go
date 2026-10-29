package client

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// Pairing is a browser extension's request to be given a token.
type Pairing struct {
	ID            string    `json:"id"`
	Status        string    `json:"status"` // pending, approved, denied or expired
	Name          string    `json:"name"`
	Browser       string    `json:"browser"`
	ClientID      string    `json:"clientId,omitempty"`
	ClientVersion string    `json:"clientVersion,omitempty"`
	Code          string    `json:"code"`
	CreatedAt     time.Time `json:"createdAt"`
	ExpiresAt     time.Time `json:"expiresAt"`
	TokenID       string    `json:"tokenId,omitempty"`
}

// PairingState is a pairing with, once, the token of an approved one.
type PairingState struct {
	Pairing
	Token string `json:"token,omitempty"`
}

// PairingRequest is what a client says about itself when it asks to pair.
type PairingRequest struct {
	Name     string `json:"name,omitempty"`
	Browser  string `json:"browser,omitempty"`
	ClientID string `json:"clientId,omitempty"`
	Version  string `json:"version,omitempty"`
}

// StartPairing calls start-pairing (POST /v1/pairing).
func (c *Client) StartPairing(ctx context.Context, req PairingRequest) (*Pairing, error) {
	var out Pairing
	if err := c.do(ctx, http.MethodPost, "/v1/pairing", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListPairings calls list-pairings (GET /v1/pairing).
func (c *Client) ListPairings(ctx context.Context) ([]Pairing, error) {
	var out struct {
		Pairings []Pairing `json:"pairings"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/pairing", nil, &out); err != nil {
		return nil, err
	}
	return out.Pairings, nil
}

// GetPairing calls get-pairing (GET /v1/pairing/{id}).
func (c *Client) GetPairing(ctx context.Context, id string) (*PairingState, error) {
	var out PairingState
	if err := c.do(ctx, http.MethodGet, "/v1/pairing/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ApprovePairing calls approve-pairing (POST /v1/pairing/{id}/approve).
func (c *Client) ApprovePairing(ctx context.Context, id string) (*Pairing, error) {
	var out Pairing
	if err := c.do(ctx, http.MethodPost, "/v1/pairing/"+url.PathEscape(id)+"/approve", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DenyPairing calls deny-pairing (POST /v1/pairing/{id}/deny).
func (c *Client) DenyPairing(ctx context.Context, id string) (*Pairing, error) {
	var out Pairing
	if err := c.do(ctx, http.MethodPost, "/v1/pairing/"+url.PathEscape(id)+"/deny", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
