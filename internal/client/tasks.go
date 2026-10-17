package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// batchResponse is the body of the bulk operations.
type batchResponse struct {
	Results []BatchItem `json:"results"`
}

func taskPath(id string, rest ...string) string {
	return "/v1/tasks/" + url.PathEscape(id) + strings.Join(rest, "")
}

// ResolveTask calls resolve-task (POST /v1/resolve).
func (c *Client) ResolveTask(ctx context.Context, req ResolveRequest) (*ResolveResult, error) {
	var out ResolveResult
	if err := c.do(ctx, http.MethodPost, "/v1/resolve", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateTask calls create-task (POST /v1/tasks).
func (c *Client) CreateTask(ctx context.Context, req CreateTaskRequest) (*Task, error) {
	var out Task
	if err := c.do(ctx, http.MethodPost, "/v1/tasks", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateTasks calls create-tasks (POST /v1/tasks/batch). Items fail
// independently; the result has one item per request, in order.
func (c *Client) CreateTasks(ctx context.Context, reqs []CreateTaskRequest) ([]BatchItem, error) {
	in := struct {
		Items []CreateTaskRequest `json:"items"`
	}{Items: reqs}
	var out batchResponse
	if err := c.do(ctx, http.MethodPost, "/v1/tasks/batch", in, &out); err != nil {
		return nil, err
	}
	return out.Results, nil
}

// ListTasks calls list-tasks (GET /v1/tasks) and returns one page.
func (c *Client) ListTasks(ctx context.Context, opts ListTasksOptions) (*TaskPage, error) {
	q := url.Values{}
	if opts.QueueID != "" {
		q.Set("queueId", opts.QueueID)
	}
	if len(opts.Status) > 0 {
		q.Set("status", strings.Join(opts.Status, ","))
	}
	if opts.Search != "" {
		q.Set("q", opts.Search)
	}
	if opts.Sort != "" {
		q.Set("sort", opts.Sort)
	}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Cursor != "" {
		q.Set("cursor", opts.Cursor)
	}
	path := "/v1/tasks"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out TaskPage
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetTask calls get-task (GET /v1/tasks/{id}).
func (c *Client) GetTask(ctx context.Context, id string) (*Task, error) {
	return c.taskCall(ctx, http.MethodGet, taskPath(id), nil)
}

// UpdateTask calls update-task (PATCH /v1/tasks/{id}).
func (c *Client) UpdateTask(ctx context.Context, id string, req UpdateTaskRequest) (*Task, error) {
	var out Task
	if err := c.doAs(ctx, http.MethodPatch, taskPath(id), mergePatchType, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// MoveTask calls move-task (POST /v1/tasks/{id}/move).
func (c *Client) MoveTask(ctx context.Context, id, queueID string) (*Task, error) {
	in := struct {
		QueueID string `json:"queueId"`
	}{QueueID: queueID}
	return c.taskCall(ctx, http.MethodPost, taskPath(id, "/move"), in)
}

// ReorderTask calls reorder-task (POST /v1/tasks/{id}/reorder).
func (c *Client) ReorderTask(ctx context.Context, id string, position int) (*Task, error) {
	return c.taskCall(ctx, http.MethodPost, taskPath(id, "/reorder"), positionBody{Position: position})
}

// PauseTasks calls pause-tasks (POST /v1/tasks/pause).
func (c *Client) PauseTasks(ctx context.Context, sel TaskSelection) ([]BatchItem, error) {
	return c.bulk(ctx, "/v1/tasks/pause", sel)
}

// ResumeTasks calls resume-tasks (POST /v1/tasks/resume).
func (c *Client) ResumeTasks(ctx context.Context, sel TaskSelection) ([]BatchItem, error) {
	return c.bulk(ctx, "/v1/tasks/resume", sel)
}

// DeleteTasks calls delete-tasks (POST /v1/tasks/delete); files also
// deletes the tasks' files.
func (c *Client) DeleteTasks(ctx context.Context, sel TaskSelection, files bool) ([]BatchItem, error) {
	in := struct {
		TaskSelection
		Files bool `json:"files,omitempty"`
	}{TaskSelection: sel, Files: files}
	return c.bulk(ctx, "/v1/tasks/delete", in)
}

// RestartTask calls restart-task (POST /v1/tasks/{id}/restart).
func (c *Client) RestartTask(ctx context.Context, id string) (*Task, error) {
	return c.taskCall(ctx, http.MethodPost, taskPath(id, "/restart"), nil)
}

// DeleteTask calls delete-task (DELETE /v1/tasks/{id}); files also
// deletes the task's files.
func (c *Client) DeleteTask(ctx context.Context, id string, files bool) error {
	path := taskPath(id)
	if files {
		path += "?files=true"
	}
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

// TaskEffectiveSettings calls get-task-effective-settings
// (GET /v1/tasks/{id}/settings/effective).
func (c *Client) TaskEffectiveSettings(ctx context.Context, id string) (*EffectiveSettings, error) {
	var out EffectiveSettings
	if err := c.do(ctx, http.MethodGet, taskPath(id, "/settings/effective"), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

type positionBody struct {
	Position int `json:"position"`
}

func (c *Client) taskCall(ctx context.Context, method, path string, in any) (*Task, error) {
	var out Task
	if err := c.do(ctx, method, path, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) bulk(ctx context.Context, path string, in any) ([]BatchItem, error) {
	var out batchResponse
	if err := c.do(ctx, http.MethodPost, path, in, &out); err != nil {
		return nil, err
	}
	return out.Results, nil
}
