package browser

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/storage"
	"github.com/chromedp/chromedp"

	"github.com/sabadia/dlm-server/internal/domain"
)

// CookieStore is the persistent cookie store the browser exchanges cookies
// with (cookies.Store).
type CookieStore interface {
	// ForSite returns the cookies of a registrable domain and its subdomains,
	// values decrypted.
	ForSite(ctx context.Context, site string) ([]*domain.Cookie, error)
	// Put stores a cookie, replacing the one with the same identity.
	Put(ctx context.Context, c *domain.Cookie) error
}

// PushCookies loads the stored cookies of a site (a registrable domain such as
// example.com, with its subdomains) into the browser of the tab of ctx, so a
// page opens with the user's login. It returns how many were loaded.
func PushCookies(ctx, tab context.Context, store CookieStore, site string) (int, error) {
	cs, err := store.ForSite(ctx, site)
	if err != nil {
		return 0, fmt.Errorf("browser: load cookies of %q: %w", site, err)
	}
	return InjectCookies(tab, cs, time.Now())
}

// PullCookies copies the cookies the browser holds back to the store, so
// engines and later sessions share what the page set (a refreshed session, a
// consent cookie). Only cookies whose domain keep accepts are copied: a page
// also collects third-party cookies that the store should not keep. It returns
// how many were stored; cookies the store rejects are skipped.
func PullCookies(ctx, tab context.Context, store CookieStore, keep func(domain string) bool) (int, error) {
	cs, err := ExtractCookies(tab, keep)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, c := range cs {
		if err := store.Put(ctx, c); err != nil {
			continue // an invalid cookie of the browser must not stop the rest
		}
		n++
	}
	return n, nil
}

// InjectCookies sets cookies in the browser of the tab. Expired cookies are
// skipped. It returns how many were set.
func InjectCookies(tab context.Context, cookies []*domain.Cookie, now time.Time) (int, error) {
	params := make([]*network.CookieParam, 0, len(cookies))
	for _, c := range cookies {
		if c.IsExpired(now) {
			continue
		}
		params = append(params, toCDP(c))
	}
	if len(params) == 0 {
		return 0, nil
	}
	if _, err := chromedp.Call(tab, storage.SetCookies, storage.SetCookiesParams{Cookies: params}); err != nil {
		return 0, fmt.Errorf("browser: set cookies: %w", err)
	}
	return len(params), nil
}

// ExtractCookies reads the cookies of the browser of the tab whose domain
// keep accepts (nil keeps all).
func ExtractCookies(tab context.Context, keep func(domain string) bool) ([]*domain.Cookie, error) {
	res, err := chromedp.Call(tab, storage.GetCookies, storage.GetCookiesParams{})
	if err != nil {
		return nil, fmt.Errorf("browser: get cookies: %w", err)
	}
	var out []*domain.Cookie
	for _, c := range res.Cookies {
		dc := fromCDP(c)
		if dc == nil || (keep != nil && !keep(dc.Domain)) {
			continue
		}
		out = append(out, dc)
	}
	return out, nil
}

// toCDP converts a stored cookie. Chrome treats a domain with a leading dot as
// a domain cookie and one without as host-only.
func toCDP(c *domain.Cookie) *network.CookieParam {
	p := &network.CookieParam{
		Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path,
		Secure: c.Secure, HTTPOnly: c.HTTPOnly,
	}
	if !c.HostOnly {
		p.Domain = "." + c.Domain
	}
	switch strings.ToLower(c.SameSite) {
	case "lax":
		p.SameSite = network.CookieSameSiteLax
	case "strict":
		p.SameSite = network.CookieSameSiteStrict
	case "none":
		p.SameSite = network.CookieSameSiteNone
	}
	if c.ExpiresAt != nil && !c.ExpiresAt.IsZero() {
		p.Expires = cdp.TimeSinceEpoch(float64(c.ExpiresAt.UnixMilli()) / 1000)
	}
	if c.PartitionKey != "" {
		p.PartitionKey = &network.CookiePartitionKey{TopLevelSite: c.PartitionKey}
	}
	return p
}

// fromCDP converts a browser cookie, or returns nil for one that cannot be
// stored (an opaque partition key).
func fromCDP(c *network.Cookie) *domain.Cookie {
	if c.PartitionKeyOpaque {
		return nil
	}
	dc := &domain.Cookie{
		Domain: domain.CanonicalDomain(c.Domain), Name: c.Name, Value: c.Value, Path: c.Path,
		Secure: c.Secure, HTTPOnly: c.HTTPOnly, HostOnly: !strings.HasPrefix(c.Domain, "."),
		SameSite: strings.ToLower(string(c.SameSite)),
	}
	if !c.Session && c.Expires > 0 && !math.IsInf(c.Expires, 0) && c.Expires < 253402300799 {
		sec, frac := math.Modf(c.Expires)
		t := time.Unix(int64(sec), int64(frac*1e9)).UTC().Truncate(time.Millisecond)
		dc.ExpiresAt = &t
	}
	if c.PartitionKey != nil {
		dc.PartitionKey = c.PartitionKey.TopLevelSite
	}
	return dc
}
