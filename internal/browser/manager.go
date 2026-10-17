package browser

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/chromedp"
	"golang.org/x/net/publicsuffix"

	"github.com/sabadia/dlm-server/internal/domain"
)

// Defaults for Options fields left zero.
const (
	DefaultMaxBrowsers = 2
	DefaultIdleTimeout = 2 * time.Minute

	startTimeout = 30 * time.Second
	// closeTimeout bounds the graceful shutdown of a browser.
	closeTimeout = 10 * time.Second
)

// ErrClosed is returned by Acquire after Close.
var ErrClosed = errors.New("browser: manager is closed")

// Options configures a Manager. Zero values select the defaults.
type Options struct {
	// Path is the browser executable to use. When empty the manager uses the
	// first discovered browser, then a cached chrome-headless-shell, then (with
	// AutoDownload) a freshly downloaded one.
	Path string
	// CacheDir holds the downloaded chrome-headless-shell.
	CacheDir string
	// ProfilesDir holds one isolated profile directory per site group. It is
	// required.
	ProfilesDir string
	// AutoDownload lets the manager download chrome-headless-shell when no
	// browser is installed.
	AutoDownload bool
	// Fetcher overrides the downloader (tests); default Fetcher{Dir: CacheDir}.
	Fetcher *Fetcher
	// MaxBrowsers bounds the browser processes running at once.
	MaxBrowsers int
	// IdleTimeout is how long a browser without sessions stays alive.
	IdleTimeout time.Duration
	// NoSandbox starts browsers with --no-sandbox, needed in containers that
	// run as root.
	NoSandbox bool
	// Discover overrides discovery (tests); default Discover.
	Discover func() []Installation
	Logger   *slog.Logger
}

// Manager runs headless browsers on demand. Each site group (see SiteGroup)
// has its own profile, hence its own cookies and storage, and at most one
// browser process; the number of processes is bounded and idle ones are shut
// down. It is safe for concurrent use.
type Manager struct {
	opts Options
	log  *slog.Logger

	resolveMu sync.Mutex
	install   *Installation

	mu        sync.Mutex
	closed    bool
	instances map[string]*instance
	changed   chan struct{} // closed and replaced whenever an instance is released or removed
}

// instance is one running browser process with the profile of one group.
type instance struct {
	group    string
	ready    chan struct{} // closed when the browser started (or failed: err)
	err      error
	ctx      context.Context
	cancel   context.CancelFunc
	refs     int
	lastUsed time.Time
	idle     *time.Timer
}

// NewManager creates a Manager. It starts no browser until Acquire.
func NewManager(opts Options) *Manager {
	if opts.MaxBrowsers <= 0 {
		opts.MaxBrowsers = DefaultMaxBrowsers
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = DefaultIdleTimeout
	}
	if opts.Discover == nil {
		opts.Discover = Discover
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Manager{opts: opts, log: log, instances: make(map[string]*instance), changed: make(chan struct{})}
}

// Resolve returns the browser the manager uses, finding or downloading it on
// the first call.
func (m *Manager) Resolve(ctx context.Context) (Installation, error) {
	m.resolveMu.Lock()
	defer m.resolveMu.Unlock()
	if m.install != nil {
		return *m.install, nil
	}
	inst, err := m.find(ctx)
	if err != nil {
		return Installation{}, err
	}
	m.install = &inst
	m.log.Info("browser: using", "name", inst.Name, "path", inst.Path, "version", inst.Version)
	return inst, nil
}

func (m *Manager) fetcher() *Fetcher {
	if m.opts.Fetcher != nil {
		return m.opts.Fetcher
	}
	return &Fetcher{Dir: m.opts.CacheDir}
}

func (m *Manager) find(ctx context.Context) (Installation, error) {
	if m.opts.Path != "" {
		return Validate(m.opts.Path)
	}
	if found := m.opts.Discover(); len(found) > 0 {
		return found[0], nil
	}
	f := m.fetcher()
	if f.Dir == "" {
		return Installation{}, fmt.Errorf("%w: install Chrome, Chromium, Edge or Brave, or set the browser path", ErrNoBrowser)
	}
	if cached, ok := f.Cached(); ok {
		return cached, nil
	}
	if !m.opts.AutoDownload {
		return Installation{}, fmt.Errorf("%w: install Chrome, Chromium, Edge or Brave, set the browser path, or allow downloading chrome-headless-shell", ErrNoBrowser)
	}
	m.log.Info("browser: downloading chrome-headless-shell")
	inst, err := f.Fetch(ctx)
	if err != nil {
		return Installation{}, fmt.Errorf("%w: %w", ErrNoBrowser, err)
	}
	return inst, nil
}

// noop is an action that does nothing; running it starts the browser or tab
// of a chromedp context.
func noop(context.Context, *chromedp.Target) (chromedp.Void, error) { return chromedp.Void{}, nil }

// Session is a browser tab leased from the Manager. Ctx is a chromedp context
// for it; Close closes the tab and returns the browser to the pool.
type Session struct {
	// Ctx drives the tab with chromedp.Run. It ends when the session is
	// closed or the browser dies.
	Ctx   context.Context
	Group string

	cancel  context.CancelFunc
	release func()
	once    sync.Once
}

// Close closes the tab and releases the browser; it is idempotent.
func (s *Session) Close() {
	s.once.Do(func() {
		s.cancel()
		s.release()
	})
}

// Acquire opens a new tab in the browser of the given site group, starting
// the browser if needed. When MaxBrowsers browsers are running, an idle one
// of another group is stopped; when all are in use Acquire waits for one
// (or ctx).
func (m *Manager) Acquire(ctx context.Context, group string) (*Session, error) {
	group = sanitizeGroup(group)
	inst, err := m.lease(ctx, group)
	if err != nil {
		return nil, err
	}
	tabCtx, cancel := chromedp.NewContext(inst.ctx)
	// Opening the tab now surfaces a dead browser here, not in the caller.
	if err := chromedp.Do(tabCtx, noop); err != nil { //nolint:contextcheck // tabCtx derives from the browser context on purpose: a tab outlives the request
		cancel()
		m.unlease(inst)
		return nil, fmt.Errorf("browser: open tab: %w", err)
	}
	return &Session{Ctx: tabCtx, Group: group, cancel: cancel, release: func() { m.unlease(inst) }}, nil
}

// lease reserves the browser of group (starting it) and counts a user.
func (m *Manager) lease(ctx context.Context, group string) (*instance, error) {
	for {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return nil, ErrClosed
		}
		if inst := m.instances[group]; inst != nil {
			if inst.dead() {
				m.removeLocked(inst)
				m.mu.Unlock()
				continue
			}
			inst.refs++
			if inst.idle != nil {
				inst.idle.Stop()
			}
			m.mu.Unlock()
			select {
			case <-inst.ready:
			case <-ctx.Done():
				m.unlease(inst)
				return nil, fmt.Errorf("browser: %w", ctx.Err())
			}
			if inst.err != nil {
				m.unlease(inst)
				return nil, inst.err
			}
			return inst, nil
		}
		if len(m.instances) < m.opts.MaxBrowsers {
			inst := &instance{group: group, ready: make(chan struct{}), refs: 1}
			m.instances[group] = inst
			m.mu.Unlock()
			m.start(ctx, inst)
			if inst.err != nil {
				m.mu.Lock()
				m.removeLocked(inst)
				m.mu.Unlock()
				return nil, inst.err
			}
			return inst, nil
		}
		if victim := m.idlestLocked(); victim != nil {
			m.removeLocked(victim)
			m.mu.Unlock()
			victim.stop()
			continue
		}
		changed := m.changed
		m.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, fmt.Errorf("browser: waiting for a free browser: %w", ctx.Err())
		}
	}
}

// start launches the browser of inst and publishes the result through
// inst.ready.
func (m *Manager) start(ctx context.Context, inst *instance) {
	defer close(inst.ready)
	exe, err := m.Resolve(ctx)
	if err != nil {
		inst.err = err
		return
	}
	dir, err := m.profileDir(inst.group)
	if err != nil {
		inst.err = err
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		inst.err = fmt.Errorf("browser: create profile: %w", err)
		return
	}
	opts := append(slices.Clone(chromedp.DefaultExecAllocatorOptions[:]),
		chromedp.ExecPath(exe.Path),
		chromedp.UserDataDir(dir),
		chromedp.Flag("disable-gpu", true),
		chromedp.WSURLReadTimeout(startTimeout),
	)
	if m.opts.NoSandbox {
		opts = append(opts, chromedp.NoSandbox)
	}
	// The browser outlives the request that started it.
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.WithoutCancel(ctx), opts...)
	browserCtx, browserCancel := chromedp.NewContext(allocCtx)
	startCtx, cancelStart := context.WithTimeout(ctx, startTimeout)
	defer cancelStart()
	done := make(chan error, 1)
	go func() { done <- chromedp.Do(browserCtx, noop) }()
	select {
	case err = <-done:
	case <-startCtx.Done():
		err = startCtx.Err()
		browserCancel()
		<-done //nolint:errcheck // the attempt ends with an error once the context is cancelled
	}
	if err != nil {
		browserCancel()
		allocCancel()
		inst.err = fmt.Errorf("browser: start %s: %w", exe.Path, err)
		return
	}
	inst.ctx = browserCtx
	inst.cancel = func() {
		// Close gracefully so the browser flushes cookies and storage to the
		// profile; the timeout bounds a browser that hangs.
		tctx, done := context.WithTimeout(browserCtx, closeTimeout)
		_ = chromedp.Cancel(tctx) //nolint:errcheck // the browser is cancelled below either way
		done()
		browserCancel()
		allocCancel()
	}
	m.log.Debug("browser: started", "group", inst.group)
}

// unlease ends one use of inst and arms the idle timer when it was the last.
func (m *Manager) unlease(inst *instance) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inst.refs--
	inst.lastUsed = time.Now()
	if inst.refs == 0 && !m.closed && m.instances[inst.group] == inst && inst.err == nil {
		inst.idle = time.AfterFunc(m.opts.IdleTimeout, func() { m.closeIfIdle(inst) })
	}
	m.signalLocked()
}

func (m *Manager) closeIfIdle(inst *instance) {
	m.mu.Lock()
	if inst.refs != 0 || m.instances[inst.group] != inst {
		m.mu.Unlock()
		return
	}
	m.removeLocked(inst)
	m.mu.Unlock()
	m.log.Debug("browser: stopping idle browser", "group", inst.group)
	inst.stop()
}

// removeLocked forgets inst. The caller stops it outside the lock.
func (m *Manager) removeLocked(inst *instance) {
	if m.instances[inst.group] == inst {
		delete(m.instances, inst.group)
	}
	if inst.idle != nil {
		inst.idle.Stop()
	}
	m.signalLocked()
}

func (m *Manager) signalLocked() {
	close(m.changed)
	m.changed = make(chan struct{})
}

// idlestLocked returns the started instance without users that was used
// longest ago.
func (m *Manager) idlestLocked() *instance {
	var best *instance
	for _, inst := range m.instances {
		if inst.refs > 0 || inst.cancel == nil {
			continue
		}
		if best == nil || inst.lastUsed.Before(best.lastUsed) {
			best = inst
		}
	}
	return best
}

func (i *instance) dead() bool {
	select {
	case <-i.ready:
		return i.err != nil || (i.ctx != nil && i.ctx.Err() != nil)
	default:
		return false
	}
}

func (i *instance) stop() {
	if i.cancel != nil {
		i.cancel()
	}
}

// Close stops every browser. Acquire fails afterwards.
func (m *Manager) Close() error {
	m.mu.Lock()
	m.closed = true
	var all []*instance
	for _, inst := range m.instances {
		all = append(all, inst)
		if inst.idle != nil {
			inst.idle.Stop()
		}
	}
	clear(m.instances)
	m.signalLocked()
	m.mu.Unlock()
	for _, inst := range all {
		inst.stop()
	}
	return nil
}

// Running describes a browser process of the pool.
type Running struct {
	Group    string `json:"group"`
	Sessions int    `json:"sessions"`
}

// Status describes the manager.
type Status struct {
	// Browser is the browser in use; nil until the first Acquire or Resolve.
	Browser *Installation `json:"browser,omitempty"`
	Running []Running     `json:"running"`
	Max     int           `json:"max"`
}

// Status reports the browser in use and the running pool.
func (m *Manager) Status() Status {
	m.resolveMu.Lock()
	var br *Installation
	if m.install != nil {
		cp := *m.install
		br = &cp
	}
	m.resolveMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	st := Status{Browser: br, Running: []Running{}, Max: m.opts.MaxBrowsers}
	for _, inst := range m.instances {
		st.Running = append(st.Running, Running{Group: inst.group, Sessions: inst.refs})
	}
	slices.SortFunc(st.Running, func(a, b Running) int { return strings.Compare(a.Group, b.Group) })
	return st
}

// ---- profiles ----

func (m *Manager) profileDir(group string) (string, error) {
	if m.opts.ProfilesDir == "" {
		return "", errors.New("browser: no profiles directory configured")
	}
	return filepath.Join(m.opts.ProfilesDir, sanitizeGroup(group)), nil
}

// Profiles lists the groups that have a profile on disk.
func (m *Manager) Profiles() ([]string, error) {
	if m.opts.ProfilesDir == "" {
		return nil, errors.New("browser: no profiles directory configured")
	}
	entries, err := os.ReadDir(m.opts.ProfilesDir)
	if errors.Is(err, os.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("browser: list profiles: %w", err)
	}
	out := []string{}
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// DeleteProfile removes the profile of a group (its cookies and storage).
// A group whose browser is running conflicts; a missing profile is not found.
func (m *Manager) DeleteProfile(group string) error {
	group = sanitizeGroup(group)
	dir, err := m.profileDir(group)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.instances[group] != nil {
		return domain.Conflict("browser: the browser of %q is running", group)
	}
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return domain.NotFound("browser profile", group)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("browser: delete profile: %w", err)
	}
	return nil
}

// SiteGroup returns the profile group for a URL: the registrable domain
// (example.com for https://www.example.com/x), the host for IP addresses and
// single-label hosts, and "default" when the URL has no host.
func SiteGroup(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return "default"
	}
	host := strings.ToLower(u.Hostname())
	if net.ParseIP(host) != nil {
		return sanitizeGroup(host)
	}
	if etld1, err := publicsuffix.EffectiveTLDPlusOne(host); err == nil {
		return sanitizeGroup(etld1)
	}
	return sanitizeGroup(host)
}

// sanitizeGroup makes a group name safe to use as a directory name.
func sanitizeGroup(g string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(g) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	s := b.String()
	if len(s) > 100 {
		s = s[:100]
	}
	if s == "" || strings.HasPrefix(s, ".") {
		s = "_" + s
	}
	return s
}
