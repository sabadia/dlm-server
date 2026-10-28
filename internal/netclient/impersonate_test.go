package netclient

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/settings"
)

// fingerprintServer is a TLS server that only likes Chrome's ClientHello: a
// Go client has no GREASE values among its cipher suites, Chrome has. mode
// "alert" refuses the handshake; "challenge" completes it and answers a
// Cloudflare challenge page.
type fingerprintServer struct {
	*httptest.Server
	goHellos     atomic.Int32 // handshakes without GREASE
	chromeHellos atomic.Int32
	requests     atomic.Int32 // HTTP requests that reached the handler
	greased      sync.Map     // remote addr -> bool
}

func hasGrease(suites []uint16) bool {
	for _, s := range suites {
		if s&0x0f0f == 0x0a0a {
			return true
		}
	}
	return false
}

func newFingerprintServer(t *testing.T, mode string, http2, cf bool) *fingerprintServer {
	t.Helper()
	fs := &fingerprintServer{}
	fs.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fs.requests.Add(1)
		g, _ := fs.greased.Load(r.RemoteAddr)
		if mode == "challenge" && g != true {
			if cf {
				w.Header().Set("Server", "cloudflare")
				w.Header().Set("Cf-Ray", "8f0c-AMS")
				w.Header().Set("Cf-Mitigated", "challenge")
			}
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, "Just a moment...")
			return
		}
		_, _ = io.WriteString(w, "hello over "+r.Proto)
	}))
	fs.TLS = &tls.Config{GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
		g := hasGrease(h.CipherSuites)
		if g {
			fs.chromeHellos.Add(1)
		} else {
			fs.goHellos.Add(1)
		}
		fs.greased.Store(h.Conn.RemoteAddr().String(), g)
		if mode == "alert" && !g {
			return nil, io.ErrUnexpectedEOF // the server aborts the handshake
		}
		return nil, nil
	}}
	fs.EnableHTTP2 = http2
	fs.StartTLS()
	t.Cleanup(fs.Close)
	return fs
}

func trustTransport(t *testing.T, fs *fingerprintServer) *http.Transport {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(fs.Certificate())
	return &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}, DisableCompression: true}
}

func impClient(t *testing.T, primary *http.Transport) *http.Client {
	t.Helper()
	rt := NewImpersonation(ImpersonationOptions{})(TransportConfig{TLSVerify: true}, primary)
	t.Cleanup(func() {
		if c, ok := rt.(interface{ CloseIdleConnections() }); ok {
			c.CloseIdleConnections()
		}
	})
	return &http.Client{Transport: rt, Timeout: 20 * time.Second}
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestChromeFingerprintRetriesAChallengeThenRemembersTheHost(t *testing.T) {
	for _, tc := range []struct {
		name  string
		http2 bool
		want  string
	}{{"http/2", true, "hello over HTTP/2.0"}, {"http/1.1", false, "hello over HTTP/1.1"}} {
		t.Run(tc.name, func(t *testing.T) {
			fs := newFingerprintServer(t, "challenge", tc.http2, true)
			c := impClient(t, trustTransport(t, fs))
			for i := range 3 {
				resp, err := c.Get(fs.URL + "/file") //nolint:bodyclose // closed by body()
				if err != nil {
					t.Fatal(err)
				}
				if got := body(t, resp); got != tc.want || resp.StatusCode != http.StatusOK {
					t.Fatalf("request %d: %d %q, want 200 %q", i, resp.StatusCode, got, tc.want)
				}
			}
			// Only the first request met the challenge as a Go client; the rest
			// went straight to Chrome's fingerprint.
			if fs.goHellos.Load() != 1 {
				t.Errorf("%d Go handshakes, want 1 (the host must be remembered)", fs.goHellos.Load())
			}
			if fs.chromeHellos.Load() < 1 {
				t.Error("no Chrome handshake happened")
			}
		})
	}
}

func TestChromeFingerprintAfterAHandshakeRefusal(t *testing.T) {
	fs := newFingerprintServer(t, "alert", true, false)
	c := impClient(t, trustTransport(t, fs))
	resp, err := c.Get(fs.URL + "/") //nolint:bodyclose // closed by body()
	if err != nil {
		t.Fatalf("the Chrome retry should have worked: %v", err)
	}
	if got := body(t, resp); got != "hello over HTTP/2.0" {
		t.Errorf("body = %q", got)
	}
	again, err := c.Get(fs.URL + "/again") //nolint:bodyclose // closed by body()
	if err != nil {
		t.Fatal(err)
	}
	_ = body(t, again)
	if fs.goHellos.Load() != 1 {
		t.Errorf("%d Go handshakes, want 1", fs.goHellos.Load())
	}
}

func TestOtherAnswersAreNotRetried(t *testing.T) {
	t.Run("a plain 403 is final", func(t *testing.T) {
		fs := newFingerprintServer(t, "challenge", true, false) // 403 without Cloudflare markers
		c := impClient(t, trustTransport(t, fs))
		resp, err := c.Get(fs.URL + "/") //nolint:bodyclose // closed by body()
		if err != nil {
			t.Fatal(err)
		}
		_ = body(t, resp)
		if resp.StatusCode != http.StatusForbidden || fs.chromeHellos.Load() != 0 || fs.requests.Load() != 1 {
			t.Errorf("status %d, %d Chrome handshakes, %d requests: a 403 that is not a challenge must not be retried",
				resp.StatusCode, fs.chromeHellos.Load(), fs.requests.Load())
		}
	})
	t.Run("a server that does not care is left alone", func(t *testing.T) {
		fs := newFingerprintServer(t, "open", true, false)
		c := impClient(t, trustTransport(t, fs))
		for range 3 {
			resp, err := c.Get(fs.URL + "/") //nolint:bodyclose // closed by body()
			if err != nil {
				t.Fatal(err)
			}
			_ = body(t, resp)
		}
		if fs.chromeHellos.Load() != 0 {
			t.Errorf("%d Chrome handshakes for a server that never blocks", fs.chromeHellos.Load())
		}
	})
	t.Run("requests that cannot be repeated are not retried", func(t *testing.T) {
		fs := newFingerprintServer(t, "challenge", true, true)
		c := impClient(t, trustTransport(t, fs))
		resp, err := c.Post(fs.URL+"/", "text/plain", strings.NewReader("data")) //nolint:bodyclose // closed by body()
		if err != nil {
			t.Fatal(err)
		}
		_ = body(t, resp)
		if resp.StatusCode != http.StatusForbidden || fs.chromeHellos.Load() != 0 {
			t.Errorf("POST: status %d with %d Chrome handshakes", resp.StatusCode, fs.chromeHellos.Load())
		}
	})
	t.Run("plain http goes straight through", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "plain") }))
		t.Cleanup(srv.Close)
		c := impClient(t, &http.Transport{})
		resp, err := c.Get(srv.URL) //nolint:bodyclose // closed by body()
		if err != nil || body(t, resp) != "plain" {
			t.Errorf("plain http: %v", err)
		}
	})
}

func TestAChallengeChromeCannotPassEitherIsReturnedAsIs(t *testing.T) {
	// A server that blocks every client, whatever its fingerprint: a real
	// block, not a fingerprint problem. The host must not be remembered.
	var hits atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Cf-Mitigated", "challenge")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, "blocked for everyone")
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	c := impClient(t, &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}})
	for range 2 {
		resp, err := c.Get(srv.URL) //nolint:bodyclose // closed by body()
		if err != nil {
			t.Fatal(err)
		}
		if got := body(t, resp); resp.StatusCode != http.StatusForbidden || got != "blocked for everyone" {
			t.Fatalf("answer = %d %q", resp.StatusCode, got)
		}
	}
	if hits.Load() != 4 { // each request: the Go attempt and the Chrome attempt
		t.Errorf("%d requests, want 4: nothing should be remembered", hits.Load())
	}
}

func TestImpersonationThroughAnHTTPProxy(t *testing.T) {
	fs := newFingerprintServer(t, "challenge", true, true)
	var connects atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
			return
		}
		connects.Add(1)
		dst, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		hj, _ := w.(http.Hijacker)
		src, rw, err := hj.Hijack()
		if err != nil {
			return
		}
		_, _ = rw.WriteString("HTTP/1.1 200 Connection established\r\n\r\n")
		_ = rw.Flush()
		go func() { _, _ = io.Copy(dst, src); _ = dst.Close() }()
		go func() { _, _ = io.Copy(src, dst); _ = src.Close() }()
	}))
	t.Cleanup(proxy.Close)
	primary := trustTransport(t, fs)
	purl, _ := url.Parse(proxy.URL)
	primary.Proxy = http.ProxyURL(purl)
	c := impClient(t, primary)
	resp, err := c.Get(fs.URL + "/") //nolint:bodyclose // closed by body() or refused
	if err != nil {
		t.Fatal(err)
	}
	if got := body(t, resp); got != "hello over HTTP/2.0" {
		t.Errorf("body = %q", got)
	}
	if connects.Load() < 2 { // the Go attempt and the Chrome attempt both tunnel
		t.Errorf("%d CONNECTs: the impersonating attempt must use the proxy too", connects.Load())
	}
}

func TestProxyRefusingConnect(t *testing.T) {
	fs := newFingerprintServer(t, "challenge", true, true)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	t.Cleanup(proxy.Close)
	primary := trustTransport(t, fs)
	purl, _ := url.Parse(proxy.URL)
	primary.Proxy = http.ProxyURL(purl)
	c := impClient(t, primary)
	if _, err := c.Get(fs.URL + "/"); err == nil { //nolint:bodyclose // closed by body() or refused
		t.Error("a proxy that refuses CONNECT must fail the request")
	}
}

func TestFactoryInstallsTheFallbackAndKeepsTLSVerifySetting(t *testing.T) {
	fs := newFingerprintServer(t, "challenge", true, true)
	f := New(Options{Fallback: NewImpersonation(ImpersonationOptions{})})
	t.Cleanup(func() { _ = f.Close() })
	s := settings.Settings{TLSVerify: settings.Some(false), Proxy: settings.Some(settings.Proxy{Mode: settings.ProxyNone})}
	c, err := f.Client(s)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Get(fs.URL + "/") //nolint:bodyclose // closed by body() or refused
	if err != nil {
		t.Fatalf("tlsVerify=false must carry over to the impersonating attempt: %v", err)
	}
	if got := body(t, resp); got != "hello over HTTP/2.0" {
		t.Errorf("body = %q", got)
	}
	// With verification on, the self-signed test certificate is refused by both.
	strict, err := f.Client(settings.Settings{TLSVerify: settings.Some(true), Proxy: settings.Some(settings.Proxy{Mode: settings.ProxyNone})})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := strict.Get(fs.URL + "/"); err == nil { //nolint:bodyclose // closed by body() or refused
		t.Error("an untrusted certificate was accepted")
	}
}

func TestHostCache(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return now }
	c := newHostCache(time.Hour, 2, clock)
	if c.has("a") {
		t.Error("empty cache has a host")
	}
	if !c.add("a") || c.add("a") {
		t.Error("add must report whether the host was new")
	}
	now = now.Add(30 * time.Minute)
	c.add("b")
	now = now.Add(31 * time.Minute) // a expired, b has 29 minutes left
	if c.has("a") || !c.has("b") {
		t.Errorf("expiry: a=%v b=%v", c.has("a"), c.has("b"))
	}
	c.add("c")
	c.add("d") // full: the oldest unexpired entry (b) goes
	if c.has("b") || !c.has("c") || !c.has("d") {
		t.Errorf("bound: b=%v c=%v d=%v", c.has("b"), c.has("c"), c.has("d"))
	}
}

func TestBlockedAndHandshakeDetection(t *testing.T) {
	type answer struct {
		status int
		header http.Header
	}
	mk := func(code int, kv ...string) answer {
		h := http.Header{}
		for i := 0; i+1 < len(kv); i += 2 {
			h.Set(kv[i], kv[i+1])
		}
		return answer{code, h}
	}
	for name, c := range map[string]struct {
		a    answer
		want bool
	}{
		"challenge header":    {mk(403, "Cf-Mitigated", "challenge"), true},
		"cloudflare 503":      {mk(503, "Server", "cloudflare", "Cf-Ray", "1"), true},
		"cloudflare no ray":   {mk(403, "Server", "cloudflare"), false},
		"plain 403":           {mk(403), false},
		"challenge on a 200":  {mk(200, "Cf-Mitigated", "challenge"), false},
		"429 from cloudflare": {mk(429, "Server", "cloudflare", "Cf-Ray", "1"), false},
	} {
		if got := blockedAnswer(c.a.status, c.a.header); got != c.want {
			t.Errorf("blocked(%s) = %v", name, got)
		}
	}
	if blocked(nil) {
		t.Error("no response is not a block")
	}
	if handshakeFailure(io.ErrUnexpectedEOF) {
		t.Error("an unexpected EOF alone is not a handshake failure")
	}
	for msg, want := range map[string]bool{
		"remote error: tls: handshake failure": true, "tls: first record does not look like a TLS handshake": true,
		"read tcp: connection reset by peer": false, "context deadline exceeded": false,
	} {
		if got := handshakeFailure(&net.OpError{Op: "x", Err: errString(msg)}); got != want {
			t.Errorf("handshakeFailure(%q) = %v", msg, got)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }
