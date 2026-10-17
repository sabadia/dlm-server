package client

import (
	"context"
	"net/http"
	"time"
)

// RefreshAttempt is one entry of a task's refresh history.
type RefreshAttempt struct {
	ID       string `json:"id"`
	TaskID   string `json:"taskId"`
	Strategy string `json:"strategy"`
	// Cause is what started the refresh: expired, manual, capture, source or timeout.
	Cause string `json:"cause"`
	// Outcome is success, failed, mismatch, restarted, waiting, timeout or skipped.
	Outcome    string     `json:"outcome"`
	Error      string     `json:"error,omitempty"`
	OldURL     string     `json:"oldUrl,omitempty"`
	NewURL     string     `json:"newUrl,omitempty"`
	Downloaded int64      `json:"downloaded"`
	Deadline   *time.Time `json:"deadline,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
}

// UpdateTaskSourceRequest is the request of update-task-source. Only
// CurrentURL is required; empty fields keep the task's values.
type UpdateTaskSourceRequest struct {
	CurrentURL    string            `json:"currentUrl"`
	OriginalURL   string            `json:"originalUrl,omitempty"`
	PageURL       string            `json:"pageUrl,omitempty"`
	Referrer      string            `json:"referrer,omitempty"`
	Method        string            `json:"method,omitempty"`
	Body          []byte            `json:"body,omitempty"`
	Headers       map[string]string `json:"headers,omitempty"`
	UserAgent     string            `json:"userAgent,omitempty"`
	CookieDomains []string          `json:"cookieDomains,omitempty"`
	MediaHint     string            `json:"mediaHint,omitempty"`
	// OnMismatch is restart or fail; empty uses the task's refresh.onMismatch.
	OnMismatch string `json:"onMismatch,omitempty"`
}

// RefreshTask calls refresh-task (POST /v1/tasks/{id}/refresh).
func (c *Client) RefreshTask(ctx context.Context, id string) (*Task, error) {
	return c.taskCall(ctx, http.MethodPost, taskPath(id, "/refresh"), nil)
}

// AwaitTaskLink calls await-task-link (POST /v1/tasks/{id}/refresh/await).
func (c *Client) AwaitTaskLink(ctx context.Context, id string) (*Task, error) {
	return c.taskCall(ctx, http.MethodPost, taskPath(id, "/refresh/await"), nil)
}

// UpdateTaskSource calls update-task-source (PATCH /v1/tasks/{id}/source).
func (c *Client) UpdateTaskSource(ctx context.Context, id string, req UpdateTaskSourceRequest) (*Task, error) {
	var out Task
	if err := c.doAs(ctx, http.MethodPatch, taskPath(id, "/source"), mergePatchType, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TaskRefreshes calls list-task-refreshes (GET /v1/tasks/{id}/refresh/history).
func (c *Client) TaskRefreshes(ctx context.Context, id string) ([]RefreshAttempt, error) {
	var out struct {
		Attempts []RefreshAttempt `json:"attempts"`
	}
	if err := c.do(ctx, http.MethodGet, taskPath(id, "/refresh/history"), nil, &out); err != nil {
		return nil, err
	}
	return out.Attempts, nil
}
