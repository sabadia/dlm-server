package netclient

import (
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/settings"
)

// baseOf returns the cached RoundTripper behind a client.
func baseOf(t *testing.T, c *http.Client) http.RoundTripper {
	t.Helper()
	ht, ok := c.Transport.(*headerTransport)
	if !ok {
		t.Fatalf("Transport is %T, want *headerTransport", c.Transport)
	}
	return ht.base
}

func TestTransportReuse(t *testing.T) {
	f := New(Options{Getenv: noEnv, SystemProxy: noOSProxy})
	t.Cleanup(func() { _ = f.Close() })

	client := func(s settings.Settings) *http.Client {
		t.Helper()
		c, err := f.Client(s)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	insecure := settings.Settings{TLSVerify: settings.Some(false)}
	custom := proxySettings(settings.ProxyCustom, "http://proxy.example:3128")
	customCreds := proxySettings(settings.ProxyCustom, "http://u:p@proxy.example:3128")
	withHeaders := settings.Settings{
		UserAgent: settings.Some("other"),
		Headers:   settings.Some(map[string]string{"X-A": "1"}),
	}

	base := baseOf(t, client(settings.Settings{}))
	if got := baseOf(t, client(settings.Settings{})); got != base {
		t.Error("same settings must reuse the transport")
	}
	if got := baseOf(t, client(withHeaders)); got != base {
		t.Error("headers/user agent must not affect transport reuse")
	}
	if got := baseOf(t, client(proxySettings(settings.ProxySystem, ""))); got != base {
		t.Error("unset proxy must equal the system default")
	}
	others := map[string]http.RoundTripper{
		"insecure":     baseOf(t, client(insecure)),
		"custom":       baseOf(t, client(custom)),
		"custom+creds": baseOf(t, client(customCreds)),
		"none":         baseOf(t, client(proxySettings(settings.ProxyNone, ""))),
	}
	seen := map[http.RoundTripper]string{base: "default"}
	for name, rt := range others {
		if prev, dup := seen[rt]; dup {
			t.Errorf("%s shares a transport with %s", name, prev)
		}
		seen[rt] = name
	}

	// Concurrent first use of a new config still yields one transport.
	var wg sync.WaitGroup
	got := make([]http.RoundTripper, 16)
	s := proxySettings(settings.ProxyCustom, "http://concurrent.example:1")
	for i := range got {
		wg.Go(func() {
			c, err := f.Client(s)
			if err != nil {
				t.Error(err)
				return
			}
			if ht, ok := c.Transport.(*headerTransport); ok {
				got[i] = ht.base
			}
		})
	}
	wg.Wait()
	for i := range got {
		if got[i] != got[0] {
			t.Fatal("concurrent Client calls returned different transports")
		}
	}
}

func TestTransportCacheEviction(t *testing.T) {
	f := New(Options{MaxTransports: 2, Getenv: noEnv, SystemProxy: noOSProxy})
	t.Cleanup(func() { _ = f.Close() })
	for _, u := range []string{"http://a:1", "http://b:1", "http://a:1", "http://c:1"} {
		if _, err := f.Client(proxySettings(settings.ProxyCustom, u)); err != nil {
			t.Fatal(err)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lru.Len() != 2 || len(f.items) != 2 {
		t.Fatalf("cache size = %d/%d, want 2", f.lru.Len(), len(f.items))
	}
	key := func(u string) TransportConfig {
		return TransportConfig{Proxy: settings.Proxy{Mode: settings.ProxyCustom, URL: u}, TLSVerify: true}
	}
	if _, ok := f.items[key("http://b:1")]; ok {
		t.Error("least recently used transport (b) should be evicted")
	}
	for _, u := range []string{"http://a:1", "http://c:1"} {
		if _, ok := f.items[key(u)]; !ok {
			t.Errorf("%s should still be cached", u)
		}
	}
}

func TestTLSVerify(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Proto)
	}))
	srv.EnableHTTP2 = true
	srv.Config.ErrorLog = quietServerLog()
	srv.StartTLS()
	t.Cleanup(srv.Close)

	log, buf := newTestLogger()
	f := New(Options{Logger: log, Getenv: noEnv, SystemProxy: noOSProxy})
	t.Cleanup(func() { _ = f.Close() })

	secure, err := f.Client(proxySettings(settings.ProxyNone, ""))
	if err != nil {
		t.Fatal(err)
	}
	_, err = get(t, secure, srv.URL, nil)
	var verr *tls.CertificateVerificationError
	if !errors.As(err, &verr) {
		t.Fatalf("self-signed certificate: err = %v, want a verification error", err)
	}

	s := proxySettings(settings.ProxyNone, "")
	s.TLSVerify = settings.Some(false)
	for range 2 {
		insecure, err := f.Client(s)
		if err != nil {
			t.Fatal(err)
		}
		proto, err := get(t, insecure, srv.URL, nil)
		if err != nil {
			t.Fatalf("tlsVerify=false: %v", err)
		}
		if proto != "HTTP/2.0" {
			t.Errorf("protocol = %q, want HTTP/2.0", proto)
		}
	}
	if n := strings.Count(buf.String(), "verification disabled"); n != 1 {
		t.Errorf("insecure warning logged %d times, want once per transport:\n%s", n, buf.String())
	}
}

func TestTLSMinVersion(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.TLS = &tls.Config{MaxVersion: tls.VersionTLS11} //nolint:gosec // the point of the test
	srv.Config.ErrorLog = quietServerLog()
	srv.StartTLS()
	t.Cleanup(srv.Close)

	f := New(Options{Getenv: noEnv, SystemProxy: noOSProxy})
	t.Cleanup(func() { _ = f.Close() })
	s := proxySettings(settings.ProxyNone, "")
	s.TLSVerify = settings.Some(false)
	c, err := f.Client(s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := get(t, c, srv.URL, nil); err == nil {
		t.Fatal("TLS 1.1 server must be rejected")
	}
}

func TestClientDefaults(t *testing.T) {
	var gotEncoding atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEncoding.Store(r.Header.Values("Accept-Encoding"))
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = io.WriteString(w, "raw-bytes")
	}))
	t.Cleanup(srv.Close)

	f := New(Options{Getenv: noEnv, SystemProxy: noOSProxy})
	t.Cleanup(func() { _ = f.Close() })
	c, err := f.Client(proxySettings(settings.ProxyNone, ""))
	if err != nil {
		t.Fatal(err)
	}
	if c.Timeout != 0 {
		t.Errorf("client Timeout = %v, want none (long downloads)", c.Timeout)
	}
	body, err := get(t, c, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := gotEncoding.Load().([]string); len(v) != 0 {
		t.Errorf("Accept-Encoding = %v, want none (compression disabled)", v)
	}
	if body != "raw-bytes" {
		t.Errorf("body = %q: must not be transparently decoded", body)
	}

	tr, ok := baseOf(t, c).(*http.Transport)
	if !ok {
		t.Fatalf("base transport is %T", baseOf(t, c))
	}
	if tr.TLSClientConfig.MinVersion != tls.VersionTLS12 || !tr.Protocols.HTTP2() || !tr.Protocols.HTTP1() {
		t.Errorf("transport TLS/protocols = %v/%v", tr.TLSClientConfig.MinVersion, tr.Protocols)
	}
	if tr.MaxIdleConnsPerHost != DefaultMaxIdleConnsPerHost || tr.ResponseHeaderTimeout != DefaultResponseHeaderTimeout {
		t.Errorf("transport defaults not applied: %+v", tr)
	}
}

func TestCookieJarShared(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "abc", Path: "/"})
			return
		}
		ck, err := r.Cookie("session")
		if err != nil {
			_, _ = io.WriteString(w, "no-cookie")
			return
		}
		_, _ = io.WriteString(w, ck.Value)
	}))
	t.Cleanup(srv.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	f := New(Options{CookieJar: jar, Getenv: noEnv, SystemProxy: noOSProxy})
	t.Cleanup(func() { _ = f.Close() })
	c1, _ := f.Client(proxySettings(settings.ProxyNone, ""))
	if _, err := get(t, c1, srv.URL+"/login", nil); err != nil {
		t.Fatal(err)
	}
	// A new client (e.g. a retry) sees the same jar.
	c2, _ := f.Client(proxySettings(settings.ProxyNone, ""))
	if c2.Jar != jar {
		t.Fatal("client does not use the shared jar")
	}
	if body, err := get(t, c2, srv.URL+"/file", nil); err != nil || body != "abc" {
		t.Errorf("got %q, %v; want cookie value abc", body, err)
	}
}

func TestCloseClosesIdleConnections(t *testing.T) {
	closed := make(chan struct{}, 4)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateClosed {
			closed <- struct{}{}
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)

	f := New(Options{Getenv: noEnv, SystemProxy: noOSProxy})
	c, _ := f.Client(proxySettings(settings.ProxyNone, ""))
	if _, err := get(t, c, srv.URL, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("idle connection not closed by Close")
	}
	f.mu.Lock()
	n := f.lru.Len()
	f.mu.Unlock()
	if n != 0 {
		t.Errorf("cache has %d entries after Close", n)
	}
	// The factory remains usable.
	c, _ = f.Client(proxySettings(settings.ProxyNone, ""))
	if _, err := get(t, c, srv.URL, nil); err != nil {
		t.Errorf("after Close: %v", err)
	}
	_ = f.Close()
}

type fallbackRT struct {
	primary http.RoundTripper
	calls   atomic.Int32
	closed  atomic.Int32
}

func (r *fallbackRT) RoundTrip(req *http.Request) (*http.Response, error) {
	r.calls.Add(1)
	return r.primary.RoundTrip(req)
}

func (r *fallbackRT) CloseIdleConnections() { r.closed.Add(1) }

func TestFallbackExtensionPoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)

	var built []TransportConfig
	var rt *fallbackRT
	f := New(Options{
		Getenv: noEnv, SystemProxy: noOSProxy,
		Fallback: func(cfg TransportConfig, primary *http.Transport) http.RoundTripper {
			built = append(built, cfg)
			rt = &fallbackRT{primary: primary}
			return rt
		},
	})
	for range 3 {
		c, err := f.Client(proxySettings(settings.ProxyNone, ""))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := get(t, c, srv.URL, nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(built) != 1 {
		t.Fatalf("fallback built %d times, want once per cached transport", len(built))
	}
	if built[0].Proxy.Mode != settings.ProxyNone || !built[0].TLSVerify {
		t.Errorf("fallback config = %v", built[0])
	}
	if rt.calls.Load() != 3 {
		t.Errorf("fallback RoundTrip calls = %d, want 3", rt.calls.Load())
	}
	_ = f.Close()
	if rt.closed.Load() != 1 {
		t.Errorf("fallback CloseIdleConnections calls = %d, want 1", rt.closed.Load())
	}
}
