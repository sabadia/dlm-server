package netclient

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

// Impersonation defaults.
const (
	// DefaultImpersonationTTL is how long a host that needed impersonation is
	// remembered, so its later requests skip the attempt that fails.
	DefaultImpersonationTTL = time.Hour
	// DefaultImpersonationHosts bounds the hosts remembered.
	DefaultImpersonationHosts = 1024

	impersonationHandshakeTimeout = 15 * time.Second
	// pendingConnLifetime is how long a connection made to learn the
	// negotiated protocol waits for the request that needs it.
	pendingConnLifetime = 30 * time.Second
)

// ImpersonationOptions configure NewImpersonation.
type ImpersonationOptions struct {
	Logger *slog.Logger
	// TTL is how long a host that needed impersonation is remembered; default
	// DefaultImpersonationTTL.
	TTL time.Duration
	// MaxHosts bounds the remembered hosts; default DefaultImpersonationHosts.
	MaxHosts int
	// Clock is for the TTL (default time.Now).
	Clock func() time.Time
}

// NewImpersonation returns a Fallback for Options that retries requests a site
// refused because of Go's TLS fingerprint through a Chrome-like one.
//
// Every request goes through the normal transport first. When the answer looks
// like a fingerprint block (a TLS handshake failure, or a 403/503 that
// Cloudflare marks as a challenge) and the request is idempotent and can be
// replayed, it is sent again through a transport whose ClientHello copies
// Chrome's (uTLS), over HTTP/2 or HTTP/1.1 as the server chooses. If that works,
// the host is remembered for a while and its later requests go straight there.
// The proxy, the dialer, TLS verification and the root CAs of the normal
// transport apply to both. JavaScript challenges are not solved: that is the
// internal browser's job.
func NewImpersonation(o ImpersonationOptions) FallbackFunc {
	if o.TTL <= 0 {
		o.TTL = DefaultImpersonationTTL
	}
	if o.MaxHosts <= 0 {
		o.MaxHosts = DefaultImpersonationHosts
	}
	if o.Clock == nil {
		o.Clock = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	return func(_ TransportConfig, primary *http.Transport) http.RoundTripper {
		return newImpersonator(o, primary)
	}
}

type proxyKey struct{}

// impersonator is the RoundTripper NewImpersonation installs.
type impersonator struct {
	primary *http.Transport
	log     *slog.Logger
	hosts   *hostCache
	h1      *http.Transport
	// h2 speaks HTTP/2 over uTLS connections. net/http only does HTTP/2 itself
	// over a crypto/tls connection, so x/net/http2 is needed despite its
	// deprecation notice.
	h2 *http2.Transport //nolint:staticcheck // see above

	mu      sync.Mutex        // serialises protocol probes; guards protos
	protos  map[string]string // host:port -> negotiated ALPN protocol
	pending sync.Map          // host:port -> net.Conn made by a probe
	maxPrt  int
}

func newImpersonator(o ImpersonationOptions, primary *http.Transport) *impersonator {
	i := &impersonator{
		primary: primary, log: o.Logger.With("component", "netclient"),
		hosts: newHostCache(o.TTL, o.MaxHosts, o.Clock), protos: make(map[string]string), maxPrt: o.MaxHosts,
	}
	dial := func(ctx context.Context, _, addr string) (net.Conn, error) { return i.dialChrome(ctx, addr) }
	i.h1 = &http.Transport{
		DialTLSContext:        dial,
		DisableCompression:    primary.DisableCompression,
		ResponseHeaderTimeout: primary.ResponseHeaderTimeout,
		IdleConnTimeout:       primary.IdleConnTimeout,
		MaxIdleConnsPerHost:   max(primary.MaxIdleConnsPerHost, 4),
	}
	i.h2 = &http2.Transport{ //nolint:staticcheck // see the h2 field
		DialTLSContext: func(ctx context.Context, _, addr string, _ *tls.Config) (net.Conn, error) { //nolint:staticcheck // see the h2 field
			return i.dialChrome(ctx, addr)
		},
		DisableCompression: primary.DisableCompression, //nolint:staticcheck // see the h2 field
	}
	return i
}

// CloseIdleConnections closes the idle connections of every transport.
func (i *impersonator) CloseIdleConnections() {
	i.primary.CloseIdleConnections()
	i.h1.CloseIdleConnections()
	i.h2.CloseIdleConnections() //nolint:staticcheck // see the h2 field
	i.pending.Range(func(k, v any) bool {
		if c, ok := v.(net.Conn); ok {
			_ = c.Close()
		}
		i.pending.Delete(k)
		return true
	})
}

// RoundTrip implements http.RoundTripper.
func (i *impersonator) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" {
		return i.primary.RoundTrip(req) //nolint:wrapcheck // transport errors pass through unchanged
	}
	host := strings.ToLower(req.URL.Hostname())
	if i.hosts.has(host) {
		return i.viaChrome(req)
	}
	resp, err := i.primary.RoundTrip(req)
	refused := (err != nil && handshakeFailure(err)) || (err == nil && blocked(resp))
	if !refused || !retryable(req) {
		return resp, err //nolint:wrapcheck // transport errors pass through unchanged
	}
	// Try Chrome's fingerprint while the first answer is still at hand: if that
	// does not help either, the caller gets the original answer.
	retried, rerr := i.viaChrome(req)
	switch {
	case rerr == nil && !blocked(retried):
		if resp != nil {
			_ = resp.Body.Close()
		}
		if i.hosts.add(host) {
			i.log.Info("netclient: the site refuses Go's TLS fingerprint; using a Chrome one for it", "host", host)
		}
		return retried, nil
	case rerr == nil:
		_ = retried.Body.Close() // blocked the same way: not a fingerprint problem
	}
	return resp, err //nolint:wrapcheck // the original outcome
}

// retryable reports whether req may be sent a second time: an idempotent
// method whose body, if any, can be recreated.
func retryable(req *http.Request) bool {
	switch req.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
	default:
		return false
	}
	return req.Body == nil || req.Body == http.NoBody || req.GetBody != nil
}

// blocked reports whether resp is the challenge page Cloudflare sends when it
// refuses a client: a 403 or 503 it marks as a challenge, or that comes from
// its servers.
func blocked(resp *http.Response) bool {
	return resp != nil && blockedAnswer(resp.StatusCode, resp.Header)
}

func blockedAnswer(status int, h http.Header) bool {
	if status != http.StatusForbidden && status != http.StatusServiceUnavailable {
		return false
	}
	if strings.EqualFold(h.Get("Cf-Mitigated"), "challenge") {
		return true
	}
	return strings.HasPrefix(strings.ToLower(h.Get("Server")), "cloudflare") && h.Get("Cf-Ray") != ""
}

// handshakeFailure reports whether err is a TLS handshake refusal.
func handshakeFailure(err error) bool {
	var rh tls.RecordHeaderError
	if errors.As(err, &rh) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "tls: ") || strings.Contains(msg, "handshake")
}

// viaChrome sends req through the uTLS transports.
func (i *impersonator) viaChrome(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	if req.GetBody != nil && req.Body != nil && req.Body != http.NoBody {
		body, err := req.GetBody()
		if err != nil {
			return nil, fmt.Errorf("netclient: replay the request body: %w", err)
		}
		r.Body = body
	}
	var proxy *url.URL
	if i.primary.Proxy != nil {
		var err error
		if proxy, err = i.primary.Proxy(req); err != nil {
			return nil, fmt.Errorf("netclient: proxy for %s: %w", req.URL.Hostname(), err)
		}
	}
	ctx := context.WithValue(r.Context(), proxyKey{}, proxy)
	r = r.WithContext(ctx) //nolint:contextcheck // ctx derives from the request's own
	addr := canonicalAddr(req.URL)
	proto, err := i.protocol(ctx, addr) //nolint:contextcheck // ctx derives from the request's own
	if err != nil {
		return nil, err
	}
	if proto == http2.NextProtoTLS {
		return i.h2.RoundTrip(r) //nolint:wrapcheck,staticcheck // transport errors pass through unchanged; see the h2 field
	}
	return i.h1.RoundTrip(r) //nolint:wrapcheck // transport errors pass through unchanged
}

// protocol returns the ALPN protocol the server at addr picks for Chrome's
// hello. The first request to an address connects to find out, and hands that
// connection to the transport that then needs one.
func (i *impersonator) protocol(ctx context.Context, addr string) (string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if p, ok := i.protos[addr]; ok {
		return p, nil
	}
	conn, err := i.dialChromeFresh(ctx, addr)
	if err != nil {
		return "", err
	}
	proto := conn.ConnectionState().NegotiatedProtocol
	if len(i.protos) >= i.maxPrt {
		clear(i.protos) // rare; the next request per address probes again
	}
	i.protos[addr] = proto
	i.pending.Store(addr, conn)
	time.AfterFunc(pendingConnLifetime, func() {
		if c, ok := i.pending.LoadAndDelete(addr); ok {
			if nc, ok := c.(net.Conn); ok {
				_ = nc.Close()
			}
		}
	})
	return proto, nil
}

// dialChrome is the transports' dialer: the connection of a probe if one is
// waiting for addr, else a new one.
func (i *impersonator) dialChrome(ctx context.Context, addr string) (net.Conn, error) {
	if c, ok := i.pending.LoadAndDelete(addr); ok {
		if nc, ok := c.(net.Conn); ok {
			return nc, nil
		}
	}
	return i.dialChromeFresh(ctx, addr)
}

// dialChromeFresh connects to addr (through the request's proxy, if any) and
// completes a TLS handshake with Chrome's ClientHello.
func (i *impersonator) dialChromeFresh(ctx context.Context, addr string) (*utls.UConn, error) {
	proxy, _ := ctx.Value(proxyKey{}).(*url.URL)
	var raw net.Conn
	var err error
	if proxy == nil {
		raw, err = i.dial(ctx, addr)
	} else {
		raw, err = i.dialViaProxy(ctx, proxy, addr)
	}
	if err != nil {
		return nil, err
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("netclient: bad address %q: %w", addr, err)
	}
	cfg := &utls.Config{ServerName: host, MinVersion: utls.VersionTLS12}
	if c := i.primary.TLSClientConfig; c != nil {
		cfg.InsecureSkipVerify = c.InsecureSkipVerify //nolint:gosec // the user's tlsVerify=false setting, carried over
		cfg.RootCAs = c.RootCAs
	}
	uc := utls.UClient(raw, cfg, utls.HelloChrome_Auto)
	hctx, cancel := context.WithTimeout(ctx, impersonationHandshakeTimeout)
	defer cancel()
	if err := uc.HandshakeContext(hctx); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("netclient: TLS handshake with %s: %w", host, err)
	}
	return uc, nil
}

func (i *impersonator) dial(ctx context.Context, addr string) (net.Conn, error) {
	var conn net.Conn
	var err error
	if i.primary.DialContext != nil {
		conn, err = i.primary.DialContext(ctx, "tcp", addr)
	} else {
		d := net.Dialer{Timeout: impersonationHandshakeTimeout}
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, fmt.Errorf("netclient: dial %s: %w", addr, err)
	}
	return conn, nil
}

// dialViaProxy opens a CONNECT tunnel to addr through an http or https proxy.
func (i *impersonator) dialViaProxy(ctx context.Context, proxy *url.URL, addr string) (net.Conn, error) {
	paddr := proxy.Host
	if proxy.Port() == "" {
		port := "80"
		if proxy.Scheme == "https" {
			port = "443"
		}
		paddr = net.JoinHostPort(proxy.Hostname(), port)
	}
	conn, err := i.dial(ctx, paddr)
	if err != nil {
		return nil, err
	}
	if proxy.Scheme == "https" {
		tc := tls.Client(conn, &tls.Config{ServerName: proxy.Hostname(), MinVersion: tls.VersionTLS12})
		hctx, cancel := context.WithTimeout(ctx, impersonationHandshakeTimeout)
		defer cancel()
		if err := tc.HandshakeContext(hctx); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("netclient: TLS handshake with the proxy: %w", err)
		}
		conn = tc
	}
	var b strings.Builder
	fmt.Fprintf(&b, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", addr, addr)
	if u := proxy.User; u != nil {
		pw, _ := u.Password()
		fmt.Fprintf(&b, "Proxy-Authorization: Basic %s\r\n", base64.StdEncoding.EncodeToString([]byte(u.Username()+":"+pw)))
	}
	b.WriteString("\r\n")
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(impersonationHandshakeTimeout))
	}
	if _, err := io.WriteString(conn, b.String()); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("netclient: CONNECT through the proxy: %w", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("netclient: CONNECT through the proxy: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("netclient: the proxy refused CONNECT with status %d", resp.StatusCode)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// canonicalAddr is host:port of u, with the default port of its scheme.
func canonicalAddr(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// hostCache remembers hosts for a while. It is bounded: when full, expired
// entries go first, then the oldest.
type hostCache struct {
	ttl   time.Duration
	max   int
	clock func() time.Time

	mu sync.Mutex
	m  map[string]time.Time // host -> expiry
}

func newHostCache(ttl time.Duration, maxHosts int, clock func() time.Time) *hostCache {
	return &hostCache{ttl: ttl, max: maxHosts, clock: clock, m: make(map[string]time.Time)}
}

func (c *hostCache) has(host string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	exp, ok := c.m[host]
	if ok && !c.clock().Before(exp) {
		delete(c.m, host)
		return false
	}
	return ok
}

// add remembers host and reports whether it was not remembered already.
func (c *hostCache) add(host string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock()
	_, had := c.m[host]
	if !had && len(c.m) >= c.max {
		var oldest string
		var oldestExp time.Time
		for h, exp := range c.m {
			if !now.Before(exp) {
				delete(c.m, h)
			} else if oldest == "" || exp.Before(oldestExp) {
				oldest, oldestExp = h, exp
			}
		}
		if len(c.m) >= c.max {
			delete(c.m, oldest)
		}
	}
	c.m[host] = now.Add(c.ttl)
	return !had
}
