package netclient

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"

	"github.com/sabadia/dlm-server/internal/callctx"
)

// ErrLocalAddress is returned when work that a browser extension started tries
// to connect to the machine the daemon runs on.
var ErrLocalAddress = callctx.ErrLocalAddress

// TaskCookieSource supplies the cookies kept for one task only.
type TaskCookieSource interface {
	TaskCookies(ctx context.Context, taskID string, u *url.URL) []*http.Cookie
}

// callerTransport applies what depends on who a request is made for: it adds
// the cookies kept for the calling task (the shared jar never serves them), and
// notes the route of a restricted request so the dialer can tell a connection
// to a proxy from one to the server.
type callerTransport struct {
	next    http.RoundTripper
	cookies TaskCookieSource
	// resolver looks up the addresses of a restricted request's host.
	resolver Resolver
	// proxied reports whether a request is sent through a proxy; nil: never.
	proxied func(*http.Request) bool
}

func (t *callerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	c := callctx.From(ctx)
	if !c.Restricted && (c.TaskID == "" || t.cookies == nil) {
		return t.next.RoundTrip(req) //nolint:wrapcheck // a transport passes errors through
	}
	if c.Restricted {
		ctx, _ = callctx.WithRoute(ctx)
		// An idle connection to an address that is not allowed may be in the
		// pool (an ordinary download opened it): checking only when dialling
		// would let this request use it. Every hop of a redirect comes here.
		if err := t.refuseLocalTarget(ctx, req); err != nil {
			return nil, err
		}
	}
	req = req.Clone(ctx) // a RoundTripper must not change the request it is given
	if c.TaskID != "" && t.cookies != nil {
		if own := t.cookies.TaskCookies(ctx, c.TaskID, req.URL); len(own) > 0 {
			mergeCookies(req, own)
		}
	}
	return t.next.RoundTrip(req) //nolint:wrapcheck // a transport passes errors through
}

// refuseLocalTarget fails a request whose host is, or resolves to, an address
// of the machine the daemon runs on. A request through a proxy is not checked:
// the proxy resolves and connects.
func (t *callerTransport) refuseLocalTarget(ctx context.Context, req *http.Request) error {
	if t.proxied != nil && t.proxied(req) {
		return nil
	}
	host := strings.ToLower(strings.TrimSuffix(req.URL.Hostname(), "."))
	if host == "" {
		return nil
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return ErrLocalAddress
	}
	if ip := net.ParseIP(host); ip != nil {
		if Local(ip) {
			return ErrLocalAddress
		}
		return nil
	}
	if t.resolver == nil {
		return nil
	}
	ips, err := t.resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil //nolint:nilerr // an unresolvable name fails by itself when the connection is made
	}
	for _, a := range ips {
		if Local(a.IP) {
			return ErrLocalAddress
		}
	}
	return nil
}

// CloseIdleConnections lets the factory close what the wrapped transport holds.
func (t *callerTransport) CloseIdleConnections() {
	if c, ok := t.next.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// mergeCookies puts the task's cookies first in req's Cookie header; a cookie
// of the same name that is already there (from the shared jar) is dropped, so
// the task's value is the only one the server sees.
func mergeCookies(req *http.Request, own []*http.Cookie) {
	names := make(map[string]struct{}, len(own))
	parts := make([]string, 0, len(own)+4)
	for _, c := range own {
		names[c.Name] = struct{}{}
		parts = append(parts, cookiePair(c))
	}
	for _, c := range req.Cookies() {
		if _, taken := names[c.Name]; !taken {
			parts = append(parts, cookiePair(c))
		}
	}
	req.Header.Set("Cookie", strings.Join(parts, "; "))
}

// cookiePair is how a cookie is written in a request's Cookie header: name and
// value only (the attributes belong to Set-Cookie).
func cookiePair(c *http.Cookie) string {
	return (&http.Cookie{Name: c.Name, Value: c.Value}).String() //nolint:gosec // request-side cookie: only name and value are sent
}

// Local reports whether ip is an address of the machine the daemon runs on, or
// one that reaches services on it or its network edge (cloud metadata):
// loopback, link-local and unspecified addresses. Addresses of the local
// network are not: a browser reaches them too.
func Local(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()
}

// refuseLocal is a net.Dialer Control: it runs with the address about to be
// connected, after the name was resolved, so a name that resolves to a local
// address (or changes to one between two lookups) is refused too.
func refuseLocal(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return ErrLocalAddress
	}
	if ip := net.ParseIP(host); ip == nil || Local(ip) {
		return ErrLocalAddress
	}
	return nil
}
