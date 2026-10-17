package client

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestRefreshRequests(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name        string
		call        func(c *Client) error
		response    string
		method, uri string
		contentType string
		body        string
	}{
		{
			"refresh", func(c *Client) error { _, err := c.RefreshTask(ctx, "t/1"); return err },
			taskJSON, "POST", "/v1/tasks/t%2F1/refresh", "", "",
		},
		{
			"await", func(c *Client) error { _, err := c.AwaitTaskLink(ctx, "t1"); return err },
			taskJSON, "POST", "/v1/tasks/t1/refresh/await", "", "",
		},
		{
			"update source", func(c *Client) error {
				_, err := c.UpdateTaskSource(ctx, "t1", UpdateTaskSourceRequest{
					CurrentURL: "http://x/new", Referrer: "http://x/", Headers: map[string]string{"A": "b"}, OnMismatch: "restart",
				})
				return err
			}, taskJSON, "PATCH", "/v1/tasks/t1/source", mergeType,
			`{"currentUrl":"http://x/new","referrer":"http://x/","headers":{"A":"b"},"onMismatch":"restart"}`,
		},
		{
			"history", func(c *Client) error { _, err := c.TaskRefreshes(ctx, "t1"); return err },
			`{"attempts":[]}`, "GET", "/v1/tasks/t1/refresh/history", "", "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := newCaptureClient(t)
			rec.response = tc.response
			if err := tc.call(c); err != nil {
				t.Fatalf("call: %v", err)
			}
			if rec.method != tc.method || rec.uri != tc.uri || rec.contentType != tc.contentType {
				t.Errorf("request = %s %s (%s), want %s %s (%s)", rec.method, rec.uri, rec.contentType, tc.method, tc.uri, tc.contentType)
			}
			if diff := cmp.Diff(jsonValue(t, tc.body), rec.body); diff != "" {
				t.Errorf("body (-want +got):\n%s", diff)
			}
		})
	}
}

func TestTaskRefreshesDecodes(t *testing.T) {
	c, rec := newCaptureClient(t)
	rec.response = `{"attempts":[
	  {"id":"a1","taskId":"t1","strategy":"origin-redirect","cause":"expired","outcome":"success","oldUrl":"http://x/old","newUrl":"http://x/new","downloaded":5,"createdAt":"2026-01-01T00:00:00Z"},
	  {"id":"a2","taskId":"t1","strategy":"extension-capture","cause":"expired","outcome":"waiting","downloaded":5,"deadline":"2026-01-01T00:10:00Z","createdAt":"2026-01-01T00:00:01Z"}]}`
	got, err := c.TaskRefreshes(context.Background(), "t1")
	if err != nil || len(got) != 2 || got[0].NewURL != "http://x/new" || got[0].Downloaded != 5 || got[1].Deadline == nil || got[1].Outcome != "waiting" {
		t.Errorf("TaskRefreshes = %+v, %v", got, err)
	}
}
