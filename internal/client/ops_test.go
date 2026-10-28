package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// capture records the last request and answers with a canned response.
type capture struct {
	method, uri, contentType string
	body                     any
	status                   int
	response                 string
}

func (c *capture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.method, c.uri, c.contentType = r.Method, r.URL.RequestURI(), r.Header.Get("Content-Type")
	c.body = nil
	if data, _ := io.ReadAll(r.Body); len(data) > 0 {
		if err := json.Unmarshal(data, &c.body); err != nil {
			c.body = "invalid JSON: " + string(data)
		}
	}
	status := c.status
	if status == 0 {
		status = http.StatusOK
	}
	if c.response != "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, c.response)
}

func newCaptureClient(t *testing.T) (*Client, *capture) {
	t.Helper()
	rec := &capture{}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	return c, rec
}

func jsonValue(t *testing.T, s string) any {
	t.Helper()
	if s == "" {
		return nil
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

const (
	taskJSON  = `{"id":"t1","queueId":"main","status":"queued","priority":"normal","position":0,"size":-1,"downloaded":0,"source":{"currentUrl":"http://x/f"},"settings":{"speedLimit":5},"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`
	queueJSON = `{"id":"q1","name":"Q","isDefault":false,"position":1,"state":"running","settings":{},"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`
	batchJSON = `{"results":[{"id":"t1","ok":true,"task":` + taskJSON + `},{"id":"t2","ok":false,"error":{"status":404,"title":"Not Found","detail":"nope"}}]}`
	mergeType = "application/merge-patch+json"
	jsonType  = "application/json"
)

func TestOperationRequests(t *testing.T) {
	ctx := context.Background()
	name := "n"
	labels := map[string]string{"a": "b"}
	for _, tc := range []struct {
		name        string
		call        func(c *Client) error
		response    string
		status      int
		method, uri string
		contentType string
		body        string
	}{
		{"resolve", func(c *Client) error {
			_, err := c.ResolveTask(ctx, ResolveRequest{URL: "http://x/f", QueueID: "q", Settings: json.RawMessage(`{"maxConnections":2}`)})
			return err
		}, `{"resolveId":"r"}`, 0, "POST", "/v1/resolve", jsonType, `{"url":"http://x/f","queueId":"q","settings":{"maxConnections":2}}`},
		{"create", func(c *Client) error {
			_, err := c.CreateTask(ctx, CreateTaskRequest{URL: "http://x/f", Priority: "high", Paused: true, Labels: labels})
			return err
		}, taskJSON, 201, "POST", "/v1/tasks", jsonType, `{"url":"http://x/f","priority":"high","paused":true,"labels":{"a":"b"}}`},
		{"create batch", func(c *Client) error {
			_, err := c.CreateTasks(ctx, []CreateTaskRequest{{URL: "http://x/1"}, {ResolveID: "r"}})
			return err
		}, batchJSON, 0, "POST", "/v1/tasks/batch", jsonType, `{"items":[{"url":"http://x/1"},{"resolveId":"r"}]}`},
		{"list", func(c *Client) error {
			_, err := c.ListTasks(ctx, ListTasksOptions{QueueID: "q", Status: []string{"queued", "paused"}, Search: "a b", Sort: "created", Limit: 10, Cursor: "c"})
			return err
		}, `{"tasks":[]}`, 0, "GET", "/v1/tasks?cursor=c&limit=10&q=a+b&queueId=q&sort=created&status=queued%2Cpaused", "", ""},
		{"get", func(c *Client) error { _, err := c.GetTask(ctx, "a/b"); return err }, taskJSON, 0, "GET", "/v1/tasks/a%2Fb", "", ""},
		{"update", func(c *Client) error {
			_, err := c.UpdateTask(ctx, "t1", UpdateTaskRequest{Name: &name, Labels: &map[string]string{}, SettingsPatch: json.RawMessage(`{"speedLimit":null}`)})
			return err
		}, taskJSON, 0, "PATCH", "/v1/tasks/t1", mergeType, `{"name":"n","labels":{},"settings":{"speedLimit":null}}`},
		{"move", func(c *Client) error { _, err := c.MoveTask(ctx, "t1", "q"); return err }, taskJSON, 0, "POST", "/v1/tasks/t1/move", jsonType, `{"queueId":"q"}`},
		{"reorder", func(c *Client) error { _, err := c.ReorderTask(ctx, "t1", 0); return err }, taskJSON, 0, "POST", "/v1/tasks/t1/reorder", jsonType, `{"position":0}`},
		{"pause ids", func(c *Client) error { _, err := c.PauseTasks(ctx, TaskSelection{IDs: []string{"t1"}}); return err }, batchJSON, 0, "POST", "/v1/tasks/pause", jsonType, `{"ids":["t1"]}`},
		{"resume all", func(c *Client) error { _, err := c.ResumeTasks(ctx, TaskSelection{Filter: &TaskFilter{}}); return err }, batchJSON, 0, "POST", "/v1/tasks/resume", jsonType, `{"filter":{}}`},
		{"delete filter", func(c *Client) error {
			_, err := c.DeleteTasks(ctx, TaskSelection{Filter: &TaskFilter{QueueID: "q", Status: []string{"error"}}}, true)
			return err
		}, batchJSON, 0, "POST", "/v1/tasks/delete", jsonType, `{"filter":{"queueId":"q","status":["error"]},"files":true}`},
		{"restart", func(c *Client) error { _, err := c.RestartTask(ctx, "t1"); return err }, taskJSON, 0, "POST", "/v1/tasks/t1/restart", "", ""},
		{"delete one", func(c *Client) error { return c.DeleteTask(ctx, "t1", true) }, "", 204, "DELETE", "/v1/tasks/t1?files=true", "", ""},
		{"task effective", func(c *Client) error { _, err := c.TaskEffectiveSettings(ctx, "t1"); return err }, `{"settings":{},"sources":{}}`, 0, "GET", "/v1/tasks/t1/settings/effective", "", ""},
		{"list queues", func(c *Client) error { _, err := c.ListQueues(ctx); return err }, `{"queues":[` + queueJSON + `]}`, 0, "GET", "/v1/queues", "", ""},
		{"create queue", func(c *Client) error {
			_, err := c.CreateQueue(ctx, CreateQueueRequest{Name: "Q", State: "stopped", Settings: json.RawMessage(`{"speedLimit":1}`)})
			return err
		}, queueJSON, 201, "POST", "/v1/queues", jsonType, `{"name":"Q","state":"stopped","settings":{"speedLimit":1}}`},
		{"get queue", func(c *Client) error { _, err := c.GetQueue(ctx, "q1"); return err }, queueJSON, 0, "GET", "/v1/queues/q1", "", ""},
		{"update queue", func(c *Client) error {
			_, err := c.UpdateQueue(ctx, "q1", UpdateQueueRequest{Name: &name})
			return err
		}, queueJSON, 0, "PATCH", "/v1/queues/q1", mergeType, `{"name":"n"}`},
		{"replace queue settings", func(c *Client) error {
			_, err := c.ReplaceQueueSettings(ctx, "q1", json.RawMessage(`{"maxConnections":3}`))
			return err
		}, queueJSON, 0, "PUT", "/v1/queues/q1/settings", jsonType, `{"maxConnections":3}`},
		{"delete queue", func(c *Client) error { return c.DeleteQueue(ctx, "q1", "main") }, "", 204, "DELETE", "/v1/queues/q1?moveTasksTo=main", "", ""},
		{"start queue", func(c *Client) error { _, err := c.StartQueue(ctx, "q1"); return err }, queueJSON, 0, "POST", "/v1/queues/q1/start", "", ""},
		{"stop queue", func(c *Client) error { _, err := c.StopQueue(ctx, "q1"); return err }, queueJSON, 0, "POST", "/v1/queues/q1/stop", "", ""},
		{"reorder queue", func(c *Client) error { _, err := c.ReorderQueue(ctx, "q1", 2); return err }, queueJSON, 0, "POST", "/v1/queues/q1/reorder", jsonType, `{"position":2}`},
		{"queue effective", func(c *Client) error { _, err := c.QueueEffectiveSettings(ctx, "q1"); return err }, `{"settings":{},"sources":{}}`, 0, "GET", "/v1/queues/q1/settings/effective", "", ""},
		{"get settings", func(c *Client) error { _, err := c.GetSettings(ctx); return err }, `{}`, 0, "GET", "/v1/settings", "", ""},
		{"replace settings", func(c *Client) error {
			_, err := c.ReplaceSettings(ctx, json.RawMessage(`{"userAgent":"x"}`))
			return err
		}, `{}`, 0, "PUT", "/v1/settings", jsonType, `{"userAgent":"x"}`},
		{"patch settings", func(c *Client) error {
			_, err := c.PatchSettings(ctx, json.RawMessage(`{"retry":{"maxAttempts":null}}`))
			return err
		}, `{}`, 0, "PATCH", "/v1/settings", mergeType, `{"retry":{"maxAttempts":null}}`},
		{"default settings", func(c *Client) error { _, err := c.DefaultSettings(ctx); return err }, `{}`, 0, "GET", "/v1/settings/defaults", "", ""},
		{"settings schema", func(c *Client) error { _, err := c.SettingsSchema(ctx); return err }, `{}`, 0, "GET", "/v1/settings/schema", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := newCaptureClient(t)
			rec.response, rec.status = tc.response, tc.status
			if err := tc.call(c); err != nil {
				t.Fatalf("call: %v", err)
			}
			if rec.method != tc.method || rec.uri != tc.uri {
				t.Errorf("request = %s %s, want %s %s", rec.method, rec.uri, tc.method, tc.uri)
			}
			if rec.contentType != tc.contentType {
				t.Errorf("Content-Type = %q, want %q", rec.contentType, tc.contentType)
			}
			if diff := cmp.Diff(jsonValue(t, tc.body), rec.body); diff != "" {
				t.Errorf("body (-want +got):\n%s", diff)
			}
		})
	}
}

func TestOperationResponses(t *testing.T) {
	ctx := context.Background()
	c, rec := newCaptureClient(t)

	rec.response = taskJSON
	task, err := c.GetTask(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if task.ID != "t1" || task.Size != -1 || task.Progress() != -1 || string(task.Settings) != `{"speedLimit":5}` ||
		task.Source.CurrentURL != "http://x/f" {
		t.Errorf("task = %+v", task)
	}

	rec.response = batchJSON
	items, err := c.PauseTasks(ctx, TaskSelection{IDs: []string{"t1", "t2"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || !items[0].OK || items[0].Task.ID != "t1" || items[1].OK ||
		items[1].Error == nil || items[1].Error.Status != http.StatusNotFound || items[1].Error.Error() != "Not Found: nope" {
		t.Errorf("items = %+v", items)
	}

	rec.response = `{"tasks":[` + taskJSON + `],"nextCursor":"abc"}`
	page, err := c.ListTasks(ctx, ListTasksOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Tasks) != 1 || page.NextCursor != "abc" || rec.uri != "/v1/tasks" {
		t.Errorf("page = %+v, uri %s", page, rec.uri)
	}

	rec.response = `{"$schema":"http://x/schemas/Settings.json","maxConnections":4}`
	layer, err := c.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(layer) != `{"maxConnections":4}` {
		t.Errorf("settings = %s, want the $schema link removed", layer)
	}

	rec.response = `{"settings":{"retry":{"maxAttempts":3}},"sources":{"retry.maxAttempts":"queue"}}`
	eff, err := c.QueueEffectiveSettings(ctx, "q1")
	if err != nil {
		t.Fatal(err)
	}
	if eff.Sources["retry.maxAttempts"] != "queue" {
		t.Errorf("effective = %+v", eff)
	}

	rec.status, rec.response = http.StatusConflict, ""
	if _, err := c.StopQueue(ctx, "q1"); !hasStatus(err, http.StatusConflict) {
		t.Errorf("err = %v, want 409 APIError", err)
	}
}
