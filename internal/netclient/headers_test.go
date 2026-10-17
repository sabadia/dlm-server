package netclient

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/sabadia/dlm-server/internal/settings"
)

// headerRecorder records the request headers of every request by path.
type headerRecorder struct {
	mu   sync.Mutex
	hdrs map[string]http.Header
	host map[string]string
}

func (h *headerRecorder) record(r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.hdrs == nil {
		h.hdrs, h.host = map[string]http.Header{}, map[string]string{}
	}
	h.hdrs[r.Host+r.URL.Path] = r.Header.Clone()
	h.host[r.Host+r.URL.Path] = r.Host
}

func (h *headerRecorder) get(key string) http.Header {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hdrs[key]
}

func TestDefaultHeaders(t *testing.T) {
	rec := &headerRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { rec.record(r) }))
	t.Cleanup(srv.Close)
	host := serverAddr(srv)

	log, buf := newTestLogger()
	f := New(Options{Logger: log, Getenv: noEnv, SystemProxy: noOSProxy})
	t.Cleanup(func() { _ = f.Close() })

	tests := []struct {
		name     string
		settings settings.Settings
		request  http.Header
		want     map[string]string // header → expected value ("" = absent)
	}{
		{
			name:     "default user agent when unset",
			settings: settings.Settings{},
			want:     map[string]string{"User-Agent": settings.DefaultUserAgent},
		},
		{
			name:     "settings user agent and headers",
			settings: settings.Settings{UserAgent: settings.Some("dlm/1"), Headers: settings.Some(map[string]string{"x-token": "t1", "Referer": "https://ref.example/"})},
			want:     map[string]string{"User-Agent": "dlm/1", "X-Token": "t1", "Referer": "https://ref.example/"},
		},
		{
			name:     "request headers win",
			settings: settings.Settings{UserAgent: settings.Some("dlm/1"), Headers: settings.Some(map[string]string{"X-Token": "settings", "Range": "bytes=0-"})},
			request:  http.Header{"User-Agent": {"probe/2"}, "X-Token": {"request"}, "Range": {"bytes=10-20"}},
			want:     map[string]string{"User-Agent": "probe/2", "X-Token": "request", "Range": "bytes=10-20"},
		},
		{
			name:     "headers entry beats user agent setting",
			settings: settings.Settings{UserAgent: settings.Some("dlm/1"), Headers: settings.Some(map[string]string{"User-Agent": "custom/3"})},
			want:     map[string]string{"User-Agent": "custom/3"},
		},
		{
			name: "forbidden headers are skipped",
			settings: settings.Settings{Headers: settings.Some(map[string]string{
				"Host": "evil.example", "Content-Length": "5", "Connection": "close",
				"Transfer-Encoding": "chunked", "Proxy-Authorization": "Basic x", "Upgrade": "h2c", "X-Ok": "1",
			})},
			want: map[string]string{"X-Ok": "1", "Connection": "", "Transfer-Encoding": "", "Proxy-Authorization": "", "Upgrade": ""},
		},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := f.Client(tt.settings)
			if err != nil {
				t.Fatal(err)
			}
			path := "/" + string(rune('a'+i))
			if _, err := get(t, c, srv.URL+path, tt.request); err != nil {
				t.Fatal(err)
			}
			got := rec.get(host + path)
			if got == nil {
				t.Fatalf("request to %s not recorded", path)
			}
			for k, want := range tt.want {
				if g := got.Get(k); g != want {
					t.Errorf("%s = %q, want %q", k, g, want)
				}
			}
			rec.mu.Lock()
			if h := rec.host[host+path]; h != host {
				t.Errorf("Host = %q, want %q", h, host)
			}
			rec.mu.Unlock()
		})
	}
	if strings.Contains(buf.String(), "evil.example") {
		t.Error("ignored header values must not be logged")
	}
}

func TestHeaderTransportDoesNotMutateRequest(t *testing.T) {
	var got http.Header
	rt := newHeaderTransport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		got = r.Header
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: r}, nil
	}), "ua", map[string]string{"X-A": "1"}, newDiscardLogger())
	req := httptestRequest(t, "http://example.test/")
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(req.Header) != 0 {
		t.Errorf("original request mutated: %v", req.Header)
	}
	if got.Get("X-A") != "1" || got.Get("User-Agent") != "ua" {
		t.Errorf("sent headers = %v", got)
	}
}

func TestRedirectLimit(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "/loop", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	f := New(Options{Getenv: noEnv, SystemProxy: noOSProxy})
	t.Cleanup(func() { _ = f.Close() })
	c, _ := f.Client(proxySettings(settings.ProxyNone, ""))
	_, err := get(t, c, srv.URL+"/start", nil)
	if !errors.Is(err, ErrTooManyRedirects) {
		t.Fatalf("err = %v, want ErrTooManyRedirects", err)
	}
	if n := hits.Load(); n != MaxRedirects+1 {
		t.Errorf("requests = %d, want %d (initial + %d redirects)", n, MaxRedirects+1, MaxRedirects)
	}
}

func TestRedirectHeaderHandling(t *testing.T) {
	rec := &headerRecorder{}
	mux := http.NewServeMux()
	mux.HandleFunc("a.test/start", func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		http.Redirect(w, r, "/same", http.StatusFound)
	})
	mux.HandleFunc("a.test/same", func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		http.Redirect(w, r, "http://cdn.a.test/sub", http.StatusFound)
	})
	mux.HandleFunc("cdn.a.test/sub", func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		http.Redirect(w, r, "http://b.test/other", http.StatusFound)
	})
	mux.HandleFunc("b.test/other", func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		http.Redirect(w, r, "http://a.test/back", http.StatusFound)
	})
	mux.HandleFunc("a.test/back", func(_ http.ResponseWriter, r *http.Request) { rec.record(r) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	addr := serverAddr(srv)

	f := New(Options{
		DialContext: hostDialer(map[string]string{"a.test": addr, "cdn.a.test": addr, "b.test": addr}),
		Getenv:      noEnv, SystemProxy: noOSProxy,
	})
	t.Cleanup(func() { _ = f.Close() })
	s := proxySettings(settings.ProxyNone, "")
	s.Headers = settings.Some(map[string]string{"Authorization": "Bearer settings-token", "X-Settings": "s"})
	c, err := f.Client(s)
	if err != nil {
		t.Fatal(err)
	}
	reqHdr := http.Header{
		"Range":   {"bytes=100-"},
		"Referer": {"https://page.example/"},
		"X-Req":   {"r"},
		"Cookie":  {"explicit=1"},
	}
	if _, err := get(t, c, "http://a.test/start", reqHdr); err != nil {
		t.Fatal(err)
	}

	type view struct{ Range, Referer, XReq, XSettings, Auth, Cookie string }
	viewOf := func(key string) view {
		h := rec.get(key)
		if h == nil {
			t.Fatalf("%s not requested", key)
		}
		return view{h.Get("Range"), h.Get("Referer"), h.Get("X-Req"), h.Get("X-Settings"), h.Get("Authorization"), h.Get("Cookie")}
	}
	full := view{"bytes=100-", "https://page.example/", "r", "s", "Bearer settings-token", "explicit=1"}
	stripped := view{"bytes=100-", "https://page.example/", "r", "s", "", ""}
	want := map[string]view{
		"a.test/start":   full,
		"a.test/same":    full, // same host
		"cdn.a.test/sub": full, // subdomain of the initial host
		"b.test/other":   stripped,
		"a.test/back":    stripped, // stays stripped once the chain left the domain
	}
	for key, w := range want {
		if diff := cmp.Diff(w, viewOf(key)); diff != "" {
			t.Errorf("%s headers (-want +got):\n%s", key, diff)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
