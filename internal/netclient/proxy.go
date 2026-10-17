package netclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http/httpproxy"
	"golang.org/x/net/proxy"

	"github.com/sabadia/dlm-server/internal/platform"
	"github.com/sabadia/dlm-server/internal/settings"
)

// systemProxyLookupTimeout bounds one OS proxy lookup (scutil, registry).
const systemProxyLookupTimeout = 3 * time.Second

func (f *Factory) configureProxy(t *http.Transport, p settings.Proxy) error {
	switch p.Mode {
	case settings.ProxyNone:
		t.Proxy = nil
	case settings.ProxySystem:
		t.Proxy = f.system.proxy
	case settings.ProxyCustom:
		u, err := parseProxyURL(p.URL)
		if err != nil {
			return err
		}
		if u.Scheme == "socks5" {
			// net/http treats socks5 like socks5h (remote DNS); dial
			// through x/net/proxy instead and resolve locally.
			dial, err := f.socksLocalDNS(u)
			if err != nil {
				return err
			}
			t.Proxy = nil
			t.DialContext = dial
			return nil
		}
		t.Proxy = http.ProxyURL(u)
	default:
		return fmt.Errorf("%w: unknown mode %q", ErrInvalidProxy, p.Mode)
	}
	return nil
}

// parseProxyURL validates a custom proxy URL. Errors never include the URL
// itself because it may carry credentials.
func parseProxyURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: malformed url", ErrInvalidProxy)
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("%w: unsupported scheme %q", ErrInvalidProxy, u.Scheme)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("%w: missing host", ErrInvalidProxy)
	}
	return u, nil
}

// redactProxyURL returns raw with the whole userinfo (user names can be
// tokens too) replaced, or a placeholder when it does not parse (it may
// still contain secrets).
func redactProxyURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparsable>"
	}
	if u.User != nil {
		u.User = url.User("xxxxx")
	}
	return u.String()
}

// contextDialer adapts a DialContext function to proxy.Dialer and
// proxy.ContextDialer.
type contextDialer func(ctx context.Context, network, addr string) (net.Conn, error)

func (d contextDialer) Dial(network, addr string) (net.Conn, error) {
	return d(context.Background(), network, addr)
}

func (d contextDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return d(ctx, network, addr)
}

// socksLocalDNS returns a DialContext that resolves the target host locally
// and connects to the resulting IP through the SOCKS5 proxy u.
func (f *Factory) socksLocalDNS(u *url.URL) (func(ctx context.Context, network, addr string) (net.Conn, error), error) {
	var auth *proxy.Auth
	if u.User != nil {
		pass, _ := u.User.Password()
		auth = &proxy.Auth{User: u.User.Username(), Password: pass}
	}
	host := u.Host
	if u.Port() == "" {
		host = net.JoinHostPort(u.Hostname(), "1080")
	}
	d, err := proxy.SOCKS5("tcp", host, auth, contextDialer(f.dial))
	if err != nil {
		return nil, fmt.Errorf("%w: socks5: %w", ErrInvalidProxy, err)
	}
	cd, ok := d.(proxy.ContextDialer)
	if !ok {
		return nil, fmt.Errorf("%w: socks5 dialer lacks context support", ErrInvalidProxy)
	}
	resolver := f.opts.Resolver
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		h, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("netclient: socks5 dial %q: %w", addr, err)
		}
		if net.ParseIP(h) != nil {
			return cd.DialContext(ctx, network, addr) //nolint:wrapcheck // dial errors are self-describing
		}
		ips, err := resolver.LookupIPAddr(ctx, h)
		if err != nil {
			return nil, fmt.Errorf("netclient: socks5 resolve %q: %w", h, err)
		}
		var errs []error
		for _, ip := range ips {
			conn, err := cd.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
			if err == nil {
				return conn, nil
			}
			errs = append(errs, err)
			if ctx.Err() != nil {
				break
			}
		}
		if len(errs) == 0 {
			return nil, fmt.Errorf("netclient: socks5 resolve %q: no addresses", h)
		}
		return nil, fmt.Errorf("netclient: socks5 dial %q: %w", h, errors.Join(errs...))
	}, nil
}

// systemProxy evaluates the system proxy configuration (environment, then
// OS settings), cached for ttl.
type systemProxy struct {
	getenv func(string) string
	lookup func(ctx context.Context) (*platform.ProxySettings, error)
	ttl    time.Duration
	now    func() time.Time
	log    *slog.Logger

	mu      sync.Mutex
	expires time.Time
	fn      func(*url.URL) (*url.URL, error)
	simple  bool // bypass host names without a dot
}

// proxy is an http.Transport.Proxy function.
func (p *systemProxy) proxy(req *http.Request) (*url.URL, error) {
	fn, simple := p.current(req.Context())
	if simple && !strings.Contains(req.URL.Hostname(), ".") && net.ParseIP(req.URL.Hostname()) == nil {
		return nil, nil
	}
	return fn(req.URL)
}

func (p *systemProxy) current(ctx context.Context) (func(*url.URL) (*url.URL, error), bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if p.fn != nil && now.Before(p.expires) {
		return p.fn, p.simple
	}
	p.fn, p.simple = p.load(ctx)
	p.expires = now.Add(p.ttl)
	return p.fn, p.simple
}

func (p *systemProxy) load(ctx context.Context) (func(*url.URL) (*url.URL, error), bool) {
	env := httpproxy.Config{
		HTTPProxy:  p.env("HTTP_PROXY"),
		HTTPSProxy: p.env("HTTPS_PROXY"),
		NoProxy:    p.env("NO_PROXY"),
	}
	if env.HTTPProxy != "" || env.HTTPSProxy != "" {
		return env.ProxyFunc(), false
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), systemProxyLookupTimeout)
	defer cancel()
	sp, err := p.lookup(ctx)
	if err != nil {
		p.log.Warn("netclient: reading OS proxy settings failed; connecting directly", "error", err)
		return env.ProxyFunc(), false
	}
	if sp.IsZero() {
		return env.ProxyFunc(), false
	}
	cfg := httpproxy.Config{HTTPProxy: sp.HTTP, HTTPSProxy: sp.HTTPS, NoProxy: sp.NoProxy()}
	if cfg.HTTPProxy == "" {
		cfg.HTTPProxy = sp.SOCKS
	}
	if cfg.HTTPSProxy == "" {
		cfg.HTTPSProxy = sp.SOCKS
	}
	return cfg.ProxyFunc(), sp.BypassSimpleHostnames
}

// env returns the upper-case variable, or the lower-case one if unset.
func (p *systemProxy) env(name string) string {
	if v := p.getenv(name); v != "" {
		return v
	}
	return p.getenv(strings.ToLower(name))
}
