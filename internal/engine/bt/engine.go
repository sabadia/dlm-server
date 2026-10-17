package bt

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"golang.org/x/time/rate"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/engine/httpx"
	"github.com/sabadia/dlm-server/internal/platform"
)

// EngineName is the name of the BitTorrent engine, stored on tasks.
const EngineName = "bt"

// Defaults for the tunables.
const (
	DefaultMetadataTimeout    = 2 * time.Minute
	DefaultPollInterval       = 500 * time.Millisecond
	DefaultCheckpointInterval = time.Second
	// DefaultStallReset is how long a download may make no progress, while
	// connected to peers, before the connections are reset.
	DefaultStallReset      = time.Minute
	finalCheckpointTimeout = 5 * time.Second
	// stateInterval is how often the engine state (the uploaded bytes and
	// the seeding time) is written while seeding.
	stateInterval = time.Minute
	// uploadBurst is the smallest burst of the upload limiter: it must hold
	// one chunk.
	uploadBurst = 256 << 10
)

// Config configures the BitTorrent client the engine owns.
type Config struct {
	// ListenPort is the port for incoming connections; 0 picks a free one.
	ListenPort int
	// ListenHost binds the client to one local address; empty binds all.
	ListenHost string
	// DisableDHT and DisablePEX turn off the distributed hash table and
	// peer exchange.
	DisableDHT bool
	DisablePEX bool
	// DisablePortForwarding stops the client from asking the router (UPnP,
	// NAT-PMP) to forward the listen port.
	DisablePortForwarding bool
	// UploadLimit caps the upload of all torrents together in bytes per
	// second; 0 means unlimited.
	UploadLimit int64
	// MetadataTimeout bounds how long a magnet link may take to deliver its
	// metadata; 0 means DefaultMetadataTimeout.
	MetadataTimeout time.Duration
}

type options struct {
	poll           time.Duration
	checkpointIntv time.Duration
	stallReset     time.Duration
	freeSpace      func(dir string) (uint64, error)
}

// Option configures an Engine.
type Option func(*options)

// WithPollInterval sets how often a running download looks at its torrent
// (default 500ms).
func WithPollInterval(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.poll = d
		}
	}
}

// WithCheckpointInterval sets the minimum time between periodic checkpoints
// (default 1s).
func WithCheckpointInterval(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.checkpointIntv = d
		}
	}
}

// WithStallReset sets how long a download may make no progress, while it has
// peers, before its peer connections are closed so they are made again
// (default 1 minute). A connection can get stuck with requests that are never
// answered; a fresh one fixes that.
func WithStallReset(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.stallReset = d
		}
	}
}

// WithFreeSpace replaces platform.FreeSpace for the disk-space precheck.
func WithFreeSpace(fn func(dir string) (uint64, error)) Option {
	return func(o *options) {
		if fn != nil {
			o.freeSpace = fn
		}
	}
}

// Engine is the BitTorrent engine. It owns one torrent client, created on
// first use and released by Close. It is safe for concurrent use.
type Engine struct {
	env  engine.Env
	cfg  Config
	opts options

	mu     sync.Mutex
	client *torrent.Client
	closed bool
	// active holds the torrents in the client by info hash.
	active map[metainfo.Hash]*entry
}

var _ engine.Engine = (*Engine)(nil)

// entry is a torrent in the client: either downloaded by a task, or only
// held while resolves read its metadata.
type entry struct {
	t           *torrent.Torrent
	users       int  // resolves reading the metadata
	downloading bool // a task downloads it
	// idle is closed once the entry is gone.
	idle chan struct{}
}

// New returns the BitTorrent engine. The client is not started until a
// torrent needs it. A nil env.Clients leaves .torrent URLs unsupported.
func New(env engine.Env, cfg Config, opts ...Option) *Engine {
	if env.Logger == nil {
		env.Logger = slog.New(slog.DiscardHandler)
	}
	if env.Clock == nil {
		env.Clock = time.Now
	}
	if cfg.MetadataTimeout <= 0 {
		cfg.MetadataTimeout = DefaultMetadataTimeout
	}
	o := options{poll: DefaultPollInterval, checkpointIntv: DefaultCheckpointInterval, stallReset: DefaultStallReset, freeSpace: platform.FreeSpace}
	for _, opt := range opts {
		opt(&o)
	}
	return &Engine{env: env, cfg: cfg, opts: o, active: map[metainfo.Hash]*entry{}}
}

// Name returns "bt".
func (*Engine) Name() string { return EngineName }

// Match accepts magnet links, embedded .torrent data URLs and http(s) URLs
// ending in .torrent.
func (*Engine) Match(src domain.Source) bool { return match(src.CurrentURL) }

// Close stops the torrent client and every download using it. The engine
// cannot be used afterwards.
func (e *Engine) Close() error {
	e.mu.Lock()
	cl := e.client
	e.client, e.closed = nil, true
	e.mu.Unlock()
	if cl == nil {
		return nil
	}
	return errors.Join(cl.Close()...)
}

// warnHandler passes only warnings and errors of the torrent library on to
// the daemon's log; it is very chatty otherwise.
type warnHandler struct{ slog.Handler }

func (h warnHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= slog.LevelWarn && h.Handler.Enabled(ctx, l)
}

func (h warnHandler) WithAttrs(a []slog.Attr) slog.Handler {
	return warnHandler{h.Handler.WithAttrs(a)}
}
func (h warnHandler) WithGroup(n string) slog.Handler { return warnHandler{h.Handler.WithGroup(n)} }

// getClient returns the client, starting it when needed.
func (e *Engine) getClient() (*torrent.Client, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, engine.Fatal(domain.ErrorCodeFatal, errors.New("bt: the engine is closed"))
	}
	if e.client != nil {
		return e.client, nil
	}
	cc := torrent.NewDefaultClientConfig()
	cc.DefaultStorage = nullStorage{}
	cc.ListenPort = e.cfg.ListenPort
	if h := e.cfg.ListenHost; h != "" {
		cc.ListenHost = func(string) string { return h }
		if ip := net.ParseIP(h); ip != nil && ip.To4() != nil {
			cc.DisableIPv6 = true
		}
	}
	cc.NoDHT = e.cfg.DisableDHT
	cc.DisablePEX = e.cfg.DisablePEX
	cc.NoDefaultPortForwarding = e.cfg.DisablePortForwarding
	cc.Seed = true // uploading after our own needs are met is what seeding is
	cc.Slogger = slog.New(warnHandler{e.env.Logger.With("engine", EngineName).Handler()})
	if n := e.cfg.UploadLimit; n > 0 {
		cc.UploadRateLimiter = rate.NewLimiter(rate.Limit(n), int(max(n, uploadBurst)))
	}
	cl, err := torrent.NewClient(cc)
	if err != nil {
		return nil, engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("bt: start the torrent client: %w", err))
	}
	e.client = cl
	return cl, nil
}

// metaAttachment carries the metadata a probe found to the download created
// from it. It owns nothing.
type metaAttachment struct{ mi *metainfo.MetaInfo }

func (metaAttachment) Close() error { return nil }

// Resolve reads the torrent's metadata and describes its files. A magnet
// link has to fetch the metadata from peers first, for at most the metadata
// timeout.
func (e *Engine) Resolve(ctx context.Context, req engine.Request) (*engine.Probe, error) {
	src, err := e.load(ctx, req.Source, req.Settings)
	if err != nil {
		return nil, err
	}
	mi := src.mi
	if mi == nil {
		if mi, err = e.fetchMetadata(ctx, src); err != nil {
			return nil, err
		}
	}
	info, err := mi.UnmarshalInfo()
	if err != nil {
		return nil, engine.Fatal(domain.ErrorCodeFatal, domain.InvalidArgument("bt: invalid torrent: %v", err))
	}
	res := resourceOf(&info, src.hash)
	out := req.Source
	out.Fingerprint = domain.Fingerprint{Size: res.Size, Filename: res.Name}
	p := &engine.Probe{Engine: EngineName, Source: out, Resource: res, Suggested: res.Name}
	p.Attach(metaAttachment{mi: mi})
	return p, nil
}

// fetchMetadata asks the swarm of a magnet link for the torrent's metadata.
func (e *Engine) fetchMetadata(ctx context.Context, src *source) (*metainfo.MetaInfo, error) {
	cl, err := e.getClient()
	if err != nil {
		return nil, err
	}
	t, release, err := e.hold(cl, src)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := e.waitInfo(ctx, t); err != nil {
		return nil, err
	}
	mi := t.Metainfo()
	// A v1 torrent has no piece layers. The library hands out an empty (not
	// nil) map, which it rejects when the metainfo is added again.
	if len(mi.PieceLayers) == 0 {
		mi.PieceLayers = nil
	}
	return &mi, nil
}

// waitInfo waits for the torrent's metadata.
func (e *Engine) waitInfo(ctx context.Context, t *torrent.Torrent) error {
	timer := time.NewTimer(e.cfg.MetadataTimeout)
	defer timer.Stop()
	select {
	case <-t.GotInfo():
		return nil
	case <-ctx.Done():
		return ctx.Err() //nolint:wrapcheck // cancellation is reported as is
	case <-t.Closed():
		return engine.Transient(domain.ErrorCodeNetwork, errors.New("bt: the torrent was removed while its metadata was being fetched"))
	case <-timer.C:
		return engine.Transient(domain.ErrorCodeNetwork, fmt.Errorf("bt: no metadata for %s after %s (no peers found?)", src2hex(t), e.cfg.MetadataTimeout))
	}
}

func src2hex(t *torrent.Torrent) string { return t.InfoHash().HexString() }

// hold keeps the torrent of src in the client for a resolve and returns a
// function that lets it go. A torrent a task downloads is shared, never
// dropped.
func (e *Engine) hold(cl *torrent.Client, src *source) (*torrent.Torrent, func(), error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ent := e.active[src.hash]
	if ent == nil {
		spec := *src.magnet
		t, _, err := cl.AddTorrentSpec(&spec)
		if err != nil {
			return nil, nil, engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("bt: add torrent: %w", err))
		}
		ent = &entry{t: t, idle: make(chan struct{})}
		e.active[src.hash] = ent
	}
	ent.users++
	return ent.t, func() { e.release(src.hash, ent, false) }, nil
}

// release gives up one hold (download=false) or the download (download=true)
// of an entry; the torrent is dropped once nothing uses it.
func (e *Engine) release(hash metainfo.Hash, ent *entry, download bool) {
	e.mu.Lock()
	if download {
		ent.downloading = false
	} else {
		ent.users--
	}
	gone := ent.users <= 0 && !ent.downloading && e.active[hash] == ent
	if gone {
		delete(e.active, hash)
	}
	e.mu.Unlock()
	if gone {
		ent.t.Drop()
		close(ent.idle)
	}
}

// claim registers a download of hash, adding the torrent with add. It waits
// for resolves of the same torrent to finish first, because the torrent's
// storage is fixed when it is added. Two downloads of one torrent are
// refused.
func (e *Engine) claim(ctx context.Context, hash metainfo.Hash, add func() (*torrent.Torrent, error)) (*entry, error) {
	for {
		e.mu.Lock()
		ent := e.active[hash]
		if ent == nil {
			t, err := add()
			if err != nil {
				e.mu.Unlock()
				return nil, err
			}
			ent = &entry{t: t, downloading: true, idle: make(chan struct{})}
			e.active[hash] = ent
			e.mu.Unlock()
			return ent, nil
		}
		if ent.downloading {
			e.mu.Unlock()
			return nil, engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("bt: the torrent %s is already being downloaded by another task", hash.HexString()))
		}
		idle := ent.idle
		e.mu.Unlock()
		select {
		case <-idle:
		case <-ctx.Done():
			return nil, ctx.Err() //nolint:wrapcheck // cancellation is reported as is
		}
	}
}

// resourceOf describes a torrent: every file selected.
func resourceOf(info *metainfo.Info, hash metainfo.Hash) domain.Resource {
	name := httpx.SanitizeFilename(info.BestName())
	if name == "" || name == metainfo.NoName {
		name = "torrent-" + hash.HexString()[:8]
	}
	res := domain.Resource{Name: name, Size: info.TotalLength()}
	if !info.IsDir() {
		res.Files = []domain.File{{Index: 0, Name: name, Size: info.TotalLength(), Selected: true}}
		return res
	}
	for i, f := range info.UpvertedFiles() {
		parts := f.BestPath()
		if len(parts) == 0 {
			continue
		}
		res.Files = append(res.Files, domain.File{
			Index: i, Path: strings.Join(parts[:len(parts)-1], "/"), Name: parts[len(parts)-1], Size: f.Length, Selected: true,
		})
	}
	return res
}

// NewDownload validates spec and prepares a download without doing I/O. It
// takes ownership of spec.Probe: metadata a probe of this engine found is
// kept for Run.
func (e *Engine) NewDownload(spec engine.Spec) (engine.Download, error) {
	var mi *metainfo.MetaInfo
	if spec.Probe != nil {
		if spec.Probe.Engine == EngineName {
			if a, ok := spec.Probe.TakeAttachment().(metaAttachment); ok {
				mi = a.mi
			}
		}
		_ = spec.Probe.Close() // nil-safe; releases an attachment that was not taken
	}
	if spec.Path == "" || !filepath.IsAbs(spec.Path) {
		return nil, engine.Fatal(domain.ErrorCodeFatal, domain.InvalidArgument("bt: download path %q must be absolute", spec.Path))
	}
	if !match(spec.Source.CurrentURL) {
		return nil, engine.Fatal(domain.ErrorCodeFatal, domain.InvalidArgument("bt: %q is not a torrent source", redact(spec.Source.CurrentURL)))
	}
	return newDownload(e, spec, mi), nil
}

// redact shortens a source for messages: magnet links and data URLs can be
// very long.
func redact(raw string) string {
	if len(raw) > 80 {
		return raw[:80] + "…"
	}
	return raw
}

var errDestinationExists = errors.New("bt: destination file already exists")

// ErrDestinationExists is wrapped by the error of a download whose
// single-file destination already exists although nothing was downloaded
// before (the file-exists policy runs before the engine).
var ErrDestinationExists = errDestinationExists

// pathExists reports whether something is at path.
func pathExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("bt: stat %q: %w", path, err)
	}
}
