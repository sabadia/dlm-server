package netclient

import (
	"testing"

	"github.com/sabadia/dlm-server/internal/settings"
)

func TestProxyRulesRoutePerHost(t *testing.T) {
	p := newForwardProxy(t)
	_, dial := newOrigin(t)
	f := New(Options{DialContext: dial, Getenv: noEnv, SystemProxy: noOSProxy})
	t.Cleanup(func() { _ = f.Close() })

	s := proxySettings(settings.ProxyNone, "")
	s.ProxyRules = settings.Some([]settings.ProxyRule{
		{Hosts: []string{"*.video.test"}, Proxy: settings.Proxy{Mode: settings.ProxyCustom, URL: p.URL}},
	})
	c, err := f.Client(s)
	if err != nil {
		t.Fatal(err)
	}

	if body, err := get(t, c, "http://cdn.video.test/a.mp4", nil); err != nil || body != "via-proxy" {
		t.Fatalf("matching host: got %q, %v; want via-proxy", body, err)
	}
	if body, err := get(t, c, "http://origin.test/", nil); err != nil || body != "direct" {
		t.Fatalf("non-matching host: got %q, %v; want direct", body, err)
	}
	if urls, _ := p.seen(); len(urls) != 1 || urls[0] != "http://cdn.video.test/a.mp4" {
		t.Errorf("proxied URLs = %v", urls)
	}

	// Transports are shared: the rule's transport comes from the cache.
	direct, _ := f.transport(TransportConfig{Proxy: settings.Proxy{Mode: settings.ProxyNone}, TLSVerify: true})
	again, _ := f.transport(TransportConfig{Proxy: settings.Proxy{Mode: settings.ProxyNone}, TLSVerify: true})
	if direct != again {
		t.Error("transport cache not reused")
	}
}
