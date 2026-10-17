package client

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
)

const cookieJSON = `{"id":"c1","domain":"example.com","name":"sid","path":"/","secure":true,"httpOnly":true,"hostOnly":true,"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`

func TestCookieRequests(t *testing.T) {
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
			"list", func(c *Client) error { _, err := c.ListCookies(ctx, CookieFilter{}); return err },
			`{"cookies":[` + cookieJSON + `]}`, 0, "GET", "/v1/cookies", "", "",
		},
		{"list filtered", func(c *Client) error {
			_, err := c.ListCookies(ctx, CookieFilter{Domain: "a.example.com", Name: "s id"})
			return err
		}, `{"cookies":[]}`, 0, "GET", "/v1/cookies?domain=a.example.com&name=s+id", "", ""},
		{
			"delete cookies", func(c *Client) error { _, err := c.DeleteCookies(ctx, "example.com"); return err },
			`{"deleted":2}`, 0, "DELETE", "/v1/cookies?domain=example.com", "", "",
		},
		{
			"delete cookie", func(c *Client) error { return c.DeleteCookie(ctx, "a/b") },
			"", 204, "DELETE", "/v1/cookies/a%2Fb", "", "",
		},
		{
			"import", func(c *Client) error { _, err := c.ImportCookies(ctx, "netscape", "data"); return err },
			`{"imported":1,"skipped":0,"errors":[]}`, 0, "POST", "/v1/cookies/import", jsonType, `{"format":"netscape","data":"data"}`,
		},
		{
			"import detect", func(c *Client) error { _, err := c.ImportCookies(ctx, "", "[]"); return err },
			`{"imported":0,"skipped":0,"errors":[]}`, 0, "POST", "/v1/cookies/import", jsonType, `{"data":"[]"}`,
		},
		{"export", func(c *Client) error {
			_, err := c.ExportCookies(ctx, "netscape", CookieFilter{Domain: "example.com"})
			return err
		}, `{"format":"netscape","count":0,"data":""}`, 0, "GET", "/v1/cookies/export?domain=example.com&format=netscape", "", ""},
		{
			"export default", func(c *Client) error { _, err := c.ExportCookies(ctx, "", CookieFilter{}); return err },
			`{"format":"json","count":0,"data":"[]"}`, 0, "GET", "/v1/cookies/export", "", "",
		},
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

func TestCookieResponses(t *testing.T) {
	ctx := context.Background()
	c, rec := newCaptureClient(t)

	rec.response = `{"cookies":[` + cookieJSON + `]}`
	list, err := c.ListCookies(ctx, CookieFilter{})
	if err != nil || len(list) != 1 || list[0].ID != "c1" || list[0].Domain != "example.com" || !list[0].Secure || !list[0].HostOnly {
		t.Errorf("list = %+v, %v", list, err)
	}

	rec.response = `{"deleted":2}`
	if n, err := c.DeleteCookies(ctx, "example.com"); err != nil || n != 2 {
		t.Errorf("DeleteCookies = %d, %v; want 2", n, err)
	}

	rec.response = `{"imported":1,"skipped":2,"errors":["#3: bad"]}`
	res, err := c.ImportCookies(ctx, "", "x")
	if err != nil || res.Imported != 1 || res.Skipped != 2 || len(res.Errors) != 1 {
		t.Errorf("import = %+v, %v", res, err)
	}
}

func TestCaptureDownload(t *testing.T) {
	c, rec := newCaptureClient(t)
	rec.status = 201
	rec.response = `{"task":` + taskJSON + `,"cookies":{"imported":1,"skipped":0,"errors":["#2: bad"]}}`
	res, err := c.CaptureDownload(context.Background(), CaptureRequest{
		URL: "http://x/f", FinalURL: "http://cdn/f", PageURL: "http://x/p", Size: 5, Paused: true,
		Cookies:         []CaptureCookie{{Domain: "x", Name: "a", Value: "b", HostOnly: true}},
		MediaCandidates: []MediaCandidate{{URL: "http://x/m.m3u8", Kind: "hls", Variant: "720p"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.method != "POST" || rec.uri != "/v1/capture" || rec.contentType != jsonType {
		t.Errorf("request = %s %s (%s)", rec.method, rec.uri, rec.contentType)
	}
	want := `{"url":"http://x/f","finalUrl":"http://cdn/f","pageUrl":"http://x/p","size":5,"paused":true,
	  "cookies":[{"domain":"x","name":"a","value":"b","hostOnly":true}],
	  "mediaCandidates":[{"url":"http://x/m.m3u8","kind":"hls","variant":"720p"}]}`
	if diff := cmp.Diff(jsonValue(t, want), rec.body); diff != "" {
		t.Errorf("body (-want +got):\n%s", diff)
	}
	if res.Task.ID != "t1" || res.Cookies.Imported != 1 || len(res.Cookies.Errors) != 1 {
		t.Errorf("result = %+v", res)
	}
}
