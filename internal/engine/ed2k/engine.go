package ed2k

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/monkeyWie/goed2k"
	"github.com/monkeyWie/goed2k/protocol"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/engine/httpx"
	"github.com/sabadia/dlm-server/internal/platform"
)

// EngineName is the name of the ed2k engine, stored on tasks.
const EngineName = "ed2k"

// Defaults for the tunables.
const (
	DefaultPollInterval       = 500 * time.Millisecond
	DefaultCheckpointInterval = 5 * time.Second
	finalCheckpointTimeout    = 5 * time.Second
	// PartSuffix is appended to the destination path while downloading.
	PartSuffix = ".part"
)

// Config configures the ed2k client the engine owns.
type Config struct {
	// ListenPort is the TCP port for incoming connections (and the next one
	// for UDP); 0 picks free ports.
	ListenPort int
	// Servers are ed2k servers (host:port) the client connects to for finding
	// sources.
	Servers []string
	// ServerMet is a server.met file whose servers are connected too.
	ServerMet string
	// EnableKad turns on the Kad network for finding sources without a server.
	EnableKad bool
	// EnableUPnP asks the router to forward the listen port.
	EnableUPnP bool
	// UploadLimitKB caps the upload in kilobytes per second; 0 is unlimited.
	UploadLimitKB int
}

type options struct {
	poll           time.Duration
	checkpointIntv time.Duration
	freeSpace      func(dir string) (uint64, error)
}

// Option configures an Engine.
type Option func(*options)

// WithPollInterval sets how often a running download looks at its transfer
// (default 500ms).
func WithPollInterval(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.poll = d
		}
	}
}

// WithCheckpointInterval sets the minimum time between periodic checkpoints
// (default 5s: a checkpoint carries the resume data).
func WithCheckpointInterval(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.checkpointIntv = d
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

// Engine is the ed2k engine. It owns one client, created on first use and
// released by Close. It is safe for concurrent use.
type Engine struct {
	env  engine.Env
	cfg  Config
	opts options

	mu     sync.Mutex
	client *goed2k.Client
	closed bool
	active map[string]bool // hashes being downloaded
}

var _ engine.Engine = (*Engine)(nil)

// New returns the ed2k engine; the client starts when a transfer needs it.
func New(env engine.Env, cfg Config, opts ...Option) *Engine {
	if env.Logger == nil {
		env.Logger = slog.New(slog.DiscardHandler)
	}
	if env.Clock == nil {
		env.Clock = time.Now
	}
	o := options{poll: DefaultPollInterval, checkpointIntv: DefaultCheckpointInterval, freeSpace: platform.FreeSpace}
	for _, opt := range opts {
		opt(&o)
	}
	return &Engine{env: env, cfg: cfg, opts: o, active: map[string]bool{}}
}

// Name returns "ed2k".
func (*Engine) Name() string { return EngineName }

// Match accepts ed2k://|file| links.
func (*Engine) Match(src domain.Source) bool { return match(src.CurrentURL) }

// Close stops the client and every download using it. The engine cannot be
// used afterwards.
func (e *Engine) Close() error {
	e.mu.Lock()
	cl := e.client
	e.client, e.closed = nil, true
	e.mu.Unlock()
	if cl != nil {
		cl.Close()
	}
	return nil
}

// warnHandler passes only warnings and errors of the library on to the log.
type warnHandler struct{ slog.Handler }

func (h warnHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= slog.LevelWarn && h.Handler.Enabled(ctx, l)
}

func (h warnHandler) WithAttrs(a []slog.Attr) slog.Handler {
	return warnHandler{h.Handler.WithAttrs(a)}
}
func (h warnHandler) WithGroup(n string) slog.Handler { return warnHandler{h.Handler.WithGroup(n)} }

// getClient returns the client, starting it and connecting it to the
// configured servers when needed.
func (e *Engine) getClient() (*goed2k.Client, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, engine.Fatal(domain.ErrorCodeFatal, errors.New("ed2k: the engine is closed"))
	}
	if e.client != nil {
		return e.client, nil
	}
	s := goed2k.NewSettings()
	s.ListenPort = e.cfg.ListenPort
	s.UDPPort = 0
	if e.cfg.ListenPort > 0 {
		s.UDPPort = e.cfg.ListenPort + 1
	}
	s.EnableDHT = e.cfg.EnableKad
	s.EnableUPnP = e.cfg.EnableUPnP
	s.MaxUploadRateKB = e.cfg.UploadLimitKB
	s.ReconnectToServer = true
	s.Logger = slog.New(warnHandler{e.env.Logger.With("engine", EngineName).Handler()})
	cl := goed2k.NewClient(s)
	if err := cl.Start(); err != nil {
		return nil, engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("ed2k: start the client: %w", err))
	}
	if len(e.cfg.Servers) == 0 && e.cfg.ServerMet == "" && !e.cfg.EnableKad {
		e.env.Logger.Warn("ed2k: no servers and no Kad configured; only sources named in the link can be used (ed2k.servers, ed2k.server_met, ed2k.kad)")
	}
	if len(e.cfg.Servers) > 0 {
		if err := cl.ConnectServers(e.cfg.Servers...); err != nil {
			e.env.Logger.Warn("ed2k: cannot connect to the configured servers", "error", err)
		}
	}
	if e.cfg.ServerMet != "" {
		if err := cl.ConnectServerMet(e.cfg.ServerMet); err != nil {
			e.env.Logger.Warn("ed2k: cannot use the server.met file", "path", e.cfg.ServerMet, "error", err)
		}
	}
	e.client = cl
	return cl, nil
}

// claim registers a download of hash; it fails when one is running.
func (e *Engine) claim(hash string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.active[hash] {
		return false
	}
	e.active[hash] = true
	return true
}

func (e *Engine) unclaim(hash string) {
	e.mu.Lock()
	delete(e.active, hash)
	e.mu.Unlock()
}

// Resolve describes the file of a link; it needs no network.
func (e *Engine) Resolve(_ context.Context, req engine.Request) (*engine.Probe, error) {
	l, err := parseLink(req.Source.CurrentURL)
	if err != nil {
		return nil, engine.Fatal(domain.ErrorCodeFatal, err)
	}
	name := httpx.SanitizeFilename(l.name)
	src := req.Source
	src.Fingerprint = domain.Fingerprint{Size: l.size, Filename: name}
	return &engine.Probe{
		Engine: EngineName, Source: src, Suggested: name,
		Resource: domain.Resource{Name: name, Size: l.size, Files: []domain.File{{Index: 0, Name: name, Size: l.size, Selected: true}}},
	}, nil
}

// NewDownload validates spec and prepares a download without doing I/O.
func (e *Engine) NewDownload(spec engine.Spec) (engine.Download, error) {
	_ = spec.Probe.Close() // nil-safe; nothing is attached by this engine
	if spec.Path == "" || !filepath.IsAbs(spec.Path) {
		return nil, engine.Fatal(domain.ErrorCodeFatal, domain.InvalidArgument("ed2k: download path %q must be absolute", spec.Path))
	}
	l, err := parseLink(spec.Source.CurrentURL)
	if err != nil {
		return nil, engine.Fatal(domain.ErrorCodeFatal, err)
	}
	return newDownload(e, spec, l), nil
}

var errDestinationExists = errors.New("ed2k: destination file already exists")

// ErrDestinationExists is wrapped by the error of a download whose final path
// already exists (the file-exists policy runs before the engine).
var ErrDestinationExists = errDestinationExists

// hashKey is the map key of a hash.
func hashKey(h protocol.Hash) string { return h.String() }
