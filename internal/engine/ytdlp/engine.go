package ytdlp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/ffmpeg"
	"github.com/sabadia/dlm-server/internal/platform"
	"github.com/sabadia/dlm-server/internal/ytdlp"
)

// EngineName is the name of the yt-dlp engine, stored on tasks.
const EngineName = "ytdlp"

// Defaults for the tunables.
const (
	DefaultCheckpointInterval = time.Second
	// maxInfoBytes bounds the JSON `yt-dlp -J` prints.
	maxInfoBytes = 64 << 20
	// interruptGrace is how long yt-dlp gets to stop after an interrupt
	// before it is killed.
	interruptGrace = 10 * time.Second
	// finalCheckpointTimeout bounds the last checkpoint after a cancellation.
	finalCheckpointTimeout = 5 * time.Second
)

// Locator provides the yt-dlp executable (ytdlp.Locator).
type Locator interface {
	Locate(ctx context.Context) (ytdlp.Binary, error)
}

// FFmpeg provides the ffmpeg executable (ffmpeg.Locator).
type FFmpeg interface {
	Locate(ctx context.Context) (ffmpeg.Binary, error)
}

// CookieSource returns the stored cookies a request to u would carry.
type CookieSource func(ctx context.Context, u *url.URL) ([]*domain.Cookie, error)

type options struct {
	cookies        CookieSource
	tempDir        string
	checkpointIntv time.Duration
	freeSpace      func(dir string) (uint64, error)
}

// Option configures an Engine.
type Option func(*options)

// WithCookies lets yt-dlp use the stored cookies of the site.
func WithCookies(c CookieSource) Option { return func(o *options) { o.cookies = c } }

// WithTempDir sets where the temporary cookie files are made (default: the OS
// temporary directory).
func WithTempDir(dir string) Option { return func(o *options) { o.tempDir = dir } }

// WithCheckpointInterval sets the minimum time between periodic checkpoints
// (default 1s).
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

// Engine runs yt-dlp. It is safe for concurrent use.
type Engine struct {
	env  engine.Env
	bin  Locator
	ff   FFmpeg
	opts options
}

var _ engine.Engine = (*Engine)(nil)

// New returns the yt-dlp engine. ff may be nil, which leaves merging video and
// audio to an ffmpeg yt-dlp finds itself.
func New(env engine.Env, bin Locator, ff FFmpeg, opts ...Option) *Engine {
	if env.Logger == nil {
		env.Logger = slog.New(slog.DiscardHandler)
	}
	if env.Clock == nil {
		env.Clock = time.Now
	}
	o := options{checkpointIntv: DefaultCheckpointInterval, freeSpace: platform.FreeSpace}
	for _, opt := range opts {
		opt(&o)
	}
	return &Engine{env: env, bin: bin, ff: ff, opts: o}
}

// Name returns "ytdlp".
func (*Engine) Name() string { return EngineName }

// Match accepts http(s) sources whose resolver is yt-dlp.
func (*Engine) Match(src domain.Source) bool {
	if src.ResolverKind != domain.ResolverYtdlp {
		return false
	}
	u, err := url.Parse(strings.TrimSpace(src.CurrentURL))
	if err != nil || u.Host == "" {
		return false
	}
	s := strings.ToLower(u.Scheme)
	return s == "http" || s == "https"
}

// Resolve runs `yt-dlp -J` for the source and describes the file it would
// download.
func (e *Engine) Resolve(ctx context.Context, req engine.Request) (*engine.Probe, error) {
	bin, err := e.binary(ctx)
	if err != nil {
		return nil, err
	}
	cs, err := e.prepare(ctx, req.Source, req.Settings)
	if err != nil {
		return nil, err
	}
	defer cs.cleanup()
	args := append(cs.args(), "--dump-single-json", "--no-playlist", "--skip-download")
	args = append(args, "--", strings.TrimSpace(req.Source.CurrentURL))
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, bin.Path, args...) //nolint:gosec // the user's or our own yt-dlp
	cmd.Stdout = &limitWriter{w: &stdout, n: maxInfoBytes}
	cmd.Stderr = &limitWriter{w: &stderr, n: 1 << 20}
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err() //nolint:wrapcheck // cancellation is reported as is
		}
		return nil, classifyFailure(stderr.String(), err)
	}
	info, err := parseInfo(stdout.Bytes())
	if err != nil {
		return nil, engine.Fatal(domain.ErrorCodeFatal, err)
	}
	name := info.fileName()
	src := req.Source
	src.Fingerprint = domain.Fingerprint{Size: info.size(), Filename: name}
	return &engine.Probe{
		Engine: EngineName, Source: src, Suggested: name,
		Resource: domain.Resource{Name: name, Size: info.size(), Files: []domain.File{{Index: 0, Name: name, Size: info.size(), Selected: true}}},
	}, nil
}

// NewDownload validates spec and prepares a download without doing I/O.
func (e *Engine) NewDownload(spec engine.Spec) (engine.Download, error) {
	_ = spec.Probe.Close() // nil-safe; nothing is attached by this engine
	if spec.Path == "" || !filepath.IsAbs(spec.Path) {
		return nil, engine.Fatal(domain.ErrorCodeFatal, domain.InvalidArgument("ytdlp: download path %q must be absolute", spec.Path))
	}
	if !e.Match(spec.Source) {
		return nil, engine.Fatal(domain.ErrorCodeFatal, domain.InvalidArgument("ytdlp: %q is not a yt-dlp source", spec.Source.CurrentURL))
	}
	return newDownload(e, spec), nil
}

func (e *Engine) binary(ctx context.Context) (ytdlp.Binary, error) {
	if e.bin == nil {
		return ytdlp.Binary{}, engine.Fatal(domain.ErrorCodeFatal, errors.New("ytdlp: no yt-dlp locator"))
	}
	b, err := e.bin.Locate(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ytdlp.Binary{}, ctx.Err() //nolint:wrapcheck // cancellation is reported as is
		}
		return ytdlp.Binary{}, engine.Fatal(domain.ErrorCodeFatal, err)
	}
	return b, nil
}

// ErrDestinationExists is wrapped by the error of a download whose final path
// already exists (the file-exists policy runs before the engine).
var ErrDestinationExists = errors.New("ytdlp: destination file already exists")

// limitWriter stops accepting data after n bytes: yt-dlp is not trusted to
// print a bounded amount.
type limitWriter struct {
	w io.Writer
	n int64
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > l.n {
		return 0, errors.New("ytdlp: output too large")
	}
	l.n -= int64(len(p))
	return l.w.Write(p) //nolint:wrapcheck // a buffer write
}
