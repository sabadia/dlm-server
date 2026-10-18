package httpx

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/engine/retry"
	"github.com/sabadia/dlm-server/internal/platform"
	"github.com/sabadia/dlm-server/internal/settings"
)

// PartSuffix is appended to the destination path while downloading.
const PartSuffix = ".part"

// Sentinel errors carried (wrapped in an *engine.Error) by Download.Run.
var (
	// ErrDestinationExists: the final path already exists. The file-exists
	// policy runs before the engine, so the engine never overwrites.
	ErrDestinationExists = errors.New("httpx: destination file already exists")
	// ErrRangeMismatch: a 206 response's Content-Range does not match the
	// requested range or the known size, or the server sent more data than
	// the known size.
	ErrRangeMismatch = errors.New("httpx: response range does not match the request")
	// ErrRangeIgnored: a range request was answered with 200 (the If-Range
	// validator failed because the file changed, or the server stopped
	// honouring ranges) and the refresh.onMismatch policy is not restart.
	ErrRangeIgnored = errors.New("httpx: server ignored the range request")
	// ErrSizeMismatch: a full response announced a different size than the
	// known one and the refresh.onMismatch policy is not restart.
	ErrSizeMismatch = errors.New("httpx: response size differs from the known size")
	// ErrStalled: a connection received no data for the stall timeout. It
	// is classified transient (network).
	ErrStalled = errors.New("httpx: connection stalled")
)

// Download is one run of an HTTP download; see the package documentation
// for the design. Build it with Engine.NewDownload.
type Download struct {
	e        *Engine
	spec     engine.Spec
	log      *slog.Logger
	policy   retry.Policy
	maxConns int
	partPath string

	// probeSrc and probeRes come from spec.Probe, if any.
	probeSrc *domain.Source
	probeRes *domain.Resource

	started    atomic.Bool
	downloaded atomic.Int64
	size       atomic.Int64
	conns      atomic.Int32

	// Owned by the goroutine running Run.
	client    *http.Client
	f         *os.File
	attach    *ProbeResult
	res       domain.Resource
	restoring bool
	lastCkpt  time.Time

	mu         sync.Mutex
	src        domain.Source
	srcChanged bool
	// tab is the segment table of a ranged run; nil when sequential.
	tab *segTable
	// ckptReady is set once the .part file state is established; before
	// that no checkpoint is written so a failed start never erases the
	// stored resume state.
	ckptReady bool
	// mirrors spreads ranged requests over verified mirrors; nil without any.
	mirrors *mirrorSet
	// held is the 200 response kept for an onMismatch=restart switch to
	// sequential mode.
	held *stream
}

var _ engine.Download = (*Download)(nil)

func newDownload(e *Engine, spec engine.Spec, attach *ProbeResult, probeSrc *domain.Source, probeRes *domain.Resource) *Download {
	d := &Download{
		e:        e,
		spec:     spec,
		log:      e.env.Logger.With("engine", EngineName, "task_id", spec.TaskID),
		policy:   retry.FromSettings(spec.Settings.Retry),
		maxConns: max(spec.Settings.MaxConnections.OrElse(DefaultMaxConnections), 1),
		partPath: spec.Path + PartSuffix,
		probeSrc: probeSrc,
		probeRes: probeRes,
		attach:   attach,
		src:      spec.Source,
	}
	size := int64(-1)
	switch {
	case spec.Resource != nil:
		size = spec.Resource.Size
	case probeRes != nil:
		size = probeRes.Size
	}
	d.size.Store(size)
	var done int64
	for _, s := range spec.Segments {
		done += s.Done
	}
	d.downloaded.Store(done)
	return d
}

// Progress returns the current progress. It is safe to call at any time.
func (d *Download) Progress() engine.Progress {
	return engine.Progress{
		Downloaded:  d.downloaded.Load(),
		Size:        d.size.Load(),
		Connections: int(d.conns.Load()),
	}
}

// Run downloads the file; see the package documentation. It returns nil
// on success, ctx.Err() when ctx is cancelled, otherwise a classified
// error (*engine.Error in the chain). A final checkpoint is written before
// it returns whenever the .part file state was established. Run may be
// called only once.
func (d *Download) Run(ctx context.Context) error {
	if !d.started.CompareAndSwap(false, true) {
		return engine.Fatal(domain.ErrorCodeFatal, errors.New("httpx: download: Run called more than once"))
	}
	defer d.cleanup()
	err := d.run(ctx)
	d.finalCheckpoint(ctx)
	if err != nil && ctx.Err() != nil {
		return ctx.Err() //nolint:wrapcheck // contract: cancellation returns ctx.Err() as is
	}
	return err
}

func (d *Download) run(ctx context.Context) error {
	if d.e.env.Clients == nil {
		return engine.Fatal(domain.ErrorCodeFatal, errors.New("httpx: download: no client factory"))
	}
	client, err := d.e.env.Clients.Client(d.spec.Settings)
	if err != nil {
		return engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("httpx: download: client: %w", err))
	}
	d.client = client
	if err := d.resolve(ctx); err != nil {
		return err
	}
	if err := checkDestination(d.spec.Path); err != nil {
		return err
	}
	dir := filepath.Dir(d.spec.Path)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // download directories are user-visible, like the files
		return d.fileError("create directory", err)
	}
	ranged := d.res.RangeSupported && d.res.Size > 0
	if err := d.openPart(ctx, ranged); err != nil {
		return err
	}
	if ranged {
		d.verifyMirrors(ctx)
		err = d.runRanged(ctx)
		if err != nil && errors.Is(err, ErrRangeIgnored) && d.onMismatch() == settings.MismatchRestart && ctx.Err() == nil {
			err = d.restartSequential(ctx)
		}
	} else {
		err = d.runSequential(ctx, d.attachmentStream(ctx, false))
	}
	if err != nil {
		return err
	}
	return d.finalize()
}

// resolve establishes the resource and source: from the spec, from the
// probe handed to NewDownload, or by probing now.
func (d *Download) resolve(ctx context.Context) error {
	switch {
	case d.spec.Resource != nil:
		d.res = *d.spec.Resource
	case d.probeRes != nil:
		d.res = *d.probeRes
	default:
		var pr *ProbeResult
		err := retry.Do(ctx, d.policy, func(ctx context.Context, _ int) error {
			var err error
			pr, err = Probe(ctx, d.client, d.source(), d.spec.Settings)
			if err != nil {
				return d.e.classifier.Classify(err)
			}
			return nil
		}, retry.WithSleeper(d.e.opts.sleep), retry.OnRetry(d.logRetry(-1)))
		if err != nil {
			return err
		}
		d.attach = pr
		d.res = pr.Resource
		src := pr.Source
		d.probeSrc = &src
	}
	d.size.Store(d.res.Size)
	if d.probeSrc != nil && d.probeSrc.RedirectURL != "" {
		d.updateSource(func(s *domain.Source) { s.RedirectURL = d.probeSrc.RedirectURL })
	}
	return nil
}

// openPart opens the .part file: it reuses it when the restored segments
// are valid and the file has the expected length, otherwise it starts over
// with an empty file. It then checks free space and preallocates.
func (d *Download) openPart(ctx context.Context, ranged bool) error {
	size := d.res.Size
	if ranged && len(d.spec.Segments) > 0 {
		if segs, ok := validateSegments(d.spec.Segments, size); ok {
			d.restoring = d.reopenPart(size)
			if d.restoring {
				d.setTable(newSegTable(segs, size, d.e.opts.minSplit))
			}
		}
		if !d.restoring {
			d.log.InfoContext(ctx, "httpx: restored state invalid; starting over", "segments", len(d.spec.Segments))
		}
	}
	if !d.restoring {
		f, err := os.OpenFile(d.partPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644) //nolint:gosec // path chosen by the user; downloads are user-visible files
		if err != nil {
			return d.fileError("create", err)
		}
		d.f = f
		if ranged {
			d.setTable(newSegTable(nil, size, d.e.opts.minSplit))
		}
		// Fresh start: the fingerprint learnt by the probe now describes
		// the file. A restored download keeps the fingerprint its bytes
		// were downloaded with, so If-Range detects a changed file.
		if d.probeSrc != nil {
			fp := d.probeSrc.Fingerprint
			d.updateSource(func(s *domain.Source) { s.Fingerprint = fp })
		}
	}
	var done int64
	if t := d.table(); t != nil {
		_, done = t.snapshot()
	}
	d.downloaded.Store(done)
	d.mu.Lock()
	d.ckptReady = true
	d.mu.Unlock()

	if size <= 0 {
		return nil
	}
	if err := d.checkFreeSpace(ctx, size-done); err != nil {
		return err
	}
	if !d.restoring {
		if err := d.e.opts.preallocate(d.f, size); err != nil {
			return d.fileError("preallocate", err)
		}
	}
	return nil
}

// reopenPart opens an existing .part file of exactly size bytes.
func (d *Download) reopenPart(size int64) bool {
	f, err := os.OpenFile(d.partPath, os.O_RDWR, 0)
	if err != nil {
		return false
	}
	fi, err := f.Stat()
	if err != nil || fi.Size() != size {
		_ = f.Close() // unusable; a fresh file replaces it
		return false
	}
	d.f = f
	return true
}

func (d *Download) checkFreeSpace(ctx context.Context, need int64) error {
	if need <= 0 {
		return nil
	}
	dir := filepath.Dir(d.spec.Path)
	free, err := d.e.opts.freeSpace(dir)
	if err != nil {
		d.log.DebugContext(ctx, "httpx: free space unknown; skipping precheck", "error", err)
		return nil
	}
	if free < uint64(need) {
		return engine.Fatal(domain.ErrorCodeDiskFull,
			fmt.Errorf("httpx: not enough free space in %q: need %d bytes, %d available", dir, need, free))
	}
	return nil
}

// restartSequential switches a ranged run to a fresh sequential download
// from offset 0 after a 200 answer to a range request (onMismatch=restart),
// streaming that 200 response when it could be kept.
func (d *Download) restartSequential(ctx context.Context) error {
	d.mu.Lock()
	st := d.held
	d.held = nil
	d.tab = nil
	d.mu.Unlock()
	d.log.InfoContext(ctx, "httpx: server ignored range request; restarting sequentially")
	d.downloaded.Store(0)
	size := int64(-1)
	if st != nil {
		size = st.resp.ContentLength
		fp := fingerprintOf(st.resp, size)
		d.updateSource(func(s *domain.Source) {
			fp.Filename, fp.ContentType = s.Fingerprint.Filename, s.Fingerprint.ContentType
			s.Fingerprint = fp
		})
	}
	d.size.Store(size)
	if err := d.f.Truncate(0); err != nil {
		st.close()
		return d.fileError("truncate", err)
	}
	if size > 0 {
		if err := d.e.opts.preallocate(d.f, size); err != nil {
			st.close()
			return d.fileError("preallocate", err)
		}
	}
	return d.runSequential(ctx, st)
}

// finalize verifies the byte count, flushes and closes the .part file and
// renames it into place.
func (d *Download) finalize() error {
	size := d.size.Load()
	var written int64
	if t := d.table(); t != nil {
		if !t.complete() {
			return engine.Fatal(domain.ErrorCodeFatal, errors.New("httpx: download ended with incomplete segments"))
		}
		_, written = t.snapshot()
	} else {
		written = d.downloaded.Load()
		if err := d.f.Truncate(written); err != nil {
			return d.fileError("truncate", err)
		}
	}
	if size >= 0 && written != size {
		return engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("httpx: downloaded %d bytes, expected %d", written, size))
	}
	d.size.Store(written)
	d.downloaded.Store(written)
	if err := d.f.Sync(); err != nil {
		return d.fileError("sync", err)
	}
	f := d.f
	d.f = nil
	if err := f.Close(); err != nil {
		return d.fileError("close", err)
	}
	if err := checkDestination(d.spec.Path); err != nil {
		return err
	}
	if err := os.Rename(d.partPath, d.spec.Path); err != nil {
		return d.fileError("rename", err)
	}
	return nil
}

func checkDestination(path string) error {
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("%w: %q", ErrDestinationExists, path))
	case errors.Is(err, fs.ErrNotExist):
		return nil
	default:
		return engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("httpx: stat %q: %w", path, err))
	}
}

// fileError classifies a local file error: disk full or fatal.
func (d *Download) fileError(op string, err error) error {
	err = fmt.Errorf("httpx: %s %q: %w", op, d.partPath, err)
	if platform.IsDiskFull(err) {
		return engine.Fatal(domain.ErrorCodeDiskFull, err)
	}
	return engine.Fatal(domain.ErrorCodeFatal, err)
}

func (d *Download) cleanup() {
	_ = d.attach.Close() // unused probe response
	d.mu.Lock()
	held := d.held
	d.held = nil
	d.mu.Unlock()
	held.close()
	if d.f != nil {
		_ = d.f.Close() // error path: the .part file stays for resume
		d.f = nil
	}
	d.conns.Store(0)
}

func (d *Download) onMismatch() settings.MismatchPolicy {
	return d.spec.Settings.Refresh.OnMismatch.OrElse(settings.MismatchFail)
}

func (d *Download) table() *segTable {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.tab
}

func (d *Download) setTable(t *segTable) {
	d.mu.Lock()
	d.tab = t
	d.mu.Unlock()
}

func (d *Download) source() domain.Source {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.src
}

func (d *Download) updateSource(fn func(*domain.Source)) {
	d.mu.Lock()
	fn(&d.src)
	d.srcChanged = true
	d.mu.Unlock()
}

// requestURL is the URL ranged and sequential requests go to: the probed
// redirect target when known, else the current URL.
func (d *Download) requestURL() string {
	s := d.source()
	if s.RedirectURL != "" {
		return s.RedirectURL
	}
	return s.CurrentURL
}

// ifRange returns the If-Range validator: the strong ETag, else the
// Last-Modified date, else "".
func (d *Download) ifRange() string {
	fp := d.source().Fingerprint
	switch {
	case fp.ETag != "":
		return fp.ETag
	case fp.LastModified != nil:
		return fp.LastModified.UTC().Format(http.TimeFormat)
	default:
		return ""
	}
}

func (d *Download) logRetry(segment int) func(int, time.Duration, error) {
	return func(attempt int, delay time.Duration, err error) {
		d.log.Warn("httpx: retrying after transient error",
			"segment", segment, "attempt", attempt, "delay", delay, "error", err)
	}
}

// snapshotCheckpoint returns a consistent checkpoint, or false while the
// file state is not established.
func (d *Download) snapshotCheckpoint() (engine.Checkpoint, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.ckptReady {
		return engine.Checkpoint{}, false
	}
	cp := engine.Checkpoint{Size: d.size.Load()}
	if d.tab != nil {
		cp.Segments, cp.Downloaded = d.tab.snapshot()
	} else {
		cp.Downloaded = d.downloaded.Load()
		if cp.Size > 0 {
			cp.Segments = []domain.Segment{{Start: 0, End: cp.Size, Done: min(cp.Downloaded, cp.Size)}}
		}
	}
	if d.srcChanged {
		s := d.src
		cp.Source = &s
	}
	return cp, true
}

// checkpoint writes a checkpoint; failures are logged, never fatal.
func (d *Download) checkpoint(ctx context.Context) {
	if d.spec.Checkpointer == nil {
		return
	}
	cp, ok := d.snapshotCheckpoint()
	if !ok {
		return
	}
	d.lastCkpt = d.e.env.Clock()
	if err := d.spec.Checkpointer.Checkpoint(ctx, cp); err != nil {
		d.log.WarnContext(ctx, "httpx: checkpoint failed", "error", err)
	}
}

// maybeCheckpoint writes a checkpoint when the interval has elapsed.
func (d *Download) maybeCheckpoint(ctx context.Context) {
	if d.e.env.Clock().Sub(d.lastCkpt) >= d.e.opts.checkpointIntv {
		d.checkpoint(ctx)
	}
}

// finalCheckpoint writes the last checkpoint, with a short detached
// context when ctx is already cancelled.
func (d *Download) finalCheckpoint(ctx context.Context) {
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), finalCheckpointTimeout)
		defer cancel()
	}
	d.checkpoint(ctx)
}

// fingerprintOf builds a fingerprint from a full response.
func fingerprintOf(resp *http.Response, size int64) domain.Fingerprint {
	return domain.Fingerprint{
		Size:         size,
		ETag:         strongETag(resp.Header.Get("ETag")),
		LastModified: lastModified(resp.Header.Get("Last-Modified")),
	}
}
