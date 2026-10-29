package client

import (
	"context"
	"net/http"
)

// NativeHostBrowser is the registration of the host with one browser.
type NativeHostBrowser struct {
	Browser  string `json:"browser"`
	Manifest string `json:"manifest,omitempty"`
	Registry string `json:"registry,omitempty"`
	Status   string `json:"status"` // installed or absent
}

// NativeHostStatus is the answer of get-native-host.
type NativeHostStatus struct {
	Launcher       string              `json:"launcher"`
	LauncherExists bool                `json:"launcherExists"`
	Browsers       []NativeHostBrowser `json:"browsers"`
	AllowedOrigins []string            `json:"allowedOrigins"`
}

// NativeHostStatus calls get-native-host (GET /v1/native-host).
func (c *Client) NativeHostStatus(ctx context.Context) (*NativeHostStatus, error) {
	var out NativeHostStatus
	if err := c.do(ctx, http.MethodGet, "/v1/native-host", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
