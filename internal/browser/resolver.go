package browser

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/chromedp"

	"github.com/sabadia/dlm-server/internal/domain"
)

// Defaults for ResolverOptions fields left zero.
const (
	DefaultResolveWait  = 20 * time.Second
	DefaultResolveQuiet = 1500 * time.Millisecond
	DefaultResultTTL    = 30 * time.Minute
)

// ErrResultGone is returned for a resolve result that expired or never
// existed.
var ErrResultGone = errors.New("browser: the resolve result expired; resolve the page again")

// ResolveOptions tunes ResolvePage.
type ResolveOptions struct {
	// Wait bounds how long the page may keep producing candidates.
	Wait time.Duration
	// Quiet is how long nothing new has to appear before the page counts as
	// settled.
	Quiet time.Duration
}

// PageResult is what opening a page found.
type PageResult struct {
	// ID names the result for Candidate, Release and the download calls.
	ID string `json:"id"`
	// URL is the page that was asked for, FinalURL where it ended up.
	URL      string `json:"url"`
	FinalURL string `json:"finalUrl"`
	Title    string `json:"title"`
	// Group is the profile (site group) the page ran in.
	Group      string      `json:"group"`
	Candidates []Candidate `json:"candidates"`
	ExpiresAt  time.Time   `json:"expiresAt"`
}

// ResolverOptions configures a Resolver.
type ResolverOptions struct {
	// Manager supplies the browsers; required.
	Manager *Manager
	// Cookies is the persistent cookie store synced with the browser (before
	// the page loads and after it settled); nil disables the sync.
	Cookies CookieStore
	// Dir stages the files the browser downloads (blobs and attachments)
	// until they are imported or the result expires; required.
	Dir string
	// TTL is how long a result and its staged files are kept.
	TTL    time.Duration
	Logger *slog.Logger
	// Clock defaults to time.Now.
	Clock func() time.Time
}

// Resolver opens pages in the headless browser and reports the downloads they
// lead to, keeping the files the browser downloaded itself for a while.
type Resolver struct {
	opts ResolverOptions
	log  *slog.Logger

	mu      sync.Mutex
	results map[string]*stored
	groups  map[string]*sync.Mutex
}

type stored struct {
	result  PageResult
	dir     string
	expires time.Time
}

// NewResolver creates a Resolver.
func NewResolver(opts ResolverOptions) *Resolver {
	if opts.TTL <= 0 {
		opts.TTL = DefaultResultTTL
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Resolver{opts: opts, log: log, results: map[string]*stored{}, groups: map[string]*sync.Mutex{}}
}

// Status reports the browser in use and the running pool.
func (r *Resolver) Status() Status { return r.opts.Manager.Status() }

// Profiles lists the site groups that have a browser profile.
func (r *Resolver) Profiles() ([]string, error) { return r.opts.Manager.Profiles() }

// DeleteProfile removes the profile of a site group (see Manager.DeleteProfile).
func (r *Resolver) DeleteProfile(group string) error { return r.opts.Manager.DeleteProfile(group) }

// ResolvePage opens pageURL in the browser of its site group with the stored
// cookies of the site, waits until the page settled and returns the downloads
// it led to. The cookies the page set are written back to the store. Only one
// page per site group runs at a time, because the browser's download behaviour
// is shared by its tabs.
func (r *Resolver) ResolvePage(ctx context.Context, pageURL string, opts ResolveOptions) (*PageResult, error) {
	u, err := url.Parse(pageURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, domain.InvalidArgument("browser: %q is not an http(s) URL", pageURL)
	}
	if opts.Wait <= 0 {
		opts.Wait = DefaultResolveWait
	}
	if opts.Quiet <= 0 {
		opts.Quiet = DefaultResolveQuiet
	}
	group := SiteGroup(pageURL)
	unlock := r.lockGroup(group)
	defer unlock()

	id, err := newID()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(r.opts.Dir, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("browser: stage directory: %w", err)
	}
	keepDir := false
	defer func() {
		if !keepDir {
			_ = os.RemoveAll(dir) // best effort: nothing references it
		}
	}()

	sess, err := r.opts.Manager.Acquire(ctx, group)
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	site := SiteOf(pageURL)
	if r.opts.Cookies != nil {
		if _, err := PushCookies(ctx, sess.Ctx, r.opts.Cookies, site); err != nil {
			r.log.Warn("browser: cannot load stored cookies", "site", site, "error", err)
		}
	}
	sn, err := StartSniffer(sess.Ctx, SnifferOptions{DownloadDir: dir}) //nolint:contextcheck // the tab context derives from the browser on purpose
	if err != nil {
		return nil, err
	}
	defer sn.Close()

	navCtx, cancelNav := context.WithTimeout(sess.Ctx, opts.Wait)
	navErr := chromedp.Do(navCtx, chromedp.Navigate(pageURL)) //nolint:contextcheck // the tab context derives from the browser on purpose
	cancelNav()
	cands := sn.Wait(ctx, opts.Quiet, opts.Wait)
	aborted := navErr != nil && len(cands) > 0 && strings.Contains(navErr.Error(), "ERR_ABORTED")
	if navErr != nil && !aborted {
		// ERR_ABORTED is what a navigation to a download looks like; anything
		// else with nothing found is a failure to load the page.
		if len(cands) == 0 || ctx.Err() != nil {
			return nil, fmt.Errorf("browser: open %s: %w", pageURL, navErr)
		}
	}
	result := PageResult{URL: pageURL, FinalURL: pageURL, Group: group, ID: id}
	if title, err := chromedp.Run(sess.Ctx, chromedp.Title()); err == nil { //nolint:contextcheck // the tab context derives from the browser on purpose
		result.Title = title
	}
	if loc, err := chromedp.Run(sess.Ctx, chromedp.Location()); err == nil && loc != "" { //nolint:contextcheck // the tab context derives from the browser on purpose
		result.FinalURL = loc
	}
	if r.opts.Cookies != nil {
		hosts := candidateHosts(cands)
		keep := func(d string) bool { return inSite(d, site) || hosts[d] || hostsMatch(hosts, d) }
		if _, err := PullCookies(ctx, sess.Ctx, r.opts.Cookies, keep); err != nil {
			r.log.Warn("browser: cannot store the cookies of the page", "site", site, "error", err)
		}
	}
	for i := range cands {
		cands[i].Index = i
		cands[i].Page = result.FinalURL // the page to open again to find the download
	}
	result.Candidates = cands
	result.ExpiresAt = r.opts.Clock().Add(r.opts.TTL).UTC()
	r.mu.Lock()
	r.results[id] = &stored{result: result, dir: dir, expires: result.ExpiresAt}
	r.mu.Unlock()
	keepDir = true
	return clonePage(result), nil
}

// Result returns a stored result, or ErrResultGone.
func (r *Resolver) Result(id string) (*PageResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := r.results[id]
	if st == nil || !st.expires.After(r.opts.Clock()) {
		return nil, ErrResultGone
	}
	return clonePage(st.result), nil
}

// Candidate returns candidate index of a stored result.
func (r *Resolver) Candidate(id string, index int) (Candidate, error) {
	res, err := r.Result(id)
	if err != nil {
		return Candidate{}, err
	}
	if index < 0 || index >= len(res.Candidates) {
		return Candidate{}, domain.NotFound("candidate", fmt.Sprintf("%s/%d", id, index))
	}
	return res.Candidates[index], nil
}

// Release deletes a result and the files staged for it; unknown IDs are
// ignored.
func (r *Resolver) Release(id string) {
	r.mu.Lock()
	st := r.results[id]
	delete(r.results, id)
	r.mu.Unlock()
	if st != nil {
		_ = os.RemoveAll(st.dir) // best effort: staged files of a finished result
	}
}

// Run removes expired results until ctx ends, and clears staged directories
// left over from an earlier run.
func (r *Resolver) Run(ctx context.Context) error {
	r.clearStale()
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		r.sweep()
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (r *Resolver) sweep() {
	now := r.opts.Clock()
	r.mu.Lock()
	var gone []string
	for id, st := range r.results {
		if !st.expires.After(now) {
			gone = append(gone, id)
		}
	}
	r.mu.Unlock()
	for _, id := range gone {
		r.Release(id)
	}
}

// clearStale removes staged directories no result references (left by a
// previous daemon).
func (r *Resolver) clearStale() {
	entries, err := os.ReadDir(r.opts.Dir)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range entries {
		if e.IsDir() && r.results[e.Name()] == nil {
			_ = os.RemoveAll(filepath.Join(r.opts.Dir, e.Name())) // best effort: unreferenced
		}
	}
}

func (r *Resolver) lockGroup(group string) (unlock func()) {
	r.mu.Lock()
	m := r.groups[group]
	if m == nil {
		m = &sync.Mutex{}
		r.groups[group] = m
	}
	r.mu.Unlock()
	m.Lock()
	return m.Unlock
}

func clonePage(p PageResult) *PageResult {
	out := p
	out.Candidates = make([]Candidate, len(p.Candidates))
	for i, c := range p.Candidates {
		out.Candidates[i] = c
		out.Candidates[i].Headers = cloneMap(c.Headers)
	}
	return &out
}

func newID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("browser: generate id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// inSite reports whether a cookie domain belongs to site.
func inSite(cookieDomain, site string) bool {
	return cookieDomain == site || strings.HasSuffix(cookieDomain, "."+site)
}

// candidateHosts lists the hosts of the candidates' URLs.
func candidateHosts(cands []Candidate) map[string]bool {
	hosts := map[string]bool{}
	for _, c := range cands {
		if u, err := url.Parse(c.URL); err == nil && u.Hostname() != "" && u.Scheme != "blob" {
			hosts[strings.ToLower(u.Hostname())] = true
		}
	}
	return hosts
}

// hostsMatch reports whether a cookie domain is a parent domain of one of the
// hosts (a cookie for example.com is sent to cdn.example.com).
func hostsMatch(hosts map[string]bool, cookieDomain string) bool {
	for h := range hosts {
		if h == cookieDomain || strings.HasSuffix(h, "."+cookieDomain) {
			return true
		}
	}
	return false
}
