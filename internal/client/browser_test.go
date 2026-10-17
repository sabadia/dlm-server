package client

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestBrowserRequests(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name        string
		call        func(c *Client) error
		response    string
		status      int
		method, uri string
		contentType string
		body        string
	}{
		{
			"resolve", func(c *Client) error { _, err := c.ResolvePage(ctx, "https://example.com/p", 7); return err },
			`{"id":"r1","candidates":[]}`, 0, "POST", "/v1/browser/resolve", jsonType, `{"url":"https://example.com/p","waitSeconds":7}`,
		},
		{
			"resolve default wait", func(c *Client) error { _, err := c.ResolvePage(ctx, "https://example.com/p", 0); return err },
			`{"id":"r1","candidates":[]}`, 0, "POST", "/v1/browser/resolve", jsonType, `{"url":"https://example.com/p"}`,
		},
		{"download", func(c *Client) error {
			_, err := c.DownloadCandidate(ctx, "r/1", 3, DownloadCandidateRequest{QueueID: "q", Name: "n.bin", Paused: true})
			return err
		}, taskJSON, 201, "POST", "/v1/browser/results/r%2F1/candidates/3/download", jsonType, `{"queueId":"q","name":"n.bin","paused":true}`},
		{
			"status", func(c *Client) error { _, err := c.BrowserStatus(ctx); return err },
			`{"running":[],"max":2}`, 0, "GET", "/v1/browser", "", "",
		},
		{
			"profiles", func(c *Client) error { _, err := c.BrowserProfiles(ctx); return err },
			`{"profiles":["a.com"]}`, 0, "GET", "/v1/browser/profiles", "", "",
		},
		{
			"delete profile", func(c *Client) error { return c.DeleteBrowserProfile(ctx, "a b") },
			"", 204, "DELETE", "/v1/browser/profiles/a%20b", "", "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := newCaptureClient(t)
			rec.response, rec.status = tc.response, tc.status
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

func TestBrowserResponsesDecode(t *testing.T) {
	c, rec := newCaptureClient(t)
	rec.response = `{"id":"r1","url":"https://e/p","finalUrl":"https://e/p2","title":"T","group":"e","expiresAt":"2026-01-01T00:00:00Z",
	  "candidates":[{"index":0,"url":"https://e/v.mp4","kind":"video","mime":"video/mp4","size":-1,"source":"network","foundAt":"2026-01-01T00:00:00Z","headers":{"Referer":"https://e/p"}}]}`
	got, err := c.ResolvePage(context.Background(), "https://e/p", 0)
	if err != nil || got.ID != "r1" || got.Title != "T" || len(got.Candidates) != 1 || got.Candidates[0].Kind != "video" || got.Candidates[0].Size != -1 ||
		got.Candidates[0].Headers["Referer"] != "https://e/p" {
		t.Errorf("ResolvePage = %+v, %v", got, err)
	}
	rec.response = `{"browser":{"name":"chrome","path":"/x","version":"1"},"running":[{"group":"a","sessions":2}],"max":3}`
	st, err := c.BrowserStatus(context.Background())
	if err != nil || st.Browser == nil || st.Browser.Name != "chrome" || len(st.Running) != 1 || st.Running[0].Sessions != 2 || st.Max != 3 {
		t.Errorf("BrowserStatus = %+v, %v", st, err)
	}
}
