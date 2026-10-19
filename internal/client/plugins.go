package client

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// Plugin is an installed plugin.
type Plugin struct {
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	Title       string    `json:"title,omitempty"`
	Description string    `json:"description,omitempty"`
	Source      string    `json:"source"`
	Enabled     bool      `json:"enabled"`
	Hooks       []string  `json:"hooks"`
	Net         []string  `json:"net,omitempty"`
	Error       string    `json:"error,omitempty"`
	InstalledAt time.Time `json:"installedAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// InstallPluginRequest is the request of install-plugin.
type InstallPluginRequest struct {
	Source   string `json:"source"`
	Ref      string `json:"ref,omitempty"`
	Replace  bool   `json:"replace,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
}

func pluginPath(name string) string { return "/v1/plugins/" + url.PathEscape(name) }

// ListPlugins calls list-plugins (GET /v1/plugins).
func (c *Client) ListPlugins(ctx context.Context) ([]*Plugin, error) {
	var out struct {
		Plugins []*Plugin `json:"plugins"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/plugins", nil, &out); err != nil {
		return nil, err
	}
	return out.Plugins, nil
}

// InstallPlugin calls install-plugin (POST /v1/plugins).
func (c *Client) InstallPlugin(ctx context.Context, req InstallPluginRequest) (*Plugin, error) {
	var out Plugin
	if err := c.do(ctx, http.MethodPost, "/v1/plugins", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetPlugin calls get-plugin (GET /v1/plugins/{name}).
func (c *Client) GetPlugin(ctx context.Context, name string) (*Plugin, error) {
	var out Plugin
	if err := c.do(ctx, http.MethodGet, pluginPath(name), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetPluginEnabled calls update-plugin (PATCH /v1/plugins/{name}).
func (c *Client) SetPluginEnabled(ctx context.Context, name string, enabled bool) (*Plugin, error) {
	var out Plugin
	body := struct {
		Enabled bool `json:"enabled"`
	}{Enabled: enabled}
	if err := c.do(ctx, http.MethodPatch, pluginPath(name), body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeletePlugin calls delete-plugin (DELETE /v1/plugins/{name}).
func (c *Client) DeletePlugin(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, pluginPath(name), nil, nil)
}
