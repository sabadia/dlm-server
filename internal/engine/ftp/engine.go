package ftp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/engine/httpx"
	"github.com/sabadia/dlm-server/internal/engine/retry"
	"github.com/sabadia/dlm-server/internal/platform"
)

// EngineName is the name of the FTP engine, stored on tasks.
const EngineName = "ftp"

// Defaults for the tunables (see the With* options).
const (
	DefaultStallTimeout       = 30 * time.Second
	DefaultControlTimeout     = 30 * time.Second
	DefaultCheckpointInterval = time.Second
	bufferSize                = 64 << 10
	finalCheckpointTimeout    = 5 * time.Second
	// PartSuffix is appended to the destination path while downloading.
	PartSuffix = ".part"
)

type options struct {
	stallTimeout   time.Duration
	controlTimeout time.Duration
	checkpointIntv time.Duration
	sleep          retry.Sleeper
	freeSpace      func(dir string) (uint64, error)
	preallocate    func(f *os.File, size int64) error
}

// Option configures an Engine.
type Option func(*options)

// WithStallTimeout sets how long a data connection may wait for bytes
// before the attempt is retried as a network error (default 30s).
func WithStallTimeout(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.stallTimeout = d
		}
	}
}

// WithControlTimeout sets how long one read or write on the control
// connection (including the greeting and TLS handshake) may take (default
// 30s).
func WithControlTimeout(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.controlTimeout = d
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

// Engine is the FTP engine. It is safe for concurrent use.
type Engine struct {
	env  engine.Env
	opts options
}

var _ engine.Engine = (*Engine)(nil)

// New returns the FTP engine. A nil Logger discards logs and a nil Clock
// uses time.Now. The engine opens its own control connections, so
// env.Clients is not used.
func New(env engine.Env, opts ...Option) *Engine {
	if env.Logger == nil {
		env.Logger = slog.New(slog.DiscardHandler)
	}
	if env.Clock == nil {
		env.Clock = time.Now
	}
	o := options{
		stallTimeout:   DefaultStallTimeout,
		controlTimeout: DefaultControlTimeout,
		checkpointIntv: DefaultCheckpointInterval,
		sleep:          retry.Sleep,
		freeSpace:      platform.FreeSpace,
		preallocate:    platform.Preallocate,
	}
	for _, opt := range opts {
		opt(&o)
	}
	return &Engine{env: env, opts: o}
}

// Name returns "ftp".
func (*Engine) Name() string { return EngineName }

// Match accepts ftp, ftps and ftpes URLs.
func (*Engine) Match(src domain.Source) bool {
	u, err := url.Parse(strings.TrimSpace(src.CurrentURL))
	if err != nil || u.Host == "" {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "ftp", "ftps", "ftpes":
		return true
	default:
		return false
	}
}

// stat is what the server says about the file.
type stat struct {
	size    int64 // -1 when the server has no SIZE
	modTime *time.Time
}

// statFile asks for the size and modification time. A server without SIZE or
// MDTM just leaves them unknown.
func statFile(c *conn, t target) (stat, error) {
	st := stat{size: -1}
	size, err := c.FileSize(t.remotePath())
	switch {
	case err == nil:
		st.size = size
	case unsupported(err):
	default:
		return st, classify(fmt.Errorf("ftp: size of %s: %w", t.redacted(), err))
	}
	if c.IsGetTimeSupported() {
		if mt, err := c.GetTime(t.remotePath()); err == nil && !mt.IsZero() {
			mt = mt.UTC()
			st.modTime = &mt
		}
	}
	return st, nil
}

func fileName(t target) string {
	base := path.Base("/" + t.path)
	if decoded, err := url.PathUnescape(base); err == nil {
		base = decoded
	}
	if name := httpx.SanitizeFilename(base); name != "" {
		return name
	}
	return httpx.SanitizeFilename(t.host)
}

func checkFile(t target) error {
	if t.path == "" || strings.HasSuffix(t.path, "/") {
		return engine.Fatal(domain.ErrorCodeFatal, domain.InvalidArgument("ftp: %s is a directory; only single files can be downloaded", t.redacted()))
	}
	return nil
}

// Resolve connects, logs in and asks for the size and modification time of
// the file.
func (e *Engine) Resolve(ctx context.Context, req engine.Request) (*engine.Probe, error) {
	t, err := parseTarget(req.Source.CurrentURL)
	if err != nil {
		return nil, engine.Fatal(domain.ErrorCodeFatal, err)
	}
	if err := checkFile(t); err != nil {
		return nil, err
	}
	var st stat
	err = retry.Do(ctx, retry.FromSettings(req.Settings.Retry), func(ctx context.Context, _ int) error {
		c, err := open(ctx, t, req.Settings, e.opts.controlTimeout)
		if err != nil {
			return err
		}
		defer closeConn(c)
		st, err = statFile(c, t)
		return err
	}, retry.WithSleeper(e.opts.sleep))
	if err != nil {
		return nil, err //nolint:wrapcheck // classified by the attempt
	}
	name := fileName(t)
	src := req.Source
	src.Fingerprint = domain.Fingerprint{Size: st.size, LastModified: st.modTime, Filename: name}
	return &engine.Probe{
		Engine: EngineName, Source: src, Suggested: name,
		Resource: domain.Resource{
			Name: name, Size: st.size, RangeSupported: st.size > 0,
			Files: []domain.File{{Index: 0, Name: name, Size: st.size, Selected: true}},
		},
	}, nil
}

// NewDownload validates spec and prepares a download without doing I/O. It
// takes ownership of spec.Probe, which this engine does not need to keep.
func (e *Engine) NewDownload(spec engine.Spec) (engine.Download, error) {
	_ = spec.Probe.Close() // nil-safe; nothing is attached by this engine
	if spec.Path == "" || !filepath.IsAbs(spec.Path) {
		return nil, engine.Fatal(domain.ErrorCodeFatal, domain.InvalidArgument("ftp: download path %q must be absolute", spec.Path))
	}
	t, err := parseTarget(spec.Source.CurrentURL)
	if err != nil {
		return nil, engine.Fatal(domain.ErrorCodeFatal, err)
	}
	if err := checkFile(t); err != nil {
		return nil, err
	}
	return newDownload(e, spec, t), nil
}

var errDestinationExists = errors.New("ftp: destination file already exists")

// ErrDestinationExists is wrapped by the error of a download whose final path
// already exists (the file-exists policy runs before the engine).
var ErrDestinationExists = errDestinationExists

// ErrContentChanged is wrapped by the error of a resumed download whose file
// changed on the server and refresh.onMismatch is not restart.
var ErrContentChanged = errors.New("ftp: the file changed on the server")
