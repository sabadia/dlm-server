package netclient

import (
	"container/list"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/platform"
	"github.com/sabadia/dlm-server/internal/settings"
)

// Defaults for Options fields left zero.
const (
	DefaultDialTimeout           = 15 * time.Second
	DefaultTLSHandshakeTimeout   = 15 * time.Second
	DefaultResponseHeaderTimeout = 30 * time.Second
	DefaultIdleConnTimeout       = 90 * time.Second
	DefaultMaxIdleConnsPerHost   = 32
	DefaultMaxTransports         = 16
	DefaultSystemProxyTTL        = 30 * time.Second
	// MaxRedirects is the number of redirects a client follows.
	MaxRedirects = 10
)

// ErrTooManyRedirects is returned (wrapped in a *url.Error) when a request
// exceeds MaxRedirects.
var ErrTooManyRedirects = errors.New("netclient: too many redirects")

// ErrInvalidProxy reports an unusable proxy setting.
var ErrInvalidProxy = errors.New("netclient: invalid proxy")

// Resolver resolves host names; *net.Resolver implements it.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// TransportConfig is the transport-relevant part of the settings; it is the
// transport cache key.
type TransportConfig struct {
	// Proxy may contain credentials: never log it directly (String and
	// LogValue redact them).
	Proxy     settings.Proxy
	TLSVerify bool
}

// String describes the config with proxy credentials redacted.
func (c TransportConfig) String() string {
	return fmt.Sprintf("proxy=%s %s tlsVerify=%t", c.Proxy.Mode, redactProxyURL(c.Proxy.URL), c.TLSVerify)
}

// LogValue implements slog.LogValuer with credentials redacted.
func (c TransportConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("proxy_mode", string(c.Proxy.Mode)),
		slog.String("proxy_url", redactProxyURL(c.Proxy.URL)),
		slog.Bool("tls_verify", c.TLSVerify),
	)
}

// FallbackFunc wraps the transport built for cfg, for example with a
// RoundTripper that retries through a browser-impersonating TLS stack when
// the primary transport is fingerprint-blocked. It is called once per
// cached transport; returning nil keeps primary. If the returned
// RoundTripper has a CloseIdleConnections method, it is called on eviction
// and Close.
type FallbackFunc func(cfg TransportConfig, primary *http.Transport) http.RoundTripper

// Options configures a Factory. Zero values select the defaults.
type Options struct {
	Logger *slog.Logger
	// CookieJar, when set, is shared by every client.
	CookieJar http.CookieJar

	DialTimeout           time.Duration
	TLSHandshakeTimeout   time.Duration
	ResponseHeaderTimeout time.Duration
	IdleConnTimeout       time.Duration
	MaxIdleConnsPerHost   int
	// MaxTransports bounds the transport cache.
	MaxTransports int

	// DialContext replaces the TCP dialer (tests); it must honour ctx.
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	// Resolver resolves target hosts for socks5:// (local DNS) proxies.
	Resolver Resolver

	// Getenv reads environment variables for the system proxy mode
	// (default os.Getenv).
	Getenv func(string) string
	// SystemProxy reads the OS proxy settings (default platform.SystemProxy).
	SystemProxy func(ctx context.Context) (*platform.ProxySettings, error)
	// SystemProxyTTL is how long the system proxy configuration is cached.
	SystemProxyTTL time.Duration
	// Clock is used for the system proxy cache (default time.Now).
	Clock func() time.Time

	// Fallback optionally wraps each cached transport.
	Fallback FallbackFunc
}

// Factory builds HTTP clients over cached transports. It implements
// engine.ClientFactory and is safe for concurrent use.
type Factory struct {
	opts   Options
	log    *slog.Logger
	dial   func(ctx context.Context, network, addr string) (net.Conn, error)
	system *systemProxy

	mu    sync.Mutex
	lru   *list.List // of *cacheEntry, most recently used first
	items map[TransportConfig]*list.Element
}

var _ engine.ClientFactory = (*Factory)(nil)

type cacheEntry struct {
	key       TransportConfig
	transport *http.Transport
	rt        http.RoundTripper
}

// New returns a Factory.
func New(opts Options) *Factory {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	setDefault(&opts.DialTimeout, DefaultDialTimeout)
	setDefault(&opts.TLSHandshakeTimeout, DefaultTLSHandshakeTimeout)
	setDefault(&opts.ResponseHeaderTimeout, DefaultResponseHeaderTimeout)
	setDefault(&opts.IdleConnTimeout, DefaultIdleConnTimeout)
	setDefault(&opts.MaxIdleConnsPerHost, DefaultMaxIdleConnsPerHost)
	setDefault(&opts.MaxTransports, DefaultMaxTransports)
	setDefault(&opts.SystemProxyTTL, DefaultSystemProxyTTL)
	if opts.Resolver == nil {
		opts.Resolver = net.DefaultResolver
	}
	if opts.Getenv == nil {
		opts.Getenv = os.Getenv
	}
	if opts.SystemProxy == nil {
		opts.SystemProxy = platform.SystemProxy
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	dial := opts.DialContext
	if dial == nil {
		d := &net.Dialer{Timeout: opts.DialTimeout, KeepAlive: 30 * time.Second}
		dial = d.DialContext
	}
	f := &Factory{
		opts:  opts,
		log:   opts.Logger,
		dial:  dial,
		lru:   list.New(),
		items: make(map[TransportConfig]*list.Element),
	}
	f.system = &systemProxy{
		getenv: opts.Getenv,
		lookup: opts.SystemProxy,
		ttl:    opts.SystemProxyTTL,
		now:    opts.Clock,
		log:    opts.Logger,
	}
	return f
}

func setDefault[T comparable](v *T, def T) {
	var zero T
	if *v == zero {
		*v = def
	}
}

// Client returns an HTTP client honouring the effective settings: proxy,
// TLS verification, user agent and extra headers. Unset settings fall back
// to the built-in defaults (system proxy, TLS verification on,
// settings.DefaultUserAgent).
func (f *Factory) Client(s settings.Settings) (*http.Client, error) {
	cfg := TransportConfig{
		Proxy:     s.Proxy.OrElse(settings.Proxy{Mode: settings.ProxySystem}),
		TLSVerify: s.TLSVerify.OrElse(true),
	}
	rt, err := f.transport(cfg)
	if err != nil {
		return nil, err
	}
	if rules := s.ProxyRules.Value(); len(rules) > 0 {
		rt = &ruleTransport{factory: f, settings: s, tlsVerify: cfg.TLSVerify}
	}
	return &http.Client{
		Transport:     newHeaderTransport(rt, s.UserAgent.OrElse(settings.DefaultUserAgent), s.Headers.Value(), f.log),
		CheckRedirect: checkRedirect,
		Jar:           f.opts.CookieJar,
	}, nil
}

func checkRedirect(_ *http.Request, via []*http.Request) error {
	// via holds the requests already sent: the initial one plus one per
	// redirect followed so far.
	if len(via) > MaxRedirects {
		return fmt.Errorf("%w: stopped after %d redirects", ErrTooManyRedirects, MaxRedirects)
	}
	return nil
}

// transport returns the cached RoundTripper for cfg, building it if needed.
func (f *Factory) transport(cfg TransportConfig) (http.RoundTripper, error) {
	f.mu.Lock()
	if el, ok := f.items[cfg]; ok {
		f.lru.MoveToFront(el)
		rt := el.Value.(*cacheEntry).rt //nolint:errcheck,forcetypeassert // the list only holds *cacheEntry
		f.mu.Unlock()
		return rt, nil
	}
	f.mu.Unlock()

	// Build outside the lock; a concurrent builder for the same key loses
	// and its transport is discarded (it has no connections yet).
	entry, err := f.build(cfg)
	if err != nil {
		return nil, err
	}

	f.mu.Lock()
	if el, ok := f.items[cfg]; ok {
		f.lru.MoveToFront(el)
		rt := el.Value.(*cacheEntry).rt //nolint:errcheck,forcetypeassert // the list only holds *cacheEntry
		f.mu.Unlock()
		return rt, nil
	}
	f.items[cfg] = f.lru.PushFront(entry)
	var evicted []*cacheEntry
	for f.lru.Len() > f.opts.MaxTransports {
		last := f.lru.Back()
		e := f.lru.Remove(last).(*cacheEntry) //nolint:errcheck,forcetypeassert // the list only holds *cacheEntry
		delete(f.items, e.key)
		evicted = append(evicted, e)
	}
	f.mu.Unlock()

	if entry.key.TLSVerify {
		f.log.Debug("netclient: transport created", "config", cfg)
	} else {
		f.log.Warn("netclient: TLS certificate verification disabled", "config", cfg)
	}
	for _, e := range evicted {
		e.closeIdle()
	}
	return entry.rt, nil
}

func (e *cacheEntry) closeIdle() {
	e.transport.CloseIdleConnections()
	if c, ok := e.rt.(interface{ CloseIdleConnections() }); ok && e.rt != http.RoundTripper(e.transport) {
		c.CloseIdleConnections()
	}
}

func (f *Factory) build(cfg TransportConfig) (*cacheEntry, error) {
	t := &http.Transport{
		DialContext: f.dial,
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: !cfg.TLSVerify, //nolint:gosec // explicit user opt-out (tlsVerify=false), logged
		},
		TLSHandshakeTimeout:   f.opts.TLSHandshakeTimeout,
		ResponseHeaderTimeout: f.opts.ResponseHeaderTimeout,
		IdleConnTimeout:       f.opts.IdleConnTimeout,
		MaxIdleConns:          f.opts.MaxIdleConnsPerHost * 8,
		MaxIdleConnsPerHost:   f.opts.MaxIdleConnsPerHost,
		ExpectContinueTimeout: time.Second,
		// Byte-accurate ranges: never let the transport negotiate and
		// transparently decode gzip.
		DisableCompression: true,
	}
	t.Protocols = new(http.Protocols)
	t.Protocols.SetHTTP1(true)
	t.Protocols.SetHTTP2(true)

	if err := f.configureProxy(t, cfg.Proxy); err != nil {
		return nil, err
	}

	entry := &cacheEntry{key: cfg, transport: t, rt: t}
	if f.opts.Fallback != nil {
		if rt := f.opts.Fallback(cfg, t); rt != nil {
			entry.rt = rt
		}
	}
	return entry, nil
}

// Close closes the idle connections of every cached transport and empties
// the cache. The Factory stays usable; later Client calls build new
// transports.
func (f *Factory) Close() error {
	f.mu.Lock()
	entries := make([]*cacheEntry, 0, f.lru.Len())
	for el := f.lru.Front(); el != nil; el = el.Next() {
		entries = append(entries, el.Value.(*cacheEntry)) //nolint:errcheck,forcetypeassert // the list only holds *cacheEntry
	}
	f.lru.Init()
	clear(f.items)
	f.mu.Unlock()
	for _, e := range entries {
		e.closeIdle()
	}
	return nil
}
