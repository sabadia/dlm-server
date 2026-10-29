package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// LogRecord is one record of the daemon's log.
type LogRecord struct {
	ID      uint64         `json:"id"`
	Time    time.Time      `json:"time"`
	Level   string         `json:"level"`
	Message string         `json:"message"`
	Attrs   map[string]any `json:"attrs,omitempty"`
}

// LogsOptions filter the log; empty values mean no filter.
type LogsOptions struct {
	Level string
	Query string
	// Since and Until are RFC 3339 times.
	Since, Until string
	Limit        int
	// Before pages backwards: only records older than this id.
	Before uint64
}

// LogList is the answer of get-logs.
type LogList struct {
	Records []LogRecord `json:"records"`
	HasMore bool        `json:"hasMore"`
	LastID  uint64      `json:"lastId"`
	BootID  string      `json:"bootId"`
}

func logsQuery(o LogsOptions) url.Values {
	q := url.Values{}
	for k, v := range map[string]string{"level": o.Level, "q": o.Query, "since": o.Since, "until": o.Until} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if o.Limit > 0 {
		q.Set("limit", strconv.Itoa(o.Limit))
	}
	if o.Before > 0 {
		q.Set("before", strconv.FormatUint(o.Before, 10))
	}
	return q
}

// GetLogs calls get-logs (GET /v1/logs).
func (c *Client) GetLogs(ctx context.Context, o LogsOptions) (*LogList, error) {
	path := "/v1/logs"
	if q := logsQuery(o); len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out LogList
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// StreamLogs calls stream-logs (GET /v1/logs/stream) and calls onRecord for
// every record until ctx ends, the daemon closes the stream or onRecord
// returns an error. lastEventID (<bootId>-<id>, from the last record or from
// get-logs) resumes after that record; onReset hears that records may have
// been lost ("gap" or "overflow"). Only Level and Query of o are used.
func (c *Client) StreamLogs(ctx context.Context, o LogsOptions, lastEventID string, onRecord func(LogRecord) error, onReset func(reason string)) error {
	q := logsQuery(LogsOptions{Level: o.Level, Query: o.Query})
	path := "/v1/logs/stream"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	req, err := c.newRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return c.transportError(http.MethodGet, "/v1/logs/stream", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return decodeError(resp)
	}
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt != "text/event-stream" {
		return fmt.Errorf("client: stream-logs: unexpected content type %q", resp.Header.Get("Content-Type"))
	}
	p := newSSEParser(resp.Body)
	for {
		m, err := p.next()
		if errors.Is(err, io.EOF) || (err != nil && ctx.Err() != nil) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("client: stream-logs: %w", err)
		}
		switch m.Event {
		case "reset":
			var d struct {
				Reason string `json:"reason"`
			}
			if json.Unmarshal([]byte(m.Data), &d) == nil && onReset != nil {
				onReset(d.Reason)
			}
		case "log":
			var rec LogRecord
			if err := json.Unmarshal([]byte(m.Data), &rec); err != nil {
				return fmt.Errorf("client: stream-logs: decode record: %w", err)
			}
			if err := onRecord(rec); err != nil {
				return err
			}
		}
	}
}

// WebhookDelivery is one attempt to send an event to a webhook.
type WebhookDelivery struct {
	ID         uint64    `json:"id"`
	Time       time.Time `json:"time"`
	Event      string    `json:"event"`
	EventID    uint64    `json:"eventId"`
	Attempt    int       `json:"attempt"`
	OK         bool      `json:"ok"`
	Status     int       `json:"status"`
	DurationMs int64     `json:"durationMs"`
	Error      string    `json:"error,omitempty"`
	WillRetry  bool      `json:"willRetry"`
}

// WebhookDeliveries calls list-webhook-deliveries
// (GET /v1/webhooks/{id}/deliveries); the newest attempt comes first.
func (c *Client) WebhookDeliveries(ctx context.Context, id string) ([]WebhookDelivery, error) {
	var out struct {
		Deliveries []WebhookDelivery `json:"deliveries"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/webhooks/"+url.PathEscape(id)+"/deliveries", nil, &out); err != nil {
		return nil, err
	}
	return out.Deliveries, nil
}

// PluginLogs calls get-plugin-logs (GET /v1/plugins/{name}/logs).
func (c *Client) PluginLogs(ctx context.Context, name string, o LogsOptions) (*LogList, error) {
	q := logsQuery(LogsOptions{Level: o.Level, Query: o.Query, Limit: o.Limit, Before: o.Before})
	path := "/v1/plugins/" + url.PathEscape(name) + "/logs"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out LogList
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
