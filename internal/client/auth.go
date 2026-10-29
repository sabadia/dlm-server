package client

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// WhoAmI is the answer of whoami.
type WhoAmI struct {
	Name    string `json:"name"`
	Scope   string `json:"scope"` // admin, read or extension
	Primary bool   `json:"primary"`
	Local   bool   `json:"local"`
	TLS     bool   `json:"tls"`
}

// WhoAmI calls whoami (GET /v1/auth/whoami).
func (c *Client) WhoAmI(ctx context.Context) (*WhoAmI, error) {
	var out WhoAmI
	if err := c.do(ctx, http.MethodGet, "/v1/auth/whoami", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// APIToken is a named token as listed.
type APIToken struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Scope      string     `json:"scope"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	// Kind is "extension" for a paired browser extension; Browser, ClientID
	// and ClientVersion describe it.
	Kind          string `json:"kind,omitempty"`
	Browser       string `json:"browser,omitempty"`
	ClientID      string `json:"clientId,omitempty"`
	ClientVersion string `json:"clientVersion,omitempty"`
}

// CreatedToken is the answer of create-token and rotate-token.
type CreatedToken struct {
	Token string    `json:"token"`
	Info  *APIToken `json:"info,omitempty"`
}

// RotateToken calls rotate-token (POST /v1/auth/token/rotate).
func (c *Client) RotateToken(ctx context.Context) (*CreatedToken, error) {
	var out CreatedToken
	if err := c.do(ctx, http.MethodPost, "/v1/auth/token/rotate", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListTokens calls list-tokens (GET /v1/auth/tokens).
func (c *Client) ListTokens(ctx context.Context) ([]APIToken, error) {
	var out struct {
		Tokens []APIToken `json:"tokens"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/auth/tokens", nil, &out); err != nil {
		return nil, err
	}
	return out.Tokens, nil
}

// CreateToken calls create-token (POST /v1/auth/tokens).
func (c *Client) CreateToken(ctx context.Context, name, scope string) (*CreatedToken, error) {
	body := struct {
		Name  string `json:"name"`
		Scope string `json:"scope"`
	}{name, scope}
	var out CreatedToken
	if err := c.do(ctx, http.MethodPost, "/v1/auth/tokens", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteToken calls delete-token (DELETE /v1/auth/tokens/{id}).
func (c *Client) DeleteToken(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/auth/tokens/"+url.PathEscape(id), nil, nil)
}
