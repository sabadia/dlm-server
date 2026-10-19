package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/events"
	"github.com/sabadia/dlm-server/internal/service"
	"github.com/sabadia/dlm-server/internal/storage"
)

// notifyTimeout bounds all plugins' handling of one start/error/done event.
const (
	notifyTimeout = 90 * time.Second
	// One worker keeps a task's start, error and done in order.
	notifyWorkers = 1
	notifyQueue   = 256
)

// Options configure a Manager.
type Options struct {
	// Dir holds the installed plugins, one directory each.
	Dir   string
	Store storage.Store
	// Bus delivers the task events behind the start, error and done hooks;
	// nil means those hooks never run.
	Bus    *events.Bus
	Logger *slog.Logger
	// HTTP sends the requests of dlm.http (see Env.HTTP).
	HTTP *http.Client
	// Git clones git sources; default RunGit.
	Git   GitClone
	Clock func() time.Time
}

// Manager installs plugins, keeps the enabled ones loaded and runs their
// hooks. It implements service.PluginHooks.
type Manager struct {
	opts Options
	log  *slog.Logger

	mu      sync.RWMutex
	entries map[string]*entry

	ready chan struct{}
}

type entry struct {
	rec  *storage.PluginRecord
	man  *Manifest
	inst *Instance
	err  string
}

// NewManager creates a Manager; call Load to bring up the installed plugins.
func NewManager(o Options) *Manager {
	if o.Git == nil {
		o.Git = RunGit
	}
	if o.Clock == nil {
		o.Clock = time.Now
	}
	log := o.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Manager{opts: o, log: log.With("component", "plugins"), entries: make(map[string]*entry), ready: make(chan struct{})}
}

func (m *Manager) pluginDir(name string) string { return filepath.Join(m.opts.Dir, name) }

func (m *Manager) storageFile(name string) string {
	return filepath.Join(m.opts.Dir, ".data", name+".json")
}

func (m *Manager) env(name string) Env {
	return Env{Logger: m.log, HTTP: m.opts.HTTP, StorageFile: m.storageFile(name)}
}

// Load starts the enabled installed plugins. A plugin that cannot start (its
// files are gone or its script fails) is kept and reports the error; it never
// stops the others.
func (m *Manager) Load(ctx context.Context) error {
	recs, err := m.opts.Store.Plugins().List(ctx)
	if err != nil {
		return fmt.Errorf("plugin: load: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = make(map[string]*entry, len(recs))
	for _, r := range recs {
		e := &entry{rec: r}
		m.start(ctx, e)
		m.entries[r.Name] = e
	}
	return nil
}

// start (re)creates the entry's instance when the plugin is enabled. The
// caller holds m.mu.
func (m *Manager) start(ctx context.Context, e *entry) {
	e.inst, e.err = nil, ""
	man, script, err := readPlugin(m.pluginDir(e.rec.Name))
	if err != nil {
		e.err = err.Error()
		m.log.Warn("plugin cannot be read", "plugin", e.rec.Name, "error", err)
		return
	}
	e.man = man
	if !e.rec.Enabled {
		return
	}
	inst, err := Load(ctx, man, script, m.env(e.rec.Name))
	if err != nil {
		e.err = err.Error()
		m.log.Warn("plugin failed to start", "plugin", e.rec.Name, "error", err)
		return
	}
	e.inst = inst
}

func (e *entry) info() *domain.Plugin {
	p := &domain.Plugin{
		Name: e.rec.Name, Version: e.rec.Version, Title: e.rec.Title, Description: e.rec.Description, Source: e.rec.Source,
		Enabled: e.rec.Enabled, Hooks: []string{}, Error: e.err, InstalledAt: e.rec.InstalledAt, UpdatedAt: e.rec.UpdatedAt,
	}
	if e.man != nil {
		p.Hooks, p.Net = e.man.HookNames(), e.man.Net
	}
	return p
}

// List returns the installed plugins ordered by name.
func (m *Manager) List(context.Context) []*domain.Plugin {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*domain.Plugin, 0, len(m.entries))
	for _, e := range m.entries {
		out = append(out, e.info())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns one plugin or an error matching domain.ErrNotFound.
func (m *Manager) Get(_ context.Context, name string) (*domain.Plugin, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.entries[name]
	if !ok {
		return nil, domain.NotFound("plugin", name)
	}
	return e.info(), nil
}

// InstallInput says what to install.
type InstallInput = domain.PluginInstall

// Install copies (or clones) a plugin into the plugins directory after
// checking its manifest and running its script once, and starts it. The plugin
// runs with the daemon's authority over the hosts in its manifest, so only
// install plugins you trust.
func (m *Manager) Install(ctx context.Context, in InstallInput) (*domain.Plugin, error) {
	if err := os.MkdirAll(m.opts.Dir, 0o755); err != nil { //nolint:gosec // plugin files are not secret
		return nil, fmt.Errorf("plugin: install: %w", err)
	}
	stage, err := os.MkdirTemp(m.opts.Dir, ".staging-")
	if err != nil {
		return nil, fmt.Errorf("plugin: install: %w", err)
	}
	defer func() { _ = os.RemoveAll(stage) }()
	staged := filepath.Join(stage, "plugin")
	if err := m.fetch(ctx, in, staged); err != nil {
		return nil, err
	}
	man, script, err := readPlugin(staged)
	if err != nil {
		return nil, fmt.Errorf("plugin: install: %w", err)
	}
	probe, err := Load(ctx, man, script, Env{Logger: m.log})
	if err != nil {
		return nil, fmt.Errorf("plugin: install: %w", domain.InvalidArgument("%v", err))
	}
	if len(probe.handlers) == 0 {
		return nil, fmt.Errorf("plugin: install: %w", domain.InvalidArgument("plugin %s registers no handlers: call dlm.on(hook, fn) in the script", man.Name))
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	old, exists := m.entries[man.Name]
	if exists && !in.Replace {
		return nil, fmt.Errorf("plugin: install: %w", domain.AlreadyExists("plugin", man.Name))
	}
	final := m.pluginDir(man.Name)
	backup := filepath.Join(stage, "previous")
	if exists {
		if err := os.Rename(final, backup); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("plugin: install: replace %s: %w", man.Name, err)
		}
	}
	if err := os.Rename(staged, final); err != nil {
		m.restore(backup, final)
		return nil, fmt.Errorf("plugin: install: %w", err)
	}

	now := m.opts.Clock().UTC()
	rec := &storage.PluginRecord{
		Name: man.Name, Version: man.Version, Title: man.Title, Description: man.Description, Source: in.Source,
		Enabled: !in.Disabled, InstalledAt: now, UpdatedAt: now,
	}
	if exists {
		rec.InstalledAt = old.rec.InstalledAt
		if in.Disabled {
			rec.Enabled = false
		} else {
			rec.Enabled = true
		}
		err = m.opts.Store.Plugins().Update(ctx, rec)
	} else {
		err = m.opts.Store.Plugins().Create(ctx, rec)
	}
	if err != nil {
		_ = os.RemoveAll(final)
		m.restore(backup, final)
		return nil, fmt.Errorf("plugin: install: %w", err)
	}
	e := &entry{rec: rec}
	m.start(ctx, e)
	m.entries[man.Name] = e
	return e.info(), nil
}

// restore puts a replaced plugin's files back after a failed install.
func (m *Manager) restore(backup, final string) {
	if _, err := os.Stat(backup); err == nil {
		if err := os.Rename(backup, final); err != nil {
			m.log.Error("plugin: could not restore the previous version", "path", final, "error", err)
		}
	}
}

// fetch puts the source's files at dst.
func (m *Manager) fetch(ctx context.Context, in InstallInput, dst string) error {
	if IsGitSource(in.Source) {
		if err := m.opts.Git(ctx, in.Source, in.Ref, dst); err != nil {
			return fmt.Errorf("plugin: install: %w", err)
		}
		if err := os.RemoveAll(filepath.Join(dst, ".git")); err != nil {
			return fmt.Errorf("plugin: install: %w", err)
		}
		return nil
	}
	if in.Ref != "" {
		return fmt.Errorf("plugin: install: %w", domain.InvalidArgument("a ref only applies to a git source"))
	}
	if !filepath.IsAbs(in.Source) {
		return fmt.Errorf("plugin: install: %w", domain.InvalidArgument("source %q must be an absolute directory or an https:// git URL", in.Source))
	}
	info, err := os.Stat(in.Source)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("plugin: install: %w", domain.InvalidArgument("source %q is not a directory", in.Source))
	}
	if err := copyTree(in.Source, dst); err != nil {
		return fmt.Errorf("plugin: install: %w", err)
	}
	return nil
}

// SetEnabled switches a plugin on or off.
func (m *Manager) SetEnabled(ctx context.Context, name string, enabled bool) (*domain.Plugin, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[name]
	if !ok {
		return nil, domain.NotFound("plugin", name)
	}
	rec := *e.rec
	rec.Enabled, rec.UpdatedAt = enabled, m.opts.Clock().UTC()
	if err := m.opts.Store.Plugins().Update(ctx, &rec); err != nil {
		return nil, fmt.Errorf("plugin: update: %w", err)
	}
	e.rec = &rec
	m.start(ctx, e)
	return e.info(), nil
}

// Uninstall removes a plugin and its files; the data it stored is removed too.
func (m *Manager) Uninstall(ctx context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.entries[name]; !ok {
		return domain.NotFound("plugin", name)
	}
	if err := m.opts.Store.Plugins().Delete(ctx, name); err != nil {
		return fmt.Errorf("plugin: uninstall: %w", err)
	}
	delete(m.entries, name)
	if err := os.RemoveAll(m.pluginDir(name)); err != nil {
		m.log.Warn("plugin files could not be removed", "plugin", name, "error", err)
	}
	_ = os.Remove(m.storageFile(name))
	return nil
}

// running returns the loaded instances, ordered by plugin name.
func (m *Manager) running() []*Instance {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.entries))
	for n, e := range m.entries {
		if e.inst != nil {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	out := make([]*Instance, len(names))
	for i, n := range names {
		out[i] = m.entries[n].inst
	}
	return out
}

// resolution is what a resolve or refresh handler returns.
type resolution struct {
	URL       string            `json:"url"`
	Name      string            `json:"name"`
	Referrer  string            `json:"referrer"`
	UserAgent string            `json:"userAgent"`
	Headers   map[string]string `json:"headers"`
	Mirrors   []string          `json:"mirrors"`
	MediaHint string            `json:"mediaHint"`
}

func parseResolution(v any) (*resolution, error) {
	if v == nil {
		return nil, nil //nolint:nilnil // the plugin does not handle the link
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("the result cannot be read: %w", err)
	}
	var r resolution
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("the result must be null or an object with a url: %w", err)
	}
	if strings.TrimSpace(r.URL) == "" {
		return nil, errors.New("the result has no url")
	}
	return &r, nil
}

// apply returns src with the resolution's link and context.
func (r *resolution) apply(src domain.Source) domain.Source {
	next := src
	if next.OriginalURL == "" {
		next.OriginalURL = src.CurrentURL
	}
	if next.PageURL == "" {
		next.PageURL = src.CurrentURL
	}
	next.CurrentURL, next.RedirectURL = strings.TrimSpace(r.URL), ""
	next.ResolverKind = domain.ResolverPlugin
	if r.Referrer != "" {
		next.Referrer = r.Referrer
	}
	if r.UserAgent != "" {
		next.UserAgent = r.UserAgent
	}
	if len(r.Headers) > 0 {
		h := make(map[string]string, len(src.Headers)+len(r.Headers))
		for k, v := range src.Headers {
			h[k] = v
		}
		for k, v := range r.Headers {
			h[k] = v
		}
		next.Headers = h
	}
	if len(r.Mirrors) > 0 {
		next.Mirrors = r.Mirrors
	}
	if r.MediaHint != "" {
		next.MediaHint = r.MediaHint
	}
	return next
}

func sourceArg(src domain.Source) map[string]any {
	h := make(map[string]any, len(src.Headers))
	for k, v := range src.Headers {
		h[k] = v
	}
	return map[string]any{
		"url": src.CurrentURL, "originalUrl": src.OriginalURL, "pageUrl": src.PageURL, "referrer": src.Referrer, "headers": h,
	}
}

func taskArg(t *domain.Task) map[string]any {
	a := map[string]any{
		"id": t.ID, "name": t.Name, "url": t.Source.CurrentURL, "originalUrl": t.Source.OriginalURL, "pageUrl": t.Source.PageURL,
		"queueId": t.QueueID, "dir": t.Dir, "size": t.Size, "downloaded": t.Downloaded, "status": string(t.Status),
	}
	if t.Error != nil {
		a["error"] = map[string]any{"code": t.Error.Code, "message": t.Error.Message, "retryable": t.Error.Retryable}
	}
	return a
}

// Resolve runs the resolve hook of the enabled plugins whose patterns match
// src, in plugin-name order, and returns the first link a plugin gives back,
// with the file name it suggested. handled is false when no plugin took the
// link. A plugin that fails is logged and skipped: a broken plugin must not
// stop a download.
func (m *Manager) Resolve(ctx context.Context, src domain.Source) (next domain.Source, name string, handled bool) {
	for _, inst := range m.running() {
		man := inst.Manifest()
		if !man.Matches(HookResolve, src.CurrentURL) || !inst.Handles(HookResolve) {
			continue
		}
		out, err := inst.Call(ctx, HookResolve, sourceArg(src))
		if err != nil {
			m.log.Warn("plugin resolve failed", "plugin", man.Name, "url", redactURL(src.CurrentURL), "error", err)
			continue
		}
		res, err := parseResolution(out)
		if err != nil {
			m.log.Warn("plugin resolve returned something unusable", "plugin", man.Name, "error", err)
			continue
		}
		if res == nil {
			continue
		}
		m.log.Info("plugin resolved a link", "plugin", man.Name, "url", redactURL(src.CurrentURL))
		return res.apply(src), res.Name, true
	}
	return src, "", false
}

// Refresh asks the plugins for a new link for a task whose link expired. It
// returns the task's source with the new link, or handled false when no plugin
// handles the task; a plugin's failure is returned when no other plugin
// succeeded.
func (m *Manager) Refresh(ctx context.Context, t *domain.Task) (next domain.Source, handled bool, err error) {
	var errs []error
	for _, inst := range m.running() {
		man := inst.Manifest()
		if !man.Matches(HookRefresh, t.Source.CurrentURL) || !inst.Handles(HookRefresh) {
			continue
		}
		arg := sourceArg(t.Source)
		arg["task"] = taskArg(t)
		out, err := inst.Call(ctx, HookRefresh, arg)
		if err == nil {
			var res *resolution
			if res, err = parseResolution(out); err == nil && res != nil {
				return res.apply(t.Source), true, nil
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("plugin %s: %w", man.Name, err))
		}
	}
	return t.Source, false, errors.Join(errs...)
}

// Ready is closed once Run follows the bus: events published before are not
// delivered to the hooks.
func (m *Manager) Ready() <-chan struct{} { return m.ready }

// Run delivers the start, error and done hooks until ctx ends: it follows the
// task status events on the bus. It must be called once.
func (m *Manager) Run(ctx context.Context) error {
	if m.opts.Bus == nil {
		close(m.ready)
		<-ctx.Done()
		return nil
	}
	sub := m.opts.Bus.Subscribe(ctx, events.Filter{Types: []events.Type{events.TypeTaskStatus}})
	defer sub.Unsubscribe()
	close(m.ready)
	jobs := make(chan events.Event, notifyQueue)
	var wg sync.WaitGroup
	for range notifyWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case ev := <-jobs:
					m.notify(ctx, ev)
				}
			}
		}()
	}
	defer wg.Wait()
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-sub.C():
			if !ok {
				return nil
			}
			if hookFor(ev) == "" {
				continue
			}
			select {
			case jobs <- ev:
			default:
				m.log.Warn("plugin event queue full; event dropped", "task", ev.TaskID)
			}
		}
	}
}

// hookFor names the hook a task status change triggers, or "".
func hookFor(ev events.Event) string {
	d, ok := ev.Data.(service.StatusData)
	if !ok {
		return ""
	}
	switch d.To {
	case domain.StatusRunning:
		return HookStart
	case domain.StatusError:
		return HookError
	case domain.StatusCompleted:
		return HookDone
	default:
		return ""
	}
}

func (m *Manager) notify(ctx context.Context, ev events.Event) {
	hook := hookFor(ev)
	insts := m.running()
	if hook == "" || len(insts) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, notifyTimeout)
	defer cancel()
	t, err := m.opts.Store.Tasks().Get(ctx, ev.TaskID)
	if err != nil {
		m.log.Debug("plugin event: the task is gone", "task", ev.TaskID, "error", err)
		return
	}
	// The stored task may have moved on since the event: report the state the
	// event announced.
	if d, ok := ev.Data.(service.StatusData); ok {
		t.Status, t.Error = d.To, d.Error
	}
	for _, inst := range insts {
		man := inst.Manifest()
		if !man.Matches(hook, t.Source.CurrentURL) || !inst.Handles(hook) {
			continue
		}
		if _, err := inst.Call(ctx, hook, map[string]any{"event": hook, "task": taskArg(t)}); err != nil {
			m.log.Warn("plugin hook failed", "plugin", man.Name, "hook", hook, "task", t.ID, "error", err)
		}
	}
}

// redactURL drops credentials and the query of a URL for logging.
func redactURL(raw string) string {
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[:i]
	}
	if i := strings.Index(raw, "@"); i >= 0 {
		if j := strings.Index(raw, "://"); j >= 0 && j < i {
			raw = raw[:j+3] + raw[i+1:]
		}
	}
	return raw
}
