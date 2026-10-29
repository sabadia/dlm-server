package client

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

// ExportOptions select the tasks ExportTasks writes.
type ExportOptions struct {
	// Format is json (default), urls or aria2.
	Format  string
	QueueID string
	Status  []string
	Search  string
}

// ExportedTasks is the result of ExportTasks.
type ExportedTasks struct {
	Format  string `json:"format"`
	Count   int    `json:"count"`
	Content string `json:"content"`
}

// ExportTasks calls export-tasks (GET /v1/tasks/export).
func (c *Client) ExportTasks(ctx context.Context, opts ExportOptions) (*ExportedTasks, error) {
	q := url.Values{}
	if opts.Format != "" {
		q.Set("format", opts.Format)
	}
	if opts.QueueID != "" {
		q.Set("queueId", opts.QueueID)
	}
	if len(opts.Status) > 0 {
		q.Set("status", strings.Join(opts.Status, ","))
	}
	if opts.Search != "" {
		q.Set("q", opts.Search)
	}
	path := "/v1/tasks/export"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out ExportedTasks
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ImportRequest is the request of import-tasks.
type ImportRequest struct {
	Format         string `json:"format"`
	Content        string `json:"content"`
	QueueID        string `json:"queueId,omitempty"`
	Paused         bool   `json:"paused,omitempty"`
	AllowDuplicate bool   `json:"allowDuplicate,omitempty"`
	ExpandPatterns bool   `json:"expandPatterns,omitempty"`
	// DryRun creates nothing and reports per entry whether it would be added.
	DryRun bool `json:"dryRun,omitempty"`
}

// ImportTasks calls import-tasks (POST /v1/tasks/import); the result has one
// item per created task.
func (c *Client) ImportTasks(ctx context.Context, req ImportRequest) ([]BatchItem, error) {
	var out batchResponse
	if err := c.do(ctx, http.MethodPost, "/v1/tasks/import", req, &out); err != nil {
		return nil, err
	}
	return out.Results, nil
}

// Stats is the result of GetStats.
type Stats struct {
	Tasks           int            `json:"tasks"`
	ByStatus        map[string]int `json:"byStatus"`
	ByEngine        []EngineStats  `json:"byEngine"`
	DownloadedBytes int64          `json:"downloadedBytes"`
	Completed       []CompletedRow `json:"completed"`
}

// EngineStats are the tasks of one engine.
type EngineStats struct {
	Engine          string `json:"engine"`
	Tasks           int    `json:"tasks"`
	DownloadedBytes int64  `json:"downloadedBytes"`
}

// CompletedRow are the downloads completed within a period (24h, 7d, 30d).
type CompletedRow struct {
	Period string `json:"period"`
	Tasks  int    `json:"tasks"`
	Bytes  int64  `json:"bytes"`
}

// GetStats calls get-stats (GET /v1/stats).
func (c *Client) GetStats(ctx context.Context) (*Stats, error) {
	var out Stats
	if err := c.do(ctx, http.MethodGet, "/v1/stats", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
