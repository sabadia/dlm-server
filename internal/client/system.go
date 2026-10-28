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

// SystemInfo is the response of get-system-info.
type SystemInfo struct {
	Version       string `json:"version"`
	APIVersion    int    `json:"apiVersion"`
	MinClientAPI  int    `json:"minClientApi"`
	Commit        string `json:"commit,omitempty"`
	GoVersion     string `json:"goVersion"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	PID           int    `json:"pid"`
	StartedAt     string `json:"startedAt"`
	UptimeSeconds int64  `json:"uptimeSeconds"`
	Address       string `json:"address"`
	TLS           bool   `json:"tls"`
	Local         bool   `json:"local"`
	Paths         struct {
		Config    string `json:"config"`
		Data      string `json:"data"`
		Cache     string `json:"cache"`
		Log       string `json:"log"`
		Database  string `json:"database"`
		Downloads string `json:"downloads"`
	} `json:"paths"`
	Engines []string `json:"engines"`
	Tools   []struct {
		Name         string `json:"name"`
		Available    bool   `json:"available"`
		Path         string `json:"path,omitempty"`
		Version      string `json:"version,omitempty"`
		Source       string `json:"source,omitempty"`
		AutoDownload bool   `json:"autoDownload"`
		Error        string `json:"error,omitempty"`
	} `json:"tools"`
	DatabaseBytes int64 `json:"databaseBytes"`
}

// SystemInfo calls get-system-info (GET /v1/system/info).
func (c *Client) SystemInfo(ctx context.Context) (*SystemInfo, error) {
	var out SystemInfo
	if err := c.do(ctx, http.MethodGet, "/v1/system/info", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Shutdown calls shutdown-daemon (POST /v1/system/shutdown) and returns the
// status the daemon announced.
func (c *Client) Shutdown(ctx context.Context) (string, error) { return c.lifecycle(ctx, "shutdown") }

// Restart calls restart-daemon (POST /v1/system/restart) and returns the
// status the daemon announced.
func (c *Client) Restart(ctx context.Context) (string, error) { return c.lifecycle(ctx, "restart") }

func (c *Client) lifecycle(ctx context.Context, verb string) (string, error) {
	var out struct {
		Status string `json:"status"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/system/"+verb, nil, &out); err != nil {
		return "", err
	}
	return out.Status, nil
}
