package httpx

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/engine/retry"
	"github.com/sabadia/dlm-server/internal/platform"
)

// EngineName is the name of the HTTP engine, stored on tasks.
const EngineName = "http"

// Defaults for the downloader tunables (see the With* options).
const (
	DefaultMinSplitSize       int64 = 1 << 20
	DefaultStallTimeout             = 30 * time.Second
	DefaultCheckpointInterval       = time.Second
	DefaultSlowStartInterval        = 500 * time.Millisecond
	// DefaultMaxConnections is used when the effective settings carry no
	// MaxConnections value (it matches the settings defaults).
	DefaultMaxConnections = 16
	// finalCheckpointTimeout bounds the last checkpoint written after the
	// run context was cancelled.
	finalCheckpointTimeout = 5 * time.Second
	// bufferSize is the per-connection read buffer.
	bufferSize = 64 << 10
)

type options struct {
	minSplit       int64
	stallTimeout   time.Duration
	checkpointIntv time.Duration
	slowStartIntv  time.Duration
	sleep          retry.Sleeper
	freeSpace      func(dir string) (uint64, error)
	preallocate    func(f *os.File, size int64) error
}

// Option configures an Engine.
type Option func(*options)

// WithMinSplitSize sets the smallest remaining length a segment must have
// to be split for a new connection or stolen by an idle one (default
// 1 MiB). Values below 1 are ignored.
func WithMinSplitSize(n int64) Option {
	return func(o *options) {
		if n > 0 {
			o.minSplit = n
		}
	}
}

// WithStallTimeout sets how long a connection may wait for bytes (or for
// response headers) before its request is aborted and retried as a
// transient network error (default 30s). Values <= 0 are ignored.
func WithStallTimeout(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.stallTimeout = d
		}
	}
}

// WithCheckpointInterval sets the minimum time between periodic
// checkpoints (default 1s). Values <= 0 are ignored.
func WithCheckpointInterval(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.checkpointIntv = d
		}
	}
}

// WithSlowStartInterval sets how often the downloader considers adding
// connections (default 500ms). Values <= 0 are ignored.
func WithSlowStartInterval(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.slowStartIntv = d
		}
	}
}

// WithRetrySleeper replaces the wait between retries (tests use a fake).
func WithRetrySleeper(s retry.Sleeper) Option {
	return func(o *options) {
		if s != nil {
			o.sleep = s
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

// WithPreallocate replaces platform.Preallocate.
func WithPreallocate(fn func(f *os.File, size int64) error) Option {
	return func(o *options) {
		if fn != nil {
			o.preallocate = fn
		}
	}
}

// Engine is the HTTP(S) engine. It is safe for concurrent use.
type Engine struct {
	env        engine.Env
	opts       options
	classifier Classifier
}

var _ engine.Engine = (*Engine)(nil)

// New returns the HTTP engine. env.Clients is required for Resolve and
// Download.Run; a nil Logger discards logs and a nil Clock uses time.Now.
func New(env engine.Env, opts ...Option) *Engine {
	if env.Logger == nil {
		env.Logger = slog.New(slog.DiscardHandler)
	}
	if env.Clock == nil {
		env.Clock = time.Now
	}
	o := options{
		minSplit:       DefaultMinSplitSize,
		stallTimeout:   DefaultStallTimeout,
		checkpointIntv: DefaultCheckpointInterval,
		slowStartIntv:  DefaultSlowStartInterval,
		sleep:          retry.Sleep,
		freeSpace:      platform.FreeSpace,
		preallocate:    platform.Preallocate,
	}
	for _, opt := range opts {
		opt(&o)
	}
	return &Engine{env: env, opts: o, classifier: Classifier{Now: env.Clock}}
}

// Name returns "http".
func (e *Engine) Name() string { return EngineName }

// Match accepts http and https URLs. More specific engines (hls, dash) are
// registered before this one.
func (e *Engine) Match(src domain.Source) bool {
	u, err := url.Parse(strings.TrimSpace(src.CurrentURL))
	if err != nil {
		return false
	}
	s := strings.ToLower(u.Scheme)
	return (s == "http" || s == "https") && u.Host != ""
}

// Resolve probes the source (see Probe). The returned engine.Probe carries
// the open probe response as its attachment, so a download created from it
// streams the first bytes without a second request. Errors are classified
// (see Classify).
func (e *Engine) Resolve(ctx context.Context, req engine.Request) (*engine.Probe, error) {
	if e.env.Clients == nil {
		return nil, engine.Fatal(domain.ErrorCodeFatal, errors.New("httpx: resolve: no client factory"))
	}
	client, err := e.env.Clients.Client(req.Settings)
	if err != nil {
		return nil, engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("httpx: resolve: client: %w", err))
	}
	res, err := Probe(ctx, client, req.Source, req.Settings)
	if err != nil {
		return nil, e.classifier.Classify(err)
	}
	p := &engine.Probe{Engine: EngineName, Source: res.Source, Resource: res.Resource, Suggested: res.Filename}
	p.Attach(res)
	return p, nil
}

// NewDownload validates spec and prepares a download without doing I/O.
// It takes ownership of spec.Probe: the probe's open response (if it was
// produced by this engine) is kept for Run and released when Run returns;
// the probe itself is closed immediately. A Download that is never run
// keeps that response open, so callers must Run what they create.
func (e *Engine) NewDownload(spec engine.Spec) (engine.Download, error) {
	var attach *ProbeResult
	var probeSrc *domain.Source
	var probeRes *domain.Resource
	if spec.Probe != nil {
		if spec.Probe.Engine == EngineName {
			if c := spec.Probe.TakeAttachment(); c != nil {
				if pr, ok := c.(*ProbeResult); ok && pr.Response != nil {
					attach = pr
				} else {
					_ = c.Close() // not ours: release it
				}
			}
			s, r := spec.Probe.Source, spec.Probe.Resource
			probeSrc, probeRes = &s, &r
		}
		_ = spec.Probe.Close() // attachment already taken; nothing else to release
		spec.Probe = nil
	}
	fail := func(err error) (engine.Download, error) {
		_ = attach.Close() // ownership was transferred to us
		return nil, engine.Fatal(domain.ErrorCodeFatal, err)
	}
	if spec.Path == "" || !filepath.IsAbs(spec.Path) {
		return fail(domain.InvalidArgument("httpx: download path %q must be absolute", spec.Path))
	}
	if strings.TrimSpace(spec.Source.CurrentURL) == "" {
		return fail(domain.InvalidArgument("httpx: source has no current URL"))
	}
	if spec.Resource != nil && spec.Resource.Size < -1 {
		return fail(domain.InvalidArgument("httpx: invalid resource size %d", spec.Resource.Size))
	}
	return newDownload(e, spec, attach, probeSrc, probeRes), nil
}
