package netclient

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/sabadia/dlm-server/internal/settings"
)

// newOrigin starts a server answering "direct" and returns it together with
// a dialer that routes origin.test and api.internal and "intranet" to it.
func newOrigin(t *testing.T) (*httptest.Server, func(ctx context.Context, network, addr string) (net.Conn, error)) {
	t.Helper()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "direct")
	}))
	t.Cleanup(origin.Close)
	a := serverAddr(origin)
	return origin, hostDialer(map[string]string{"origin.test": a, "api.internal": a, "intranet": a})
}

func TestCustomHTTPProxy(t *testing.T) {
	p := newForwardProxy(t)
	f := New(Options{Getenv: noEnv, SystemProxy: noOSProxy})
	t.Cleanup(func() { _ = f.Close() })

	proxyURL := strings.Replace(p.URL, "http://", "http://alice:s3cret@", 1)
	c, err := f.Client(proxySettings(settings.ProxyCustom, proxyURL))
	if err != nil {
		t.Fatal(err)
	}
	body, err := get(t, c, "http://origin.test/file.bin", nil)
	if err != nil {
		t.Fatal(err)
	}
	if body != "via-proxy" {
		t.Fatalf("body = %q, want via-proxy", body)
	}
	urls, auths := p.seen()
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:s3cret"))
	if diff := cmp.Diff([]string{"http://origin.test/file.bin"}, urls); diff != "" {
		t.Errorf("proxied URLs (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{wantAuth}, auths); diff != "" {
		t.Errorf("Proxy-Authorization (-want +got):\n%s", diff)
	}
}

func TestProxyNoneIgnoresEnvironment(t *testing.T) {
	p := newForwardProxy(t)
	_, dial := newOrigin(t)
	env := map[string]string{"HTTP_PROXY": p.URL}
	f := New(Options{DialContext: dial, Getenv: func(k string) string { return env[k] }, SystemProxy: noOSProxy})
	t.Cleanup(func() { _ = f.Close() })

	c, err := f.Client(proxySettings(settings.ProxyNone, ""))
	if err != nil {
		t.Fatal(err)
	}
	body, err := get(t, c, "http://origin.test/", nil)
	if err != nil || body != "direct" {
		t.Fatalf("got %q, %v; want direct", body, err)
	}
	if urls, _ := p.seen(); len(urls) != 0 {
		t.Errorf("proxy was used: %v", urls)
	}
}

func TestSOCKS5(t *testing.T) {
	origin, _ := newOrigin(t)
	tests := []struct {
		name   string
		scheme string
		user   string
		want   string // request recorded by the SOCKS server
	}{
		{name: "socks5h resolves remotely", scheme: "socks5h", want: "domain:origin.test:80"},
		{name: "socks5 resolves locally", scheme: "socks5", want: "ip:192.0.2.10:80"},
		{name: "socks5 with credentials", scheme: "socks5", user: "bob", want: "ip:192.0.2.10:80"},
		{name: "socks5h with credentials", scheme: "socks5h", user: "bob", want: "domain:origin.test:80"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pass := ""
			if tt.user != "" {
				pass = "pw-" + tt.user
			}
			s := newSOCKSServer(t, serverAddr(origin), tt.user, pass)
			f := New(Options{
				Resolver: fakeResolver{"origin.test": "192.0.2.10"},
				Getenv:   noEnv, SystemProxy: noOSProxy,
			})
			t.Cleanup(func() { _ = f.Close() })
			u := tt.scheme + "://" + s.addr()
			if tt.user != "" {
				u = tt.scheme + "://" + tt.user + ":" + pass + "@" + s.addr()
			}
			c, err := f.Client(proxySettings(settings.ProxyCustom, u))
			if err != nil {
				t.Fatal(err)
			}
			body, err := get(t, c, "http://origin.test/", nil)
			if err != nil || body != "direct" {
				t.Fatalf("got %q, %v", body, err)
			}
			if diff := cmp.Diff([]string{tt.want}, s.seen()); diff != "" {
				t.Errorf("SOCKS requests (-want +got):\n%s", diff)
			}
		})
	}
}

func TestSOCKS5WrongCredentials(t *testing.T) {
	origin, _ := newOrigin(t)
	s := newSOCKSServer(t, serverAddr(origin), "bob", "right")
	for _, scheme := range []string{"socks5", "socks5h"} {
		t.Run(scheme, func(t *testing.T) {
			f := New(Options{Resolver: fakeResolver{"origin.test": "192.0.2.10"}, Getenv: noEnv, SystemProxy: noOSProxy})
			t.Cleanup(func() { _ = f.Close() })
			c, err := f.Client(proxySettings(settings.ProxyCustom, scheme+"://bob:wrong-pass@"+s.addr()))
			if err != nil {
				t.Fatal(err)
			}
			_, err = get(t, c, "http://origin.test/", nil)
			if err == nil {
				t.Fatal("expected an authentication error")
			}
			if strings.Contains(err.Error(), "wrong-pass") {
				t.Errorf("error leaks the proxy password: %v", err)
			}
		})
	}
}

func TestSystemProxyFromEnvironment(t *testing.T) {
	p := newForwardProxy(t)
	_, dial := newOrigin(t)
	var mu envMap
	mu.set(map[string]string{"http_proxy": p.URL, "NO_PROXY": "api.internal"})
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	var osLookups atomic.Int32
	f := New(Options{
		DialContext: dial,
		Getenv:      mu.get,
		SystemProxy: func(context.Context) (*platformProxy, error) {
			osLookups.Add(1)
			return &platformProxy{}, nil
		},
		Clock: clock.Now, SystemProxyTTL: time.Minute,
	})
	t.Cleanup(func() { _ = f.Close() })
	c, err := f.Client(proxySettings(settings.ProxySystem, ""))
	if err != nil {
		t.Fatal(err)
	}

	if body, err := get(t, c, "http://origin.test/a", nil); err != nil || body != "via-proxy" {
		t.Fatalf("origin.test: got %q, %v; want via-proxy", body, err)
	}
	if body, err := get(t, c, "http://api.internal/b", nil); err != nil || body != "direct" {
		t.Fatalf("NO_PROXY host: got %q, %v; want direct", body, err)
	}
	if osLookups.Load() != 0 {
		t.Errorf("OS proxy settings consulted although the environment sets a proxy")
	}

	// The environment is re-read once the TTL expires (unlike
	// http.ProxyFromEnvironment, which caches it forever).
	mu.set(map[string]string{})
	if body, _ := get(t, c, "http://origin.test/c", nil); body != "via-proxy" {
		t.Errorf("within TTL: got %q, want cached proxy", body)
	}
	clock.Advance(2 * time.Minute)
	if body, err := get(t, c, "http://origin.test/d", nil); err != nil || body != "direct" {
		t.Errorf("after TTL: got %q, %v; want direct", body, err)
	}
}

func TestSystemProxyFromOS(t *testing.T) {
	p := newForwardProxy(t)
	_, dial := newOrigin(t)
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	var lookups atomic.Int32
	var fail atomic.Bool
	f := New(Options{
		DialContext: dial,
		Getenv:      noEnv,
		SystemProxy: func(context.Context) (*platformProxy, error) {
			lookups.Add(1)
			if fail.Load() {
				return nil, errors.New("scutil exploded")
			}
			return &platformProxy{HTTP: p.URL, Bypass: []string{"*.internal"}, BypassSimpleHostnames: true}, nil
		},
		Clock: clock.Now, SystemProxyTTL: time.Minute,
	})
	t.Cleanup(func() { _ = f.Close() })
	c, err := f.Client(proxySettings(settings.ProxySystem, ""))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct{ url, want string }{
		{"http://origin.test/", "via-proxy"},
		{"http://api.internal/", "direct"}, // bypass list
		{"http://intranet/", "direct"},     // simple host name
	}
	for _, tt := range tests {
		if body, err := get(t, c, tt.url, nil); err != nil || body != tt.want {
			t.Errorf("%s: got %q, %v; want %q", tt.url, body, err, tt.want)
		}
	}
	if n := lookups.Load(); n != 1 {
		t.Errorf("OS lookups = %d, want 1 (cached within TTL)", n)
	}

	// A failed lookup falls back to direct connections.
	fail.Store(true)
	clock.Advance(2 * time.Minute)
	if body, err := get(t, c, "http://origin.test/", nil); err != nil || body != "direct" {
		t.Errorf("after failed lookup: got %q, %v; want direct", body, err)
	}
	if n := lookups.Load(); n != 2 {
		t.Errorf("OS lookups = %d, want 2", n)
	}
}

func TestSystemProxySOCKSFromOS(t *testing.T) {
	origin, _ := newOrigin(t)
	s := newSOCKSServer(t, serverAddr(origin), "", "")
	f := New(Options{
		Getenv: noEnv,
		SystemProxy: func(context.Context) (*platformProxy, error) {
			return &platformProxy{SOCKS: "socks5://" + s.addr()}, nil
		},
	})
	t.Cleanup(func() { _ = f.Close() })
	c, err := f.Client(proxySettings(settings.ProxySystem, ""))
	if err != nil {
		t.Fatal(err)
	}
	if body, err := get(t, c, "http://origin.test/", nil); err != nil || body != "direct" {
		t.Fatalf("got %q, %v", body, err)
	}
	if diff := cmp.Diff([]string{"domain:origin.test:80"}, s.seen()); diff != "" {
		t.Errorf("SOCKS requests (-want +got):\n%s", diff)
	}
}

func TestInvalidProxy(t *testing.T) {
	tests := []struct {
		name  string
		proxy settings.Proxy
	}{
		{"unknown mode", settings.Proxy{Mode: "pac"}},
		{"unsupported scheme", settings.Proxy{Mode: settings.ProxyCustom, URL: "ftp://u:s3cret@proxy:21"}},
		{"missing host", settings.Proxy{Mode: settings.ProxyCustom, URL: "http://u:s3cret@:8080"}},
		{"malformed", settings.Proxy{Mode: settings.ProxyCustom, URL: "http://u:s3cret@proxy:port"}},
		{"empty", settings.Proxy{Mode: settings.ProxyCustom}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			log, buf := newTestLogger()
			f := New(Options{Logger: log})
			_, err := f.Client(settings.Settings{Proxy: settings.Some(tt.proxy)})
			if !errors.Is(err, ErrInvalidProxy) {
				t.Fatalf("err = %v, want ErrInvalidProxy", err)
			}
			if strings.Contains(err.Error(), "s3cret") || strings.Contains(buf.String(), "s3cret") {
				t.Errorf("credentials leaked: err=%q log=%q", err, buf.String())
			}
		})
	}
}

func TestCredentialsRedacted(t *testing.T) {
	log, buf := newTestLogger()
	f := New(Options{Logger: log})
	t.Cleanup(func() { _ = f.Close() })
	s := proxySettings(settings.ProxyCustom, "http://alice:s3cret@proxy.example:3128")
	s.TLSVerify = settings.Some(false)
	if _, err := f.Client(s); err != nil {
		t.Fatal(err)
	}
	logs := buf.String()
	if strings.Contains(logs, "s3cret") || strings.Contains(logs, "alice") {
		t.Errorf("log leaks proxy credentials: %s", logs)
	}
	if !strings.Contains(logs, "proxy.example:3128") {
		t.Errorf("log should still name the proxy host: %s", logs)
	}
	cfg := TransportConfig{Proxy: settings.Proxy{Mode: settings.ProxyCustom, URL: "socks5://tok3n@proxy:1080"}}
	if str := cfg.String(); strings.Contains(str, "tok3n") || !strings.Contains(str, "proxy:1080") {
		t.Errorf("TransportConfig.String() = %q", str)
	}
}
