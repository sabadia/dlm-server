package httpx

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
)

// mirrorStrikes is how many failed requests take a mirror out for the rest of
// the run. The primary link is never taken out.
const mirrorStrikes = 2

// mirrorSet spreads the requests of a ranged download over the primary link
// and the mirrors that were verified to serve the same file.
type mirrorSet struct {
	mu      sync.Mutex
	urls    []string
	strikes map[string]int
	next    int
}

func newMirrorSet(urls []string) *mirrorSet {
	return &mirrorSet{urls: urls, strikes: make(map[string]int)}
}

// pick returns the URL for the next request, rotating through the primary and
// the mirrors still in; mirror reports whether it is not the primary.
func (m *mirrorSet) pick(primary string) (u string, mirror bool) {
	if m == nil {
		return primary, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cands := make([]string, 0, len(m.urls)+1)
	cands = append(cands, primary)
	for _, u := range m.urls {
		if m.strikes[u] < mirrorStrikes {
			cands = append(cands, u)
		}
	}
	m.next++
	u = cands[m.next%len(cands)]
	return u, u != primary
}

// fail records a failed request to a mirror.
func (m *mirrorSet) fail(u string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.strikes[u]++
	m.mu.Unlock()
}

// verifyMirrors probes every mirror of the source and keeps the ones that
// serve a file of the same size with range support; the others are logged and
// left out, so a wrong or dead mirror never costs the download.
func (d *Download) verifyMirrors(ctx context.Context) {
	src := d.source()
	var ok []string
	seen := map[string]bool{src.CurrentURL: true, src.RedirectURL: true}
	for _, m := range src.Mirrors {
		if seen[m] {
			continue
		}
		seen[m] = true
		if err := ctx.Err(); err != nil {
			return
		}
		pr, err := Probe(ctx, d.client, d.mirrorSource(src, m), d.spec.Settings)
		switch {
		case err != nil:
			d.log.Warn("httpx: mirror ignored: probe failed", "mirror", redact(m), "error", err)
		case !pr.Resource.RangeSupported || pr.Resource.Size != d.res.Size:
			d.log.Warn("httpx: mirror ignored: not the same file or no range support", "mirror", redact(m),
				"size", pr.Resource.Size, "want", d.res.Size, "ranges", pr.Resource.RangeSupported)
		default:
			ok = append(ok, m)
		}
		_ = pr.Close()
	}
	if len(ok) > 0 {
		d.mirrors = newMirrorSet(ok)
		d.log.Info("httpx: using mirrors", "count", len(ok))
	}
}

// mirrorSource is the source to request mirror m with. Headers and a body
// captured for the primary host are not sent to another host: they may carry
// that site's cookies or credentials.
func (d *Download) mirrorSource(src domain.Source, m string) domain.Source {
	out := src
	out.CurrentURL, out.RedirectURL = m, ""
	if !sameHost(src.CurrentURL, m) {
		out.Headers, out.Body = nil, nil
	}
	return out
}

func sameHost(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	return errA == nil && errB == nil && strings.EqualFold(ua.Host, ub.Host)
}

// redact drops the credentials and query of a URL for logging.
func redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(invalid URL)"
	}
	u.User, u.RawQuery = nil, ""
	return u.String()
}

// mirrorErr turns a failure of a request to a mirror into a transient error:
// a bad mirror must make the connection try another URL, not expire the link
// or fail the task. Cancellation passes through.
func (d *Download) mirrorErr(ctx context.Context, u string, err error) error {
	d.mirrors.fail(u)
	if ctx.Err() != nil {
		return ctx.Err() //nolint:wrapcheck // cancellation is reported as is
	}
	return engine.Transient(domain.ErrorCodeNetwork, fmt.Errorf("httpx: mirror %s: %w", redact(u), err))
}
