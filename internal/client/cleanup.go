package client

import (
	"context"
	"net/http"
)

// CleanupRequest is the request of cleanup-tasks.
type CleanupRequest struct {
	OlderThanDays int    `json:"olderThanDays"`
	QueueID       string `json:"queueId,omitempty"`
	Files         bool   `json:"files,omitempty"`
	DryRun        bool   `json:"dryRun,omitempty"`
}

// CleanupResult is the answer of cleanup-tasks.
type CleanupResult struct {
	Matched int   `json:"matched"`
	Bytes   int64 `json:"bytes"`
	Deleted int   `json:"deleted"`
	Failed  int   `json:"failed"`
}

// CleanupTasks calls cleanup-tasks (POST /v1/tasks/cleanup).
func (c *Client) CleanupTasks(ctx context.Context, req CleanupRequest) (*CleanupResult, error) {
	var out CleanupResult
	if err := c.do(ctx, http.MethodPost, "/v1/tasks/cleanup", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
