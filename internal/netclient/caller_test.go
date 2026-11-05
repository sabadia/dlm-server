package netclient

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/sabadia/dlm-server/internal/callctx"
	"github.com/sabadia/dlm-server/internal/settings"
)

type fakeTaskCookies map[string][]*http.Cookie

func (f fakeTaskCookies) TaskCookies(_ context.Context, taskID string, _ *url.URL) []*http.Cookie {
	return f[taskID]
}

func fetchAs(ctx context.Context, t *testing.T, c *http.Client, rawURL string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	return c.Do(req)
}

func TestTaskCookiesGoOnlyToTheirTask(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Cookie"))
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	jar, err := newTestJar(srv.URL, "sid=users; theme=dark")
	if err != nil {
		t.Fatal(err)
	}
	f := New(Options{
		Getenv: noEnv, SystemProxy: noOSProxy, CookieJar: jar,
		TaskCookies: fakeTaskCookies{"t1": {{Name: "sid", Value: "captured"}, {Name: "dl", Value: "1"}}},
	})
	t.Cleanup(func() { _ = f.Close() })
	c, err := f.Client(settings.Settings{Proxy: settings.Some(settings.Proxy{Mode: settings.ProxyNone})})
	if err != nil {
		t.Fatal(err)
	}

	for _, ctx := range []context.Context{
		callctx.With(t.Context(), callctx.Caller{TaskID: "t1"}),
		callctx.With(t.Context(), callctx.Caller{TaskID: "t2"}),
		t.Context(),
	} {
		resp, err := fetchAs(ctx, t, c, srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	if len(seen) != 3 {
		t.Fatalf("saw %d requests", len(seen))
	}
	// t1: its cookies first, its value wins over the jar's cookie of the same name.
	if got := seen[0]; !strings.HasPrefix(got, "sid=captured; dl=1") || strings.Contains(got, "sid=users") || !strings.Contains(got, "theme=dark") {
		t.Errorf("task t1 sent %q", got)
	}
	// Any other request sends the jar's cookies and nothing of t1.
	for _, got := range seen[1:] {
		if strings.Contains(got, "captured") || strings.Contains(got, "dl=1") || !strings.Contains(got, "sid=users") {
			t.Errorf("another request sent %q", got)
		}
	}
}

func TestRestrictedWorkMayNotConnectToTheLocalMachine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	t.Cleanup(srv.Close)
	f := New(Options{Getenv: noEnv, SystemProxy: noOSProxy})
	t.Cleanup(func() { _ = f.Close() })
	c, err := f.Client(settings.Settings{Proxy: settings.Some(settings.Proxy{Mode: settings.ProxyNone})})
	if err != nil {
		t.Fatal(err)
	}

	// An ordinary request reaches it.
	resp, err := fetchAs(t.Context(), t, c, srv.URL)
	if err != nil {
		t.Fatalf("ordinary request: %v", err)
	}
	_ = resp.Body.Close()

	// A restricted one is refused when the connection is made, by name as well
	// as by address (the name resolves to the loopback address).
	for _, u := range []string{srv.URL, strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)} {
		resp, err := fetchAs(callctx.With(t.Context(), callctx.Caller{Restricted: true}), t, c, u)
		if resp != nil {
			_ = resp.Body.Close()
		}
		if !errors.Is(err, ErrLocalAddress) {
			t.Errorf("restricted request to %s: %v, want ErrLocalAddress", u, err)
		}
	}
}

func TestRestrictedWorkMayUseAProxyOnThisMachine(t *testing.T) {
	// A proxy on the loopback address is the user's choice; what it fetches is
	// its business.
	var viaProxy int
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		viaProxy++
		_, _ = w.Write([]byte("proxied"))
	}))
	t.Cleanup(proxy.Close)
	f := New(Options{Getenv: noEnv, SystemProxy: noOSProxy})
	t.Cleanup(func() { _ = f.Close() })
	c, err := f.Client(settings.Settings{Proxy: settings.Some(settings.Proxy{Mode: settings.ProxyCustom, URL: proxy.URL})})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := fetchAs(callctx.With(t.Context(), callctx.Caller{Restricted: true}), t, c, "http://files.example.test/a.bin")
	if err != nil {
		t.Fatalf("through the proxy: %v", err)
	}
	_ = resp.Body.Close()
	if viaProxy != 1 {
		t.Errorf("the proxy saw %d requests", viaProxy)
	}
}

func TestLocalAddresses(t *testing.T) {
	for in, want := range map[string]bool{
		"127.0.0.1": true, "::1": true, "0.0.0.0": true, "169.254.169.254": true, "fe80::1": true,
		"10.0.0.5": false, "192.168.1.9": false, "93.184.216.34": false, "2606:4700::1111": false,
	} {
		if got := Local(net.ParseIP(in)); got != want {
			t.Errorf("Local(%s) = %v, want %v", in, got, want)
		}
	}
}

// newTestJar returns a jar that already holds cookies ("a=1; b=2") for rawURL.
func newTestJar(rawURL, cookies string) (http.CookieJar, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	var cs []*http.Cookie
	for _, part := range strings.Split(cookies, "; ") {
		name, value, _ := strings.Cut(part, "=")
		cs = append(cs, &http.Cookie{Name: name, Value: value})
	}
	jar.SetCookies(u, cs)
	return jar, nil
}

// The dialer's own check is the last line: it sees the address that is really
// connected, so a name that changed to a local address after the pre-flight
// lookup (rebinding) is still refused.
func TestDialControlRefusesLocalAddresses(t *testing.T) {
	for addr, refused := range map[string]bool{
		"127.0.0.1:80": true, "[::1]:80": true, "169.254.169.254:80": true, "0.0.0.0:80": true,
		"93.184.216.34:443": false, "10.1.2.3:80": false, "[2606:4700::1111]:443": false,
	} {
		err := refuseLocal("tcp", addr, nil)
		if refused != errors.Is(err, ErrLocalAddress) {
			t.Errorf("refuseLocal(%s) = %v, refused want %v", addr, err, refused)
		}
	}
}

func TestPreflightResolvesTheNameOfARestrictedRequest(t *testing.T) {
	tr := &callerTransport{resolver: stubResolver{"cdn.example.test": {net.ParseIP("93.184.216.34")}, "rebind.example.test": {net.ParseIP("127.0.0.1")}}}
	for host, want := range map[string]bool{"cdn.example.test": false, "rebind.example.test": true, "localhost": true, "foo.localhost": true, "169.254.169.254": true, "nowhere.example.test": false} {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+host+"/", nil)
		err := tr.refuseLocalTarget(t.Context(), req)
		if want != errors.Is(err, ErrLocalAddress) {
			t.Errorf("%s: err = %v, refused want %v", host, err, want)
		}
	}
	// Through a proxy it is the proxy that resolves and connects.
	tr.proxied = func(*http.Request) bool { return true }
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://localhost/", nil)
	if err := tr.refuseLocalTarget(t.Context(), req); err != nil {
		t.Errorf("through a proxy: %v", err)
	}
}

type stubResolver map[string][]net.IP

func (s stubResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	ips, ok := s[host]
	if !ok {
		return nil, errors.New("no such host")
	}
	out := make([]net.IPAddr, len(ips))
	for i, ip := range ips {
		out[i] = net.IPAddr{IP: ip}
	}
	return out, nil
}
