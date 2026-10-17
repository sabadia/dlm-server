package domain

import (
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"
)

// Cookie represents a stored HTTP cookie compliant with RFC 6265.
type Cookie struct {
	ID           string     `json:"id"`
	Domain       string     `json:"domain"`
	Name         string     `json:"name"`
	Value        string     `json:"value"`
	Path         string     `json:"path"`
	PartitionKey string     `json:"partitionKey,omitempty"`
	Secure       bool       `json:"secure"`
	HTTPOnly     bool       `json:"httpOnly"`
	SameSite     string     `json:"sameSite,omitempty"`
	HostOnly     bool       `json:"hostOnly"`
	ExpiresAt    *time.Time `json:"expiresAt,omitempty"`
	CreatedAt    time.Time  `json:"createdAt"`
	UpdatedAt    time.Time  `json:"updatedAt"`
}

// NewCookie constructs a Cookie with defaulted path, normalized domain and
// timestamps.
func NewCookie(id, name, value, domain string, now time.Time) (*Cookie, error) {
	c := &Cookie{
		ID:        id,
		Name:      strings.TrimSpace(name),
		Value:     value,
		Domain:    CanonicalDomain(domain),
		Path:      "/",
		CreatedAt: now.UTC().Truncate(time.Millisecond),
		UpdatedAt: now.UTC().Truncate(time.Millisecond),
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// Validate checks cookie fields for correctness.
func (c *Cookie) Validate() error {
	if c.Name == "" {
		return InvalidArgument("cookie: name must not be empty")
	}
	if strings.ContainsAny(c.Name, ";, \t\r\n\x00") {
		return InvalidArgument("cookie: name contains invalid characters")
	}
	if c.Domain == "" {
		return InvalidArgument("cookie: domain must not be empty")
	}
	if strings.ContainsAny(c.Domain, " \t\r\n\x00/:") {
		return InvalidArgument("cookie: domain contains invalid characters")
	}
	if c.Path == "" {
		c.Path = "/"
	} else if !strings.HasPrefix(c.Path, "/") {
		return InvalidArgument("cookie: path must start with '/'")
	}
	if c.SameSite != "" {
		switch strings.ToLower(c.SameSite) {
		case "lax", "strict", "none":
		default:
			return InvalidArgument("cookie: invalid sameSite value %q", c.SameSite)
		}
	}
	return nil
}

// IsExpired reports whether the cookie has expired at the given time.
func (c *Cookie) IsExpired(now time.Time) bool {
	if c.ExpiresAt == nil || c.ExpiresAt.IsZero() {
		return false
	}
	return !now.Before(*c.ExpiresAt)
}

// MatchesRequest checks if the cookie should be sent to the given URL and
// partition key per RFC 6265 Section 5.4.
func (c *Cookie) MatchesRequest(u *url.URL, partitionKey string, now time.Time) bool {
	if c.IsExpired(now) {
		return false
	}
	if c.PartitionKey != "" && c.PartitionKey != partitionKey {
		return false
	}
	host := u.Hostname()
	if !MatchesDomain(c.Domain, host, c.HostOnly) {
		return false
	}
	reqPath := u.Path
	if reqPath == "" {
		reqPath = "/"
	}
	if !PathMatch(c.Path, reqPath) {
		return false
	}
	if c.Secure && u.Scheme != "https" {
		return false
	}
	return true
}

// ToHTTPCookie converts the domain.Cookie to a standard *http.Cookie.
func (c *Cookie) ToHTTPCookie() *http.Cookie {
	hc := &http.Cookie{ //nolint:gosec // mirrors the stored attributes; not a Set-Cookie we issue
		Name:     c.Name,
		Value:    c.Value,
		Path:     c.Path,
		Domain:   c.Domain,
		Secure:   c.Secure,
		HttpOnly: c.HTTPOnly,
	}
	if c.ExpiresAt != nil && !c.ExpiresAt.IsZero() {
		hc.Expires = *c.ExpiresAt
	}
	switch strings.ToLower(c.SameSite) {
	case "lax":
		hc.SameSite = http.SameSiteLaxMode
	case "strict":
		hc.SameSite = http.SameSiteStrictMode
	case "none":
		hc.SameSite = http.SameSiteNoneMode
	default:
		hc.SameSite = http.SameSiteDefaultMode
	}
	return hc
}

// FromHTTPCookie constructs a domain.Cookie from an *http.Cookie and request context.
func FromHTTPCookie(hc *http.Cookie, reqURL *url.URL, partitionKey string, now time.Time) (*Cookie, error) {
	if hc == nil {
		return nil, InvalidArgument("cookie: nil http.Cookie")
	}
	host := reqURL.Hostname()
	cookieDomain := hc.Domain
	hostOnly := false
	if cookieDomain == "" {
		cookieDomain = host
		hostOnly = true
	} else {
		cookieDomain = CanonicalDomain(cookieDomain)
		if IsPublicSuffix(cookieDomain) {
			if strings.EqualFold(cookieDomain, host) {
				hostOnly = true
			} else {
				return nil, InvalidArgument("cookie: domain %q is a public suffix", cookieDomain)
			}
		}
		if !MatchesDomain(cookieDomain, host, false) {
			return nil, InvalidArgument("cookie: domain %q does not match request host %q", cookieDomain, host)
		}
	}

	path := hc.Path
	if path == "" || !strings.HasPrefix(path, "/") {
		path = DefaultPath(reqURL.Path)
	}

	var expiresAt *time.Time
	switch {
	case hc.MaxAge > 0:
		exp := now.Add(time.Duration(hc.MaxAge) * time.Second).UTC().Truncate(time.Millisecond)
		expiresAt = &exp
	case hc.MaxAge < 0:
		exp := now.Add(-time.Hour).UTC().Truncate(time.Millisecond)
		expiresAt = &exp
	case !hc.Expires.IsZero():
		exp := hc.Expires.UTC().Truncate(time.Millisecond)
		expiresAt = &exp
	}

	sameSite := ""
	switch hc.SameSite {
	case http.SameSiteLaxMode:
		sameSite = "lax"
	case http.SameSiteStrictMode:
		sameSite = "strict"
	case http.SameSiteNoneMode:
		sameSite = "none"
	case http.SameSiteDefaultMode:
		// the attribute was present without a usable value: unset
	}

	c := &Cookie{
		ID:           NewID(),
		Domain:       cookieDomain,
		Name:         hc.Name,
		Value:        hc.Value,
		Path:         path,
		PartitionKey: partitionKey,
		Secure:       hc.Secure,
		HTTPOnly:     hc.HttpOnly,
		SameSite:     sameSite,
		HostOnly:     hostOnly,
		ExpiresAt:    expiresAt,
		CreatedAt:    now.UTC().Truncate(time.Millisecond),
		UpdatedAt:    now.UTC().Truncate(time.Millisecond),
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// CanonicalDomain strips leading dots and lowercases the domain.
func CanonicalDomain(domain string) string {
	domain = strings.TrimPrefix(domain, ".")
	return strings.ToLower(domain)
}

// IsPublicSuffix reports whether domain is on the Public Suffix List.
func IsPublicSuffix(domain string) bool {
	domain = CanonicalDomain(domain)
	ps, _ := publicsuffix.PublicSuffix(domain)
	return domain == ps
}

// MatchesDomain reports whether host domain-matches cookieDomain per RFC 6265 Section 5.1.3.
func MatchesDomain(cookieDomain, host string, hostOnly bool) bool {
	cookieDomain = CanonicalDomain(cookieDomain)
	host = CanonicalDomain(host)
	if cookieDomain == host {
		return true
	}
	if hostOnly {
		return false
	}
	// IP addresses must match exactly.
	if net.ParseIP(host) != nil || net.ParseIP(cookieDomain) != nil {
		return false
	}
	if strings.HasSuffix(host, "."+cookieDomain) {
		return true
	}
	return false
}

// PathMatch reports whether reqPath path-matches cookiePath per RFC 6265 Section 5.1.4.
func PathMatch(cookiePath, reqPath string) bool {
	if cookiePath == "" {
		cookiePath = "/"
	}
	if reqPath == "" {
		reqPath = "/"
	}
	if cookiePath == reqPath {
		return true
	}
	if strings.HasPrefix(reqPath, cookiePath) {
		if strings.HasSuffix(cookiePath, "/") {
			return true
		}
		if len(reqPath) > len(cookiePath) && reqPath[len(cookiePath)] == '/' {
			return true
		}
	}
	return false
}

// DefaultPath computes the default-path from a request URL path per RFC 6265 Section 5.1.4.
func DefaultPath(reqPath string) string {
	if reqPath == "" || !strings.HasPrefix(reqPath, "/") {
		return "/"
	}
	lastSlash := strings.LastIndex(reqPath, "/")
	if lastSlash <= 0 {
		return "/"
	}
	return reqPath[:lastSlash]
}
