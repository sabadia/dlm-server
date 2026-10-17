package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// mergePatchType is the RFC 7396 media type sent by the PATCH operations.
const mergePatchType = "application/merge-patch+json"

func queuePath(id string, rest ...string) string {
	return "/v1/queues/" + url.PathEscape(id) + strings.Join(rest, "")
}

// ListQueues calls list-queues (GET /v1/queues).
func (c *Client) ListQueues(ctx context.Context) ([]*Queue, error) {
	var out struct {
		Queues []*Queue `json:"queues"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/queues", nil, &out); err != nil {
		return nil, err
	}
	return out.Queues, nil
}

// CreateQueue calls create-queue (POST /v1/queues).
func (c *Client) CreateQueue(ctx context.Context, req CreateQueueRequest) (*Queue, error) {
	return c.queueCall(ctx, http.MethodPost, "/v1/queues", "", req)
}

// GetQueue calls get-queue (GET /v1/queues/{id}).
func (c *Client) GetQueue(ctx context.Context, id string) (*Queue, error) {
	return c.queueCall(ctx, http.MethodGet, queuePath(id), "", nil)
}

// UpdateQueue calls update-queue (PATCH /v1/queues/{id}).
func (c *Client) UpdateQueue(ctx context.Context, id string, req UpdateQueueRequest) (*Queue, error) {
	return c.queueCall(ctx, http.MethodPatch, queuePath(id), mergePatchType, req)
}

// ReplaceQueueSettings calls replace-queue-settings
// (PUT /v1/queues/{id}/settings) with a settings layer object.
func (c *Client) ReplaceQueueSettings(ctx context.Context, id string, layer json.RawMessage) (*Queue, error) {
	return c.queueCall(ctx, http.MethodPut, queuePath(id, "/settings"), "", layer)
}

// DeleteQueue calls delete-queue (DELETE /v1/queues/{id}). A non-empty
// moveTasksTo names the queue that receives the deleted queue's tasks.
func (c *Client) DeleteQueue(ctx context.Context, id, moveTasksTo string) error {
	path := queuePath(id)
	if moveTasksTo != "" {
		path += "?" + url.Values{"moveTasksTo": {moveTasksTo}}.Encode()
	}
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

// StartQueue calls start-queue (POST /v1/queues/{id}/start).
func (c *Client) StartQueue(ctx context.Context, id string) (*Queue, error) {
	return c.queueCall(ctx, http.MethodPost, queuePath(id, "/start"), "", nil)
}

// StopQueue calls stop-queue (POST /v1/queues/{id}/stop).
func (c *Client) StopQueue(ctx context.Context, id string) (*Queue, error) {
	return c.queueCall(ctx, http.MethodPost, queuePath(id, "/stop"), "", nil)
}

// ReorderQueue calls reorder-queue (POST /v1/queues/{id}/reorder).
func (c *Client) ReorderQueue(ctx context.Context, id string, position int) (*Queue, error) {
	return c.queueCall(ctx, http.MethodPost, queuePath(id, "/reorder"), "", positionBody{Position: position})
}

// QueueEffectiveSettings calls get-queue-effective-settings
// (GET /v1/queues/{id}/settings/effective).
func (c *Client) QueueEffectiveSettings(ctx context.Context, id string) (*EffectiveSettings, error) {
	var out EffectiveSettings
	if err := c.do(ctx, http.MethodGet, queuePath(id, "/settings/effective"), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) queueCall(ctx context.Context, method, path, contentType string, in any) (*Queue, error) {
	if contentType == "" {
		contentType = "application/json"
	}
	var out Queue
	if err := c.doAs(ctx, method, path, contentType, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetSettings calls get-settings (GET /v1/settings): the global layer.
func (c *Client) GetSettings(ctx context.Context) (json.RawMessage, error) {
	return c.settingsCall(ctx, http.MethodGet, "/v1/settings", "application/json", nil)
}

// ReplaceSettings calls replace-settings (PUT /v1/settings) with a settings
// layer object and returns the stored layer.
func (c *Client) ReplaceSettings(ctx context.Context, layer json.RawMessage) (json.RawMessage, error) {
	return c.settingsCall(ctx, http.MethodPut, "/v1/settings", "application/json", layer)
}

// PatchSettings calls patch-settings (PATCH /v1/settings) with an RFC 7396
// merge patch and returns the stored layer.
func (c *Client) PatchSettings(ctx context.Context, patch json.RawMessage) (json.RawMessage, error) {
	return c.settingsCall(ctx, http.MethodPatch, "/v1/settings", mergePatchType, patch)
}

// DefaultSettings calls get-default-settings (GET /v1/settings/defaults).
func (c *Client) DefaultSettings(ctx context.Context) (json.RawMessage, error) {
	return c.settingsCall(ctx, http.MethodGet, "/v1/settings/defaults", "application/json", nil)
}

func (c *Client) settingsCall(ctx context.Context, method, path, contentType string, in any) (json.RawMessage, error) {
	var out json.RawMessage
	if err := c.doAs(ctx, method, path, contentType, in, &out); err != nil {
		return nil, err
	}
	return withoutSchemaLink(out), nil
}

// withoutSchemaLink drops the "$schema" link the daemon adds to response
// objects, so a settings layer can be sent back unchanged.
func withoutSchemaLink(raw json.RawMessage) json.RawMessage {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	if _, ok := m["$schema"]; !ok {
		return raw
	}
	delete(m, "$schema")
	out, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return out
}
