package httpx

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/settings"
)

const payload = "0123456789abcdefghijklmnopqrstuvwxyz"

func probe(t *testing.T, srv *httptest.Server, src domain.Source, s settings.Settings) (*ProbeResult, error) {
	t.Helper()
	if src.CurrentURL == "" {
		src.CurrentURL = srv.URL + "/files/data.bin"
	}
	res, err := Probe(context.Background(), srv.Client(), src, s)
	if res != nil {
		t.Cleanup(func() { _ = res.Close() })
	}
	return res, err
}

func readBody(t *testing.T, res *ProbeResult) string {
	t.Helper()
	b, err := io.ReadAll(res.Response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestProbeStatusAndRange(t *testing.T) {
	n := strconv.Itoa(len(payload))
	tests := []struct {
		name      string
		handler   http.HandlerFunc
		wantSize  int64
		wantRange bool
		wantBody  string
	}{
		{
			name: "206 with content-range",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Range") != "bytes=0-" {
					t.Errorf("Range = %q", r.Header.Get("Range"))
				}
				w.Header().Set("Content-Range", "bytes 0-"+strconv.Itoa(len(payload)-1)+"/"+n)
				w.WriteHeader(http.StatusPartialContent)
				_, _ = io.WriteString(w, payload)
			},
			wantSize: int64(len(payload)), wantRange: true, wantBody: payload,
		},
		{
			name: "206 with unknown total",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Range", "bytes 0-9/*")
				w.WriteHeader(http.StatusPartialContent)
				_, _ = io.WriteString(w, payload[:10])
			},
			wantSize: -1, wantRange: true, wantBody: payload[:10],
		},
		{
			name: "206 partial chunk",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Range", "bytes 0-9/"+n)
				w.WriteHeader(http.StatusPartialContent)
				_, _ = io.WriteString(w, payload[:10])
			},
			wantSize: int64(len(payload)), wantRange: true, wantBody: payload[:10],
		},
		{
			name: "206 non-zero start is malformed, re-requested without range",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Range") == "" { // retry without Range gets the whole file
					w.Header().Set("Content-Length", n)
					_, _ = io.WriteString(w, payload)
					return
				}
				w.Header().Set("Content-Range", "bytes 5-9/"+n)
				w.WriteHeader(http.StatusPartialContent)
				_, _ = io.WriteString(w, payload[5:10])
			},
			wantSize: int64(len(payload)), wantRange: false, wantBody: payload,
		},
		{
			name: "206 end beyond total is malformed, re-requested without range",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Range") == "" { // retry without Range gets the whole file
					w.Header().Set("Content-Length", n)
					_, _ = io.WriteString(w, payload)
					return
				}
				w.Header().Set("Content-Range", "bytes 0-99/10")
				w.WriteHeader(http.StatusPartialContent)
			},
			wantSize: int64(len(payload)), wantRange: false, wantBody: payload,
		},
		{
			name: "206 garbage content-range, re-requested without range",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Range") == "" { // retry without Range gets the whole file
					w.Header().Set("Content-Length", n)
					_, _ = io.WriteString(w, payload)
					return
				}
				w.Header().Set("Content-Range", "items 0-1/2")
				w.WriteHeader(http.StatusPartialContent)
			},
			wantSize: int64(len(payload)), wantRange: false, wantBody: payload,
		},
		{
			name: "200 ignoring range",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Length", n)
				_, _ = io.WriteString(w, payload)
			},
			wantSize: int64(len(payload)), wantRange: false, wantBody: payload,
		},
		{
			name: "200 advertising accept-ranges",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Accept-Ranges", "bytes")
				w.Header().Set("Content-Length", n)
				_, _ = io.WriteString(w, payload)
			},
			wantSize: int64(len(payload)), wantRange: true, wantBody: payload,
		},
		{
			name: "200 chunked unknown size",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Accept-Ranges", "bytes")
				_, _ = io.WriteString(w, payload[:5])
				if f, ok := w.(http.Flusher); ok {
					f.Flush() // forces chunked encoding
				}
				_, _ = io.WriteString(w, payload[5:])
			},
			wantSize: -1, wantRange: false, wantBody: payload,
		},
		{
			name: "compressed despite identity",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Accept-Encoding") != "identity" {
					t.Errorf("Accept-Encoding = %q", r.Header.Get("Accept-Encoding"))
				}
				w.Header().Set("Content-Encoding", "gzip")
				w.Header().Set("Content-Range", "bytes 0-2/3")
				w.WriteHeader(http.StatusPartialContent)
				_, _ = io.WriteString(w, "xyz")
			},
			wantSize: -1, wantRange: false, wantBody: "xyz",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()
			res, err := probe(t, srv, domain.Source{}, settings.Settings{})
			if err != nil {
				t.Fatal(err)
			}
			if res.Resource.Size != tt.wantSize || res.Resource.RangeSupported != tt.wantRange {
				t.Errorf("size=%d range=%v, want size=%d range=%v",
					res.Resource.Size, res.Resource.RangeSupported, tt.wantSize, tt.wantRange)
			}
			if res.Source.Fingerprint.Size != tt.wantSize {
				t.Errorf("fingerprint size = %d", res.Source.Fingerprint.Size)
			}
			if got := readBody(t, res); got != tt.wantBody {
				t.Errorf("body = %q, want %q", got, tt.wantBody)
			}
		})
	}
}

func TestProbeEmptyFile(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Accept-Ranges", "bytes")
		if r.Header.Get("Range") != "" {
			w.Header().Set("Content-Range", "bytes */0")
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			_, _ = io.WriteString(w, "range not satisfiable")
			return
		}
		w.Header().Set("Content-Length", "0")
	}))
	defer srv.Close()
	res, err := probe(t, srv, domain.Source{}, settings.Settings{})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want a single retry without Range", calls.Load())
	}
	if res.Resource.Size != 0 || res.Resource.RangeSupported {
		t.Errorf("size=%d range=%v", res.Resource.Size, res.Resource.RangeSupported)
	}
	if got := readBody(t, res); got != "" {
		t.Errorf("body = %q", got)
	}
}

func TestProbeRequestConstruction(t *testing.T) {
	type seen struct {
		method, body, ua, referer, token, custom, captured, rng, enc string
	}
	got := make(chan seen, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- seen{
			method: r.Method, body: string(b), ua: r.UserAgent(), referer: r.Referer(),
			token: r.Header.Get("X-Token"), custom: r.Header.Get("X-Settings"),
			captured: r.Header.Get("X-Captured"), rng: r.Header.Get("Range"),
			enc: r.Header.Get("Accept-Encoding"),
		}
	}))
	defer srv.Close()

	tests := []struct {
		name string
		src  domain.Source
		set  settings.Settings
		want seen
	}{
		{
			name: "defaults",
			src:  domain.Source{},
			set:  settings.Settings{UserAgent: settings.Some("settings-ua")},
			want: seen{method: http.MethodGet, ua: "settings-ua", rng: "bytes=0-", enc: "identity"},
		},
		{
			name: "source fields and settings headers precedence",
			src: domain.Source{
				Method: http.MethodPost, Body: []byte("a=1"), UserAgent: "source-ua",
				Referrer: "https://ref.example/", PageURL: "https://page.example/",
				Headers: map[string]string{
					"x-token": "captured", "X-Captured": "yes", "User-Agent": "header-ua",
					"Range": "bytes=5-", "Accept-Encoding": "gzip",
				},
			},
			set: settings.Settings{
				UserAgent: settings.Some("settings-ua"),
				Headers:   settings.Some(map[string]string{"X-Token": "explicit", "X-Settings": "1"}),
			},
			want: seen{
				method: http.MethodPost, body: "a=1", ua: "source-ua", referer: "https://ref.example/",
				token: "explicit", custom: "1", captured: "yes", rng: "bytes=0-", enc: "identity",
			},
		},
		{
			name: "page url as referer",
			src:  domain.Source{PageURL: "https://page.example/"},
			want: seen{method: http.MethodGet, referer: "https://page.example/", ua: "Go-http-client/1.1", rng: "bytes=0-", enc: "identity"},
		},
		{
			name: "header referer kept without source referrer",
			src:  domain.Source{Headers: map[string]string{"Referer": "https://hdr.example/"}},
			want: seen{method: http.MethodGet, referer: "https://hdr.example/", ua: "Go-http-client/1.1", rng: "bytes=0-", enc: "identity"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := probe(t, srv, tt.src, tt.set); err != nil {
				t.Fatal(err)
			}
			if g := <-got; g != tt.want {
				t.Errorf("request = %+v\nwant      %+v", g, tt.want)
			}
		})
	}
}

func TestProbeRedirectRecorded(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/cdn/real%20name.zip?sig=abc", http.StatusFound)
	})
	mux.HandleFunc("/cdn/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, payload)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	res, err := probe(t, srv, domain.Source{CurrentURL: srv.URL + "/start", RedirectURL: "https://stale.example/"}, settings.Settings{})
	if err != nil {
		t.Fatal(err)
	}
	if want := srv.URL + "/cdn/real%20name.zip?sig=abc"; res.Source.RedirectURL != want {
		t.Errorf("RedirectURL = %q, want %q", res.Source.RedirectURL, want)
	}
	if res.Source.CurrentURL != srv.URL+"/start" {
		t.Errorf("CurrentURL changed: %q", res.Source.CurrentURL)
	}
	if res.Filename != "real name.zip" {
		t.Errorf("Filename = %q", res.Filename)
	}

	// No redirect: a stale RedirectURL is cleared.
	res2, err := probe(t, srv, domain.Source{CurrentURL: srv.URL + "/cdn/x.zip", RedirectURL: "https://stale.example/"}, settings.Settings{})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Source.RedirectURL != "" {
		t.Errorf("RedirectURL = %q, want empty", res2.Source.RedirectURL)
	}
}

type trackingBody struct {
	io.ReadCloser
	closed *atomic.Bool
}

func (b trackingBody) Close() error { b.closed.Store(true); return b.ReadCloser.Close() }

type trackingTransport struct {
	base   http.RoundTripper
	closed *atomic.Bool
}

func (tr trackingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := tr.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	resp.Body = trackingBody{resp.Body, tr.closed}
	return resp, nil
}

func TestProbeNon2xx(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusForbidden, http.StatusServiceUnavailable} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(code)
				_, _ = io.WriteString(w, "nope")
			}))
			defer srv.Close()
			var closed atomic.Bool
			client := &http.Client{Transport: trackingTransport{srv.Client().Transport, &closed}}
			res, err := Probe(context.Background(), client, domain.Source{CurrentURL: srv.URL + "/f"}, settings.Settings{})
			if res != nil {
				t.Fatal("result must be nil on error")
			}
			var se *StatusError
			if !errors.As(err, &se) {
				t.Fatalf("err = %v, want *StatusError", err)
			}
			if se.StatusCode != code || se.Header.Get("Retry-After") != "7" || se.URL != srv.URL+"/f" {
				t.Errorf("StatusError = %+v", se)
			}
			if !closed.Load() {
				t.Error("error response body not closed")
			}
		})
	}
}

func TestProbeErrors(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close() // connection refused
	if _, err := Probe(context.Background(), srv.Client(), domain.Source{CurrentURL: srv.URL}, settings.Settings{}); err == nil {
		t.Error("expected network error")
	} else if !strings.HasPrefix(err.Error(), "httpx: probe") {
		t.Errorf("err = %v", err)
	}
	if _, err := Probe(context.Background(), http.DefaultClient, domain.Source{}, settings.Settings{}); err == nil {
		t.Error("expected error for empty URL")
	}
	if _, err := Probe(context.Background(), nil, domain.Source{CurrentURL: "http://x"}, settings.Settings{}); err == nil {
		t.Error("expected error for nil client")
	}
	if _, err := Probe(context.Background(), http.DefaultClient, domain.Source{CurrentURL: "http://x", Method: "BAD METHOD"}, settings.Settings{}); err == nil {
		t.Error("expected error for invalid method")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	live := httptest.NewServer(http.NotFoundHandler())
	defer live.Close()
	if _, err := Probe(ctx, live.Client(), domain.Source{CurrentURL: live.URL}, settings.Settings{}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled ctx: err = %v", err)
	}
}

func TestProbeFingerprint(t *testing.T) {
	lm := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	tests := []struct {
		name, etag, lastMod string
		wantETag            string
		wantLM              *time.Time
	}{
		{"strong etag", `"abc123"`, lm.Format(http.TimeFormat), `"abc123"`, &lm},
		{"weak etag ignored", `W/"abc123"`, "", "", nil},
		{"malformed etag ignored", `abc123`, "", "", nil},
		{"rfc850 last-modified", "", "Wednesday, 04-Mar-26 05:06:07 GMT", "", &lm},
		{"bad last-modified ignored", "", "yesterday", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.etag != "" {
					w.Header().Set("ETag", tt.etag)
				}
				if tt.lastMod != "" {
					w.Header().Set("Last-Modified", tt.lastMod)
				}
				w.Header().Set("Content-Type", "Application/PDF; charset=binary")
				w.Header().Set("Content-Disposition", `attachment; filename="doc"`)
				w.Header().Set("Content-Range", "bytes 0-2/3")
				w.WriteHeader(http.StatusPartialContent)
				_, _ = io.WriteString(w, "pdf")
			}))
			defer srv.Close()
			res, err := probe(t, srv, domain.Source{}, settings.Settings{})
			if err != nil {
				t.Fatal(err)
			}
			fp := res.Source.Fingerprint
			if fp.ETag != tt.wantETag {
				t.Errorf("ETag = %q, want %q", fp.ETag, tt.wantETag)
			}
			switch {
			case tt.wantLM == nil && fp.LastModified != nil:
				t.Errorf("LastModified = %v, want nil", fp.LastModified)
			case tt.wantLM != nil && (fp.LastModified == nil || !fp.LastModified.Equal(*tt.wantLM) || fp.LastModified.Location() != time.UTC):
				t.Errorf("LastModified = %v, want %v UTC", fp.LastModified, tt.wantLM)
			}
			if fp.Size != 3 || fp.Filename != "doc.pdf" || fp.ContentType != "application/pdf" {
				t.Errorf("fingerprint = %+v", fp)
			}
			r := res.Resource
			if r.Name != "doc.pdf" || len(r.Files) != 1 || r.Files[0] != (domain.File{Index: 0, Name: "doc.pdf", Size: 3, Selected: true}) {
				t.Errorf("resource = %+v", r)
			}
			if res.Filename != "doc.pdf" {
				t.Errorf("Filename = %q", res.Filename)
			}
		})
	}
}

func TestProbeBodyReadableAfterProbe(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) > 1 {
			http.Error(w, "one-time URL already used", http.StatusGone)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = io.WriteString(w, payload)
	}))
	defer srv.Close()
	res, err := probe(t, srv, domain.Source{}, settings.Settings{})
	if err != nil {
		t.Fatal(err)
	}
	if got := readBody(t, res); got != payload {
		t.Errorf("body = %q", got)
	}
	if calls.Load() != 1 {
		t.Errorf("server hit %d times", calls.Load())
	}
	if err := res.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := res.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	var nilRes *ProbeResult
	if err := nilRes.Close(); err != nil {
		t.Error(err)
	}
}
