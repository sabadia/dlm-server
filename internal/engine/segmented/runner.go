package segmented

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/engine/retry"
	"github.com/sabadia/dlm-server/internal/ffmpeg"
	"github.com/sabadia/dlm-server/internal/settings"
)

const (
	defaultWorkers            = 8
	maxWorkers                = 16
	defaultCheckpointInterval = time.Second
	minLiveReload             = time.Second
)

// FFmpeg provides the ffmpeg executable (ffmpeg.Locator).
type FFmpeg interface {
	Locate(ctx context.Context) (ffmpeg.Binary, error)
}

// Exec runs an external program to completion; it returns an error carrying
// the program's complaint when it fails.
type Exec func(ctx context.Context, bin string, args []string) error

type options struct {
	exec     Exec
	sleep    retry.Sleeper
	ckptIntv time.Duration
	// liveWait overrides how long a live download waits before reloading
	// (tests).
	liveWait func(target time.Duration) time.Duration
}

// Option configures a Runner.
type Option func(*options)

// WithExec replaces the way ffmpeg is run (tests).
func WithExec(e Exec) Option { return func(o *options) { o.exec = e } }

// WithRetrySleeper replaces the retry back-off sleeper (tests).
func WithRetrySleeper(s retry.Sleeper) Option { return func(o *options) { o.sleep = s } }

// WithCheckpointInterval sets how often progress is checkpointed (default 1s).
func WithCheckpointInterval(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.ckptIntv = d
		}
	}
}

// WithLiveWait overrides the wait before a live stream is reloaded (default:
// half the target duration, at least a second).
func WithLiveWait(f func(target time.Duration) time.Duration) Option {
	return func(o *options) { o.liveWait = f }
}

// Frontend reads the description of one kind of stream.
type Frontend interface {
	// Build reads the stream at c.Source.CurrentURL and returns its tracks. It
	// is called by Resolve and at the start of every run.
	Build(ctx context.Context, c Client) (*Plan, error)
	// Reload reads the current segments of one track again (fresh URLs after
	// an expiry, new segments of a live stream). Its Closed field says
	// whether the stream has ended.
	Reload(ctx context.Context, c Client, t *Track) (*Media, error)
}

// Runner creates downloads for the protocol engines. ff may be nil, which only
// allows a single MPEG-TS track saved as .ts (concatenated without ffmpeg).
type Runner struct {
	env  engine.Env
	ff   FFmpeg
	opts options
}

// NewRunner returns a Runner.
func NewRunner(env engine.Env, ff FFmpeg, opts ...Option) *Runner {
	o := options{exec: runExec, sleep: retry.Sleep, ckptIntv: defaultCheckpointInterval}
	for _, opt := range opts {
		opt(&o)
	}
	if env.Clock == nil {
		env.Clock = time.Now
	}
	return &Runner{env: env, ff: ff, opts: o}
}

// Client returns the HTTP client of the task's settings together with the
// source and settings every request carries.
func (r *Runner) Client(src domain.Source, s settings.Settings) (Client, error) {
	if r.env.Clients == nil {
		return Client{}, engine.Fatal(domain.ErrorCodeFatal, errors.New("segmented: no client factory"))
	}
	hc, err := r.env.Clients.Client(s)
	if err != nil {
		return Client{}, engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("segmented: client: %w", err))
	}
	return Client{HTTP: hc, Source: src, Settings: s}, nil
}

// Resolve checks that the stream can be read; it returns the Plan so callers
// can inspect it.
func (r *Runner) Resolve(ctx context.Context, fe Frontend, src domain.Source, s settings.Settings) (*Plan, error) {
	c, err := r.Client(src, s)
	if err != nil {
		return nil, err
	}
	return fe.Build(ctx, c)
}

// NewDownload creates a download of spec through fe.
func (r *Runner) NewDownload(spec engine.Spec, fe Frontend) engine.Download {
	return newDownload(r, spec, fe)
}
