package client

import (
	"context"
	"net/http"
)

// ConfigEntry is one key of the daemon's boot configuration.
type ConfigEntry struct {
	Key             string `json:"key"`
	Type            string `json:"type"` // string, bool, int, duration or list
	Value           any    `json:"value"`
	Default         any    `json:"default"`
	Source          string `json:"source"` // default, file, env or flag
	RestartRequired bool   `json:"restartRequired"`
	ReadOnly        bool   `json:"readOnly"`
	Secret          bool   `json:"secret"`
	// Pending is the value in the config file when it differs and applies at
	// the next start.
	Pending any `json:"pending,omitempty"`
}

// ConfigView is the answer of get-config.
type ConfigView struct {
	File           string        `json:"file"`
	FileExists     bool          `json:"fileExists"`
	FileError      string        `json:"fileError,omitempty"`
	RestartPending bool          `json:"restartPending"`
	Entries        []ConfigEntry `json:"entries"`
}

// GetConfig calls get-config (GET /v1/config).
func (c *Client) GetConfig(ctx context.Context) (*ConfigView, error) {
	var out ConfigView
	if err := c.do(ctx, http.MethodGet, "/v1/config", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateConfig calls update-config (PATCH /v1/config): a value sets the key
// in the config file, nil removes it.
func (c *Client) UpdateConfig(ctx context.Context, values map[string]any) (*ConfigView, error) {
	body := struct {
		Values map[string]any `json:"values"`
	}{values}
	var out ConfigView
	if err := c.do(ctx, http.MethodPatch, "/v1/config", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
