package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sabadia/dlm-server/internal/domain"
)

func TestCaptureDownload(t *testing.T) {
	f := newOpsFixture(t)
	body := `{
	  "url": "` + f.origin.URL + `/file.bin?id=1",
	  "finalUrl": "` + f.fileURL() + `",
	  "pageUrl": "https://example.com/page",
	  "filename": "dir/picked.bin",
	  "mime": "application/octet-stream",
	  "size": 1024,
	  "userAgent": "UA/1",
	  "headers": {"Accept": "*/*", "Cookie": "dropped=1"},
	  "cookies": [
	    {"domain": "127.0.0.1", "name": "sid", "value": "secret-cookie", "hostOnly": true},
	    {"domain": "com", "name": "bad", "value": "x"}
	  ],
	  "queueId": "main", "priority": "high", "paused": true, "labels": {"from": "extension"}
	}`
	var out CaptureOutput
	f.call(t, http.MethodPost, "/v1/capture", body, http.StatusCreated, &out)
	task := out.Task
	if task == nil || task.Name != "picked.bin" || task.Status != domain.StatusPaused || task.Priority != domain.PriorityHigh ||
		task.Labels["from"] != "extension" {
		t.Fatalf("task = %+v", task)
	}
	src := task.Source
	if src.OriginalURL != f.origin.URL+"/file.bin?id=1" || src.CurrentURL != f.fileURL() || src.RedirectURL != f.fileURL() ||
		src.PageURL != "https://example.com/page" || src.Referrer != "https://example.com/page" || src.UserAgent != "UA/1" ||
		src.Headers["Accept"] != "*/*" || src.Headers["Cookie"] != "" ||
		src.Fingerprint.Size != 1024 || src.Fingerprint.ContentType != "application/octet-stream" || src.Fingerprint.Filename != "picked.bin" {
		t.Errorf("source = %+v", src)
	}
	if len(src.CookieDomains) != 2 {
		t.Errorf("cookie domains = %v", src.CookieDomains)
	}
	if out.Cookies.Imported != 1 || len(out.Cookies.Errors) != 1 || !strings.Contains(out.Cookies.Errors[0], "public suffix") {
		t.Errorf("cookies = %+v", out.Cookies)
	}

	// The task and the stored cookie are visible through the other operations.
	var got domain.Task
	f.call(t, http.MethodGet, "/v1/tasks/"+task.ID, "", http.StatusOK, &got)
	var list cookieListBody
	f.call(t, http.MethodGet, "/v1/cookies?domain=127.0.0.1", "", http.StatusOK, &list)
	if got.ID != task.ID || len(list.Cookies) != 1 || list.Cookies[0]["name"] != "sid" {
		t.Errorf("task %s, cookies %v", got.ID, list.Cookies)
	}
}

func TestCaptureFromCandidatesAndErrors(t *testing.T) {
	f := newOpsFixture(t)
	var out CaptureOutput
	f.call(t, http.MethodPost, "/v1/capture", `{"mediaCandidates":[
	  {"url":"`+f.origin.URL+`/a.mp4","kind":"video","size":10},
	  {"url":"`+f.fileURL()+`","kind":"hls","variant":"720p"}]}`, http.StatusCreated, &out)
	if out.Task.Source.CurrentURL != f.fileURL() || out.Task.Source.MediaHint != "720p" {
		t.Errorf("source = %+v, want the hls candidate", out.Task.Source)
	}

	for name, tc := range map[string]struct {
		body   string
		status int
	}{
		"nothing":       {`{}`, http.StatusUnprocessableEntity},
		"relative url":  {`{"url":"/x"}`, http.StatusUnprocessableEntity},
		"unknown queue": {`{"url":"` + f.fileURL() + `","queueId":"nope"}`, http.StatusNotFound},
		"bad kind":      {`{"mediaCandidates":[{"url":"http://x/y","kind":"gif"}]}`, http.StatusUnprocessableEntity},
		"bad cookie":    {`{"url":"http://x/y","cookies":[{"domain":"","name":"a","value":"b"}]}`, http.StatusUnprocessableEntity},
		"bad priority":  {`{"url":"http://x/y","priority":"urgent"}`, http.StatusUnprocessableEntity},
		"bad same site": {`{"url":"http://x/y","cookies":[{"domain":"x.test","name":"a","value":"b","sameSite":"odd"}]}`, http.StatusUnprocessableEntity},
		"bad setting":   {`{"url":"` + f.fileURL() + `","settings":{"maxConnections":-1}}`, http.StatusUnprocessableEntity},
		"bad dir":       {`{"url":"` + f.fileURL() + `","dir":"relative"}`, http.StatusUnprocessableEntity},
	} {
		t.Run(name, func(t *testing.T) {
			assertProblem(t, do(t, f.s, request{method: http.MethodPost, path: "/v1/capture", body: tc.body, headers: bearer(testToken)}), tc.status)
		})
	}
	assertProblem(t, do(t, f.s, request{method: http.MethodPost, path: "/v1/capture", body: `{"url":"http://x/y"}`}), http.StatusUnauthorized)
}

// TestCaptureChecksTheExtensionOrigin: the extension calls capture from its
// own origin, which must be in the allowed list; other web origins are refused.
func TestCaptureChecksTheExtensionOrigin(t *testing.T) {
	f := newOpsFixture(t)
	for _, tc := range []struct {
		name, origin string
		want         int
	}{
		{"listed extension", testOrigin, http.StatusCreated},
		{"other extension", "chrome-extension://zzzzzzzzzzzzzzzz", http.StatusForbidden},
		{"web page", "https://evil.example", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := bearer(testToken)
			headers["Origin"] = tc.origin
			headers["Sec-Fetch-Site"] = "cross-site"
			w := do(t, f.s, request{method: http.MethodPost, path: "/v1/capture", body: `{"url":"` + f.fileURL() + `","paused":true}`, headers: headers})
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d; body: %s", w.Code, tc.want, w.Body)
			}
			if tc.want == http.StatusForbidden {
				assertProblem(t, w, http.StatusForbidden)
			}
		})
	}
}

func TestCaptureBodyMapsTheExtensionFields(t *testing.T) {
	b := CaptureBody{
		URL: "https://example.com/a", Name: "a.bin", ResolverKind: "ytdlp", Mirrors: []string{"https://m.example.net/a"},
		Method: "POST", Body: []byte("x=1"),
		Checksum: &CaptureChecksum{Algorithm: "sha256", Expected: "abc"},
		Cookies:  []CaptureCookie{{Domain: "example.com", Name: "s", Value: "v", PartitionKey: "https://example.com"}},
	}
	in := b.toInput(false)
	if in.Name != "a.bin" || in.ResolverKind != "ytdlp" || len(in.Mirrors) != 1 || in.Method != http.MethodPost || string(in.Body) != "x=1" ||
		in.Checksum == nil || in.Checksum.Expected != "abc" || in.Cookies[0].PartitionKey != "https://example.com" {
		t.Errorf("input = %+v", in)
	}

	// The default follows who is asking; the request can choose.
	for _, tc := range []struct {
		scope     string
		extension bool
		want      bool
	}{
		{"", false, false},
		{"", true, true},
		{"task", false, true},
		{"store", true, false},
	} {
		b.CookieScope = tc.scope
		if got := b.toInput(tc.extension).TaskCookies; got != tc.want {
			t.Errorf("scope %q extension=%v: TaskCookies = %v, want %v", tc.scope, tc.extension, got, tc.want)
		}
	}
}
