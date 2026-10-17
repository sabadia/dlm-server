package netclient

import (
	"log/slog"
	"net/http"
	"slices"
	"strings"
)

// isForbidden reports headers never taken from the settings: they are
// controlled by net/http or describe a single connection.
func isForbidden(canon string) bool {
	switch canon {
	case "Host", "Content-Length", "Connection", "Keep-Alive", "Proxy-Connection",
		"Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer",
		"Transfer-Encoding", "Upgrade":
		return true
	}
	return false
}

// isSensitive reports headers only sent while a redirect chain stays on the
// initial host or its subdomains (the rule net/http applies to headers set
// on the request).
func isSensitive(canon string) bool {
	switch canon {
	case "Authorization", "Www-Authenticate", "Cookie", "Cookie2":
		return true
	}
	return false
}

// headerTransport adds default headers that a request does not set itself.
type headerTransport struct {
	base    http.RoundTripper
	headers http.Header // canonical names, one value each
}

func newHeaderTransport(base http.RoundTripper, userAgent string, extra map[string]string, log *slog.Logger) *headerTransport {
	h := make(http.Header, len(extra)+1)
	if userAgent != "" {
		h.Set("User-Agent", userAgent)
	}
	for name, value := range extra {
		canon := http.CanonicalHeaderKey(strings.TrimSpace(name))
		if canon == "" || isForbidden(canon) {
			log.Debug("netclient: ignoring forbidden header from settings", "header", canon)
			continue
		}
		h.Set(canon, value) // overrides UserAgent: Headers entries win
	}
	return &headerTransport{base: base, headers: h}
}

// RoundTrip implements http.RoundTripper. It never mutates req: when
// headers must be added it sends a clone.
func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var missing []string
	leftDomain := false
	checked := false
	for name := range t.headers {
		if _, ok := req.Header[name]; ok {
			continue
		}
		if isSensitive(name) {
			if !checked {
				leftDomain, checked = redirectLeftDomain(req), true
			}
			if leftDomain {
				continue
			}
		}
		missing = append(missing, name)
	}
	if len(missing) == 0 {
		return t.base.RoundTrip(req) //nolint:wrapcheck // transport errors pass through unchanged
	}
	r := req.Clone(req.Context())
	for _, name := range missing {
		r.Header[name] = slices.Clone(t.headers[name]) // never share our slices
	}
	return t.base.RoundTrip(r) //nolint:wrapcheck // transport errors pass through unchanged
}

// CloseIdleConnections forwards to the wrapped transport so
// http.Client.CloseIdleConnections works.
func (t *headerTransport) CloseIdleConnections() {
	if c, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// redirectLeftDomain reports whether any hop of req's redirect chain went
// to a host that is neither the initial host nor one of its subdomains.
func redirectLeftDomain(req *http.Request) bool {
	var hosts []string
	for r := req; r != nil; {
		hosts = append(hosts, strings.ToLower(r.URL.Hostname()))
		if r.Response == nil {
			break
		}
		r = r.Response.Request
	}
	initial := hosts[len(hosts)-1]
	for _, h := range hosts[:len(hosts)-1] {
		if !isDomainOrSubdomain(h, initial) {
			return true
		}
	}
	return false
}

// isDomainOrSubdomain reports whether sub equals parent or is a subdomain
// of it. Both must be lower case.
func isDomainOrSubdomain(sub, parent string) bool {
	if sub == parent {
		return true
	}
	// IPv6 literals and zones are never subdomains.
	if strings.ContainsAny(sub, ":%") {
		return false
	}
	return strings.HasSuffix(sub, "."+parent)
}
