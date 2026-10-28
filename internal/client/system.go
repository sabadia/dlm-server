package client

import (
	"context"
	"fmt"
	"net"
	"net/http"

	"github.com/sabadia/dlm-server/internal/config"
)

// Health is the response of the get-health operation.
type Health struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	// APIVersion is the daemon's REST API version; MinClientAPI the oldest
	// client API version it still supports.
	APIVersion    int  `json:"apiVersion"`
	MinClientAPI  int  `json:"minClientApi"`
	Authenticated bool `json:"authenticated"`
}

// Health calls get-health (GET /v1/health). It does not require a token;
// Authenticated reports whether the client's token was accepted.
func (c *Client) Health(ctx context.Context) (*Health, error) {
	var out Health
	if err := c.do(ctx, http.MethodGet, "/v1/health", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// BaseURL derives the API base URL from the daemon's boot configuration:
// http(s)://<server.address>. A wildcard listen host (empty, 0.0.0.0, ::) is
// replaced with the matching loopback address so a local client can connect.
func BaseURL(cfg *config.Config) (string, error) {
	host, port, err := net.SplitHostPort(cfg.Server.Address)
	if err != nil {
		return "", fmt.Errorf("client: server address %q: %w", cfg.Server.Address, err)
	}
	switch host {
	case "", "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	}
	scheme := "http"
	if cfg.TLSEnabled() {
		scheme = "https"
	}
	return scheme + "://" + net.JoinHostPort(host, port), nil
}
