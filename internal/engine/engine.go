// Package engine defines the contract every protocol engine (http, ftp, hls,
// dash, bt, ed2k, ytdlp, browser) implements, the registry that selects an
// engine for a source, the resolve cache, and the error classification that
// drives retries and link refresh.
//
// Lifecycle: Resolve probes a source and returns a Probe (resource metadata
// plus an optional attachment such as an open one-time HTTP response). A
// Download is then built from a Spec and Run until it completes, fails, or
// its context is cancelled (pause). Engines never persist anything
// themselves: they report progress through Progress and durable state
// through the Checkpointer.
package engine

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/settings"
)

// Engine resolves and downloads sources of one protocol family.
type Engine interface {
	// Name is the stable engine identifier stored on tasks (e.g. "http").
	Name() string
	// Match reports whether the engine can handle src. The registry asks
	// engines in order and picks the first match, so specific engines (hls)
	// are registered before generic ones (http).
	Match(src domain.Source) bool
	// Resolve probes the source and describes what would be downloaded.
	// The returned Probe must be closed by the caller (or handed to
	// NewDownload through Spec.Probe, which takes ownership of it).
	Resolve(ctx context.Context, req Request) (*Probe, error)
	// NewDownload prepares a download. It must not perform I/O; all work
	// happens in Download.Run.
	NewDownload(spec Spec) (Download, error)
}

// Request is the input of Resolve.
type Request struct {
	Source domain.Source
	// Settings are the effective (fully resolved) settings.
	Settings settings.Settings
}

// Spec describes one download run of a task.
type Spec struct {
	TaskID string
	Source domain.Source
	// Resource is the resolved resource, if known.
	Resource *domain.Resource
	// Path is the absolute path of the final file (or directory for
	// multi-file resources). Engines write to temporary files next to it and
	// move them into place on completion.
	Path string
	// Settings are the effective (fully resolved) settings.
	Settings settings.Settings
	// Segments and EngineState restore a previous run; both may be empty.
	Segments    []domain.Segment
	EngineState []byte
	// Probe, when non-nil, is a fresh probe for this source whose attachment
	// the engine may take over (e.g. continue reading a one-time response).
	// NewDownload takes ownership and must close it.
	Probe *Probe
	// Limiter bounds the transfer rate; nil means unlimited.
	Limiter Limiter
	// Checkpointer receives durable state; nil disables checkpoints.
	Checkpointer Checkpointer
}

// Download is one run of a task. It is not reusable: after Run returns,
// build a new Download to resume.
type Download interface {
	// Run transfers data until the download completes (nil), fails (an
	// error, ideally an *Error), or ctx is cancelled (ctx.Err(), used for
	// pause). Run writes a final checkpoint before returning when possible.
	Run(ctx context.Context) error
	// Progress returns a consistent snapshot. Safe to call concurrently with
	// Run, including before Run starts and after it returns.
	Progress() Progress
}

// Progress is a point-in-time snapshot of a download.
type Progress struct {
	// Downloaded is the number of bytes durably written.
	Downloaded int64
	// Size is the total size, or -1 when unknown.
	Size int64
	// Connections is the number of active transfer connections.
	Connections int
	// Uploaded is the number of bytes uploaded since the download started
	// (torrents; 0 for other engines).
	Uploaded int64
	// Peers is the number of connected peers (torrents; 0 otherwise).
	Peers int
}

// Inspectable is implemented by downloads that can describe their swarm
// while they run (torrents).
type Inspectable interface {
	// Torrent describes the running transfer; ok is false until the
	// transfer has joined its swarm.
	Torrent() (info *domain.TorrentInfo, ok bool)
}

// Describer is implemented by engines that can describe a task that is not
// running from the state its last run saved.
type Describer interface {
	// Describe returns what is known about the task from its saved engine
	// state and resource; ok is false when nothing is saved yet.
	Describe(state []byte, res *domain.Resource, set settings.Settings) (info *domain.TorrentInfo, ok bool)
}

// Checkpoint is durable resume state written by an engine.
type Checkpoint struct {
	Downloaded int64
	Size       int64
	Segments   []domain.Segment
	// EngineState is opaque, engine-defined resume data.
	EngineState []byte
	// Source carries updates discovered during the run (redirect URL,
	// fingerprint), or nil when unchanged.
	Source *domain.Source
}

// Checkpointer persists checkpoints. Engines call it periodically (the
// caller throttles writes) and once before Run returns.
type Checkpointer interface {
	Checkpoint(ctx context.Context, cp Checkpoint) error
}

// Limiter is a byte-rate limiter; ratelimit chains satisfy it.
type Limiter interface {
	// WaitN blocks until n bytes may be transferred or ctx is done.
	WaitN(ctx context.Context, n int) error
}

// ClientFactory builds HTTP clients honouring the effective settings
// (proxy, TLS verification, user agent, headers, cookies). Engines must
// obtain clients from it rather than constructing their own.
type ClientFactory interface {
	Client(s settings.Settings) (*http.Client, error)
}

// Env holds dependencies shared by engines.
type Env struct {
	Logger  *slog.Logger
	Clock   func() time.Time
	Clients ClientFactory
}

// Probe is the result of Resolve.
type Probe struct {
	// Engine is the name of the engine that produced the probe.
	Engine string
	// Source is the input source updated with what the probe learnt
	// (redirect URL, fingerprint).
	Source domain.Source
	// Resource describes what would be downloaded.
	Resource domain.Resource
	// Suggested is the suggested file name.
	Suggested string
	// Formats are the qualities the stream offers (a picker); nil for a plain file.
	Formats []domain.Format

	mu         sync.Mutex
	attachment io.Closer
	closed     bool
}

// Attach stores engine-private state (such as an open response) on the
// probe. A previous attachment is closed.
func (p *Probe) Attach(c io.Closer) {
	p.mu.Lock()
	prev := p.attachment
	if p.closed {
		p.mu.Unlock()
		if c != nil {
			_ = c.Close() // probe already closed: release immediately
		}
		return
	}
	p.attachment = c
	p.mu.Unlock()
	if prev != nil {
		_ = prev.Close() // replaced attachment is no longer reachable
	}
}

// TakeAttachment transfers ownership of the attachment to the caller and
// returns nil if there is none or it was already taken.
func (p *Probe) TakeAttachment() io.Closer {
	p.mu.Lock()
	defer p.mu.Unlock()
	c := p.attachment
	p.attachment = nil
	return c
}

// Close releases the attachment, if any. It is idempotent and safe to call
// on a nil probe.
func (p *Probe) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	c := p.attachment
	p.attachment = nil
	p.closed = true
	p.mu.Unlock()
	if c == nil {
		return nil
	}
	return c.Close() //nolint:wrapcheck // attachment errors are engine errors already
}
