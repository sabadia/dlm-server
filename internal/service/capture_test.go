package service

import (
	"bytes"
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/sabadia/dlm-server/internal/cookies"
	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/storage"
)

func newCaptureFixture(t *testing.T) (*CaptureService, *cookies.Store, *svcFixture) {
	t.Helper()
	f := newSvcFixture(t)
	cipher, err := cookies.NewCipher(bytes.Repeat([]byte{5}, 32))
	if err != nil {
		t.Fatal(err)
	}
	jar := cookies.NewStore(f.store.Cookies(), cipher, cookies.Options{})
	return NewCaptureService(f.tasks, jar, nil), jar, f
}

func TestCaptureBuildsFullSource(t *testing.T) {
	svc, jar, f := newCaptureFixture(t)
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
	res, err := svc.Capture(t.Context(), CaptureInput{
		URL:       "https://example.com/dl?id=7",
		FinalURL:  "https://cdn.example.net/signed/f.zip?sig=abc",
		PageURL:   "https://example.com/files",
		Filename:  `C:\Users\me\f.zip`,
		MIME:      "application/zip",
		Size:      1234,
		UserAgent: "Mozilla/5.0 test",
		Headers:   map[string]string{"Accept-Language": "en", "Cookie": "stale=1", "host": "x", "Authorization": "Bearer t"},
		Cookies: []*domain.Cookie{
			{Domain: ".example.com", Name: "sid", Value: "s", Path: "/", Secure: true, ExpiresAt: &exp},
			{Domain: "cdn.example.net", Name: "tok", Value: "t", Path: "/", HostOnly: true},
			{Domain: "example.com", Name: "dup", Value: "d", Path: "/"},
		},
		Priority: domain.PriorityHigh,
		Paused:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Cookies.Imported != 3 || len(res.Cookies.Errors) != 0 {
		t.Errorf("cookie import = %+v", res.Cookies)
	}
	task := f.get(t, res.Task.ID)
	want := domain.Source{
		CurrentURL:    "https://cdn.example.net/signed/f.zip?sig=abc",
		OriginalURL:   "https://example.com/dl?id=7",
		RedirectURL:   "https://cdn.example.net/signed/f.zip?sig=abc",
		PageURL:       "https://example.com/files",
		Referrer:      "https://example.com/files", // defaults to the page
		Method:        "GET",
		Headers:       map[string]string{"Accept-Language": "en", "Authorization": "Bearer t"},
		UserAgent:     "Mozilla/5.0 test",
		CookieDomains: []string{"example.com", "cdn.example.net"},
		ResolverKind:  domain.ResolverDirect,
		Fingerprint:   domain.Fingerprint{Size: 1234, Filename: "f.zip", ContentType: "application/zip"},
	}
	if diff := cmp.Diff(want, task.Source); diff != "" {
		t.Errorf("source (-want +got):\n%s", diff)
	}
	if task.Name != "f.zip" || task.Priority != domain.PriorityHigh || task.Status != domain.StatusPaused {
		t.Errorf("task = %+v", task)
	}
	// The cookies are in the store (and reach the jar) before the task exists.
	got, err := jar.ForURL(context.Background(), mustParse(t, "https://www.example.com/x"))
	if err != nil || len(got) != 2 {
		t.Errorf("jar cookies for example.com = %d, %v; want sid and dup", len(got), err)
	}
}

func TestCaptureWithoutCookiesUsesTheURLHost(t *testing.T) {
	svc, _, _ := newCaptureFixture(t)
	res, err := svc.Capture(t.Context(), CaptureInput{URL: "http://Files.Example.com:8080/a.bin", Referrer: "http://r.example/"})
	if err != nil {
		t.Fatal(err)
	}
	src := res.Task.Source
	if src.CurrentURL != src.OriginalURL || src.RedirectURL != "" || src.Referrer != "http://r.example/" ||
		!cmp.Equal(src.CookieDomains, []string{"files.example.com"}) {
		t.Errorf("source = %+v", src)
	}
	if res.Cookies.Imported != 0 || len(res.Cookies.Errors) != 0 {
		t.Errorf("cookie import = %+v", res.Cookies)
	}
}

func TestCaptureFromMediaCandidates(t *testing.T) {
	svc, _, _ := newCaptureFixture(t)
	candidates := []MediaCandidate{
		{URL: "https://v.example/clip.mp4", Kind: MediaVideo, MIME: "video/mp4", Size: 500},
		{URL: "https://v.example/master.m3u8", Kind: MediaHLS, MIME: "application/vnd.apple.mpegurl", Variant: "1080p"},
		{URL: "https://v.example/audio.m4a", Kind: MediaAudio},
	}
	res, err := svc.Capture(t.Context(), CaptureInput{PageURL: "https://v.example/watch", MediaCandidates: candidates})
	if err != nil {
		t.Fatal(err)
	}
	src := res.Task.Source
	if src.CurrentURL != "https://v.example/master.m3u8" || src.MediaHint != "1080p" ||
		src.Fingerprint.ContentType != "application/vnd.apple.mpegurl" || src.Referrer != "https://v.example/watch" {
		t.Errorf("source = %+v, want the HLS candidate", src)
	}

	// A clicked URL that is a candidate supplies the variant, mime and size it lacks.
	res, err = svc.Capture(t.Context(), CaptureInput{URL: "https://v.example/clip.mp4", MediaCandidates: candidates})
	if err != nil {
		t.Fatal(err)
	}
	if fp := res.Task.Source.Fingerprint; fp.Size != 500 || fp.ContentType != "video/mp4" {
		t.Errorf("fingerprint = %+v, want size and mime of the matching candidate", fp)
	}
}

func TestPickCandidate(t *testing.T) {
	tests := []struct {
		name string
		in   []MediaCandidate
		want string
	}{
		{"kind order", []MediaCandidate{{URL: "a", Kind: MediaVideo}, {URL: "b", Kind: MediaDASH}, {URL: "c", Kind: MediaHLS}}, "c"},
		{"larger first", []MediaCandidate{{URL: "a", Kind: MediaVideo, Size: 1}, {URL: "b", Kind: MediaVideo, Size: 9}, {URL: "c", Kind: MediaVideo}}, "b"},
		{"first on a tie", []MediaCandidate{{URL: "a", Kind: MediaAudio}, {URL: "b", Kind: MediaAudio}}, "a"},
		{"unset kind is a file", []MediaCandidate{{URL: "a"}, {URL: "b", Kind: MediaAudio}}, "b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := pickCandidate(tt.in)
			if !ok || got.URL != tt.want {
				t.Errorf("pickCandidate = %q, %v; want %q", got.URL, ok, tt.want)
			}
		})
	}
	if _, ok := pickCandidate(nil); ok {
		t.Error("pickCandidate(nil) picked something")
	}
}

func TestCaptureRejectsBadInputBeforeStoringCookies(t *testing.T) {
	cookie := []*domain.Cookie{{Domain: "example.com", Name: "sid", Value: "s"}}
	for name, in := range map[string]CaptureInput{
		"nothing":            {},
		"relative url":       {URL: "/just/a/path"},
		"no host":            {URL: "https:///x"},
		"bad final url":      {URL: "https://example.com/a", FinalURL: "nope"},
		"candidate no url":   {MediaCandidates: []MediaCandidate{{Kind: MediaHLS}}},
		"candidate bad kind": {URL: "https://example.com/a", MediaCandidates: []MediaCandidate{{URL: "https://x/y", Kind: "gif"}}},
		"unknown queue":      {URL: "https://example.com/a", QueueID: "nope", Cookies: cookie},
		"bad name":           {URL: "https://example.com/a", Filename: "..", Cookies: cookie},
	} {
		t.Run(name, func(t *testing.T) {
			svc, jar, f := newCaptureFixture(t)
			in.Cookies = cookie
			_, err := svc.Capture(t.Context(), in)
			if err == nil {
				t.Fatal("want an error")
			}
			if name == "unknown queue" {
				wantErr(t, err, domain.ErrNotFound)
			} else {
				wantErr(t, err, domain.ErrInvalidArgument)
			}
			if page, err := f.tasks.List(t.Context(), storage.TaskFilter{}); err != nil || len(page.Tasks) != 0 {
				t.Errorf("tasks created by a rejected capture: %d, %v", len(page.Tasks), err)
			}
			// Input errors are caught before any cookie is stored; only
			// failures of task creation itself keep the cookies.
			stored, err := jar.ListMetadata(context.Background(), storage.CookieFilter{})
			if err != nil {
				t.Fatal(err)
			}
			if keeps := name == "unknown queue" || name == "bad name"; keeps != (len(stored) == 1) {
				t.Errorf("stored cookies = %d for %q", len(stored), name)
			}
		})
	}
}

func TestCaptureReportsRejectedCookies(t *testing.T) {
	svc, _, _ := newCaptureFixture(t)
	res, err := svc.Capture(t.Context(), CaptureInput{
		URL: "https://example.com/f.bin",
		Cookies: []*domain.Cookie{
			{Domain: "example.com", Name: "ok", Value: "1"},
			{Domain: "com", Name: "tld", Value: "1"}, // public suffix as a domain cookie
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Cookies.Imported != 1 || len(res.Cookies.Errors) != 1 {
		t.Errorf("cookie import = %+v, want 1 imported and 1 error", res.Cookies)
	}
}

func TestCaptureNameAndHeaders(t *testing.T) {
	for in, want := range map[string]string{"a.zip": "a.zip", "dir/a.zip": "a.zip", "  b.zip": "b.zip", `x\y\z.bin`: "z.bin", "": "", "/": ""} {
		if got := captureName(in); got != want {
			t.Errorf("captureName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := captureHeaders(map[string]string{"Cookie": "a", "HOST": "b"}); got != nil {
		t.Errorf("captureHeaders = %v, want nil when only dropped headers are present", got)
	}
}

func mustParse(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
