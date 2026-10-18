package client

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// Webhook receives events as signed JSON POSTs. The secret is never returned,
// HasSecret says whether one is set.
type Webhook struct {
	ID        string    `json:"id"`
	URL       string    `json:"url"`
	Events    []string  `json:"events"`
	Enabled   bool      `json:"enabled"`
	HasSecret bool      `json:"hasSecret"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// CreateWebhookRequest is the request of create-webhook.
type CreateWebhookRequest struct {
	URL      string   `json:"url"`
	Events   []string `json:"events"`
	Secret   string   `json:"secret,omitempty"`
	Disabled bool     `json:"disabled,omitempty"`
}

// UpdateWebhookRequest is the request of update-webhook; nil fields are left
// unchanged and an empty Secret removes it.
type UpdateWebhookRequest struct {
	URL     *string   `json:"url,omitempty"`
	Events  *[]string `json:"events,omitempty"`
	Secret  *string   `json:"secret,omitempty"`
	Enabled *bool     `json:"enabled,omitempty"`
}

func webhookPath(id string) string { return "/v1/webhooks/" + url.PathEscape(id) }

// ListWebhooks calls list-webhooks (GET /v1/webhooks).
func (c *Client) ListWebhooks(ctx context.Context) ([]*Webhook, error) {
	var out struct {
		Webhooks []*Webhook `json:"webhooks"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/webhooks", nil, &out); err != nil {
		return nil, err
	}
	return out.Webhooks, nil
}

// CreateWebhook calls create-webhook (POST /v1/webhooks).
func (c *Client) CreateWebhook(ctx context.Context, req CreateWebhookRequest) (*Webhook, error) {
	var out Webhook
	if err := c.do(ctx, http.MethodPost, "/v1/webhooks", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetWebhook calls get-webhook (GET /v1/webhooks/{id}).
func (c *Client) GetWebhook(ctx context.Context, id string) (*Webhook, error) {
	var out Webhook
	if err := c.do(ctx, http.MethodGet, webhookPath(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateWebhook calls update-webhook (PATCH /v1/webhooks/{id}).
func (c *Client) UpdateWebhook(ctx context.Context, id string, req UpdateWebhookRequest) (*Webhook, error) {
	var out Webhook
	if err := c.do(ctx, http.MethodPatch, webhookPath(id), req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteWebhook calls delete-webhook (DELETE /v1/webhooks/{id}).
func (c *Client) DeleteWebhook(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, webhookPath(id), nil, nil)
}

// TestWebhook calls test-webhook (POST /v1/webhooks/{id}/test).
func (c *Client) TestWebhook(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, webhookPath(id)+"/test", nil, nil)
}
