package ftp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/engine/retry"
	"github.com/sabadia/dlm-server/internal/platform"
	"github.com/sabadia/dlm-server/internal/ratelimit"
	"github.com/sabadia/dlm-server/internal/settings"
)

// Download is one run of an FTP download; build it with Engine.NewDownload.
type Download struct {
	e        *Engine
	spec     engine.Spec
	t        target
	log      *slog.Logger
	policy   retry.Policy
	partPath string

	started    atomic.Bool
	downloaded atomic.Int64
	size       atomic.Int64
	conns      atomic.Int32

	// Owned by the goroutine running Run.
	f        *os.File
	lastCkpt time.Time

	mu        sync.Mutex
	src       domain.Source
	srcDirty  bool
	ckptReady bool
}

var _ engine.Download = (*Download)(nil)

func newDownload(e *Engine, spec engine.Spec, t target) *Download {
	d := &Download{
		e: e, spec: spec, t: t,
		log:      e.env.Logger.With("engine", EngineName, "task_id", spec.TaskID),
		policy:   retry.FromSettings(spec.Settings.Retry),
		partPath: spec.Path + PartSuffix,
		src:      spec.Source,
	}
	d.size.Store(-1)
	if spec.Resource != nil {
		d.size.Store(spec.Resource.Size)
	}
	return d
}

// Progress implements engine.Download.
func (d *Download) Progress() engine.Progress {
	return engine.Progress{Downloaded: d.downloaded.Load(), Size: d.size.Load(), Connections: int(d.conns.Load())}
}

// Run downloads the file. It returns nil on success, ctx.Err() when ctx is
// cancelled, otherwise a classified error. Run may be called only once.
func (d *Download) Run(ctx context.Context) error {
	if !d.started.CompareAndSwap(false, true) {
		return engine.Fatal(domain.ErrorCodeFatal, errors.New("ftp: download: Run called more than once"))
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
	if err := checkDestination(d.spec.Path); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(d.spec.Path), 0o755); err != nil { //nolint:gosec // download directories are user-visible, like the files
		return d.fileError("create directory", err)
	}
	prog := &retry.Progress{}
	err := retry.Do(ctx, d.policy, func(ctx context.Context, _ int) error {
		return d.attempt(ctx, prog)
	}, retry.WithSleeper(d.e.opts.sleep), retry.ResetOnProgress(prog.Moved), retry.OnRetry(func(attempt int, delay time.Duration, err error) {
		d.log.Warn("ftp: retrying after transient error", "attempt", attempt, "delay", delay, "error", err)
	}))
	if err != nil {
		return err //nolint:wrapcheck // classified by the attempt
	}
	return d.finalize()
}

// attempt connects once, settles where to continue and transfers the rest.
func (d *Download) attempt(ctx context.Context, prog *retry.Progress) error {
	c, err := open(ctx, d.t, d.spec.Settings, d.e.opts.controlTimeout)
	if err != nil {
		return err
	}
	defer closeConn(c)
	st, err := statFile(c, d.t)
	if err != nil {
		return err
	}
	offset, err := d.prepare(ctx, st)
	if err != nil {
		return err
	}
	if st.size >= 0 && offset == st.size {
		return nil // everything is there already
	}
	err = d.transfer(ctx, c, st, offset, prog)
	if err != nil && offset > 0 && unsupported(err) {
		// The server does not implement REST: start over from byte 0.
		d.log.InfoContext(ctx, "ftp: server refused REST; restarting from the beginning")
		if terr := d.restart(st); terr != nil {
			return terr
		}
		c2, cerr := open(ctx, d.t, d.spec.Settings, d.e.opts.controlTimeout)
		if cerr != nil {
			return cerr
		}
		defer closeConn(c2)
		return d.transfer(ctx, c2, st, 0, prog)
	}
	return err
}

// prepare opens the .part file and returns the offset to continue from. A
// restored download continues when the file on the server is the one its
// bytes came from.
func (d *Download) prepare(ctx context.Context, st stat) (int64, error) {
	if d.f != nil { // a retry: the file is open and downloaded is accurate
		if changed := d.changed(st); changed != "" {
			return 0, d.mismatch(ctx, st, changed)
		}
		return d.downloaded.Load(), nil
	}
	resume := d.restorable(st)
	if resume >= 0 {
		if reason := d.changed(st); reason != "" {
			if err := d.mismatchPolicy(reason); err != nil {
				return 0, err
			}
			resume = -1
		}
	}
	if resume < 0 {
		return 0, d.startFresh(ctx, st)
	}
	f, err := os.OpenFile(d.partPath, os.O_RDWR, 0) //nolint:gosec // path chosen by the user
	if err != nil {
		return 0, d.startFresh(ctx, st)
	}
	d.f = f
	d.size.Store(st.size)
	d.downloaded.Store(resume)
	d.setReady()
	return resume, nil
}

// restorable returns how many bytes of the stored state can be trusted, or -1.
func (d *Download) restorable(st stat) int64 {
	segs := d.spec.Segments
	if st.size <= 0 || len(segs) != 1 {
		return -1
	}
	s := segs[0]
	if s.Start != 0 || s.End != st.size || s.Done < 0 || s.Done > st.size {
		return -1
	}
	fi, err := os.Stat(d.partPath)
	if err != nil || fi.Size() != st.size {
		return -1
	}
	return s.Done
}

// changed compares the server's file with the fingerprint the bytes were
// downloaded with; it returns why they differ, or "".
func (d *Download) changed(st stat) string {
	fp := d.source().Fingerprint
	now := domain.Fingerprint{Size: st.size, LastModified: st.modTime}
	if ok, reason := fp.Matches(now); !ok {
		return reason
	}
	if fp.LastModified != nil && st.modTime != nil && !fp.LastModified.Equal(*st.modTime) {
		return fmt.Sprintf("modification time differs: %s != %s", fp.LastModified.Format(time.RFC3339), st.modTime.Format(time.RFC3339))
	}
	return ""
}

func (d *Download) mismatchPolicy(reason string) error {
	if d.spec.Settings.Refresh.OnMismatch.OrElse(settings.MismatchFail) == settings.MismatchRestart {
		d.log.Info("ftp: the file changed; starting over", "reason", reason)
		return nil
	}
	return engine.LinkExpired(fmt.Errorf("%w (%s)", ErrContentChanged, reason))
}

// mismatch handles a change found on a retry: start over or fail.
func (d *Download) mismatch(ctx context.Context, st stat, reason string) error {
	if err := d.mismatchPolicy(reason); err != nil {
		return err
	}
	return d.startFresh(ctx, st)
}

// startFresh creates an empty, preallocated .part file for the file as the
// server describes it now.
func (d *Download) startFresh(ctx context.Context, st stat) error {
	if d.f != nil {
		_ = d.f.Close() // replaced below
		d.f = nil
	}
	f, err := os.OpenFile(d.partPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644) //nolint:gosec // path chosen by the user; downloads are user-visible files
	if err != nil {
		return d.fileError("create", err)
	}
	d.f = f
	d.downloaded.Store(0)
	d.size.Store(st.size)
	fp := domain.Fingerprint{Size: st.size, LastModified: st.modTime, Filename: d.source().Fingerprint.Filename}
	if fp.Filename == "" {
		fp.Filename = fileName(d.t)
	}
	d.updateSource(func(s *domain.Source) { s.Fingerprint = fp })
	d.setReady()
	if st.size > 0 {
		if err := d.checkFreeSpace(ctx, st.size); err != nil {
			return err
		}
		if err := d.e.opts.preallocate(d.f, st.size); err != nil {
			return d.fileError("preallocate", err)
		}
	}
	return nil
}

// restart truncates the .part file for a download that starts over.
func (d *Download) restart(st stat) error {
	if err := d.f.Truncate(0); err != nil {
		return d.fileError("truncate", err)
	}
	d.downloaded.Store(0)
	if st.size > 0 {
		if err := d.e.opts.preallocate(d.f, st.size); err != nil {
			return d.fileError("preallocate", err)
		}
	}
	return nil
}

func (d *Download) checkFreeSpace(ctx context.Context, need int64) error {
	dir := filepath.Dir(d.spec.Path)
	free, err := d.e.opts.freeSpace(dir)
	if err != nil {
		d.log.DebugContext(ctx, "ftp: free space unknown; skipping precheck", "error", err)
		return nil
	}
	if free < uint64(need) { //nolint:gosec // need is positive here
		return engine.Fatal(domain.ErrorCodeDiskFull, fmt.Errorf("ftp: not enough free space in %q: need %d bytes, %d available", dir, need, free))
	}
	return nil
}

// transfer reads the file from offset to its end into the .part file.
func (d *Download) transfer(ctx context.Context, c *conn, st stat, offset int64, prog *retry.Progress) error {
	resp, err := c.RetrFrom(d.t.remotePath(), uint64(offset)) //nolint:gosec // offset is never negative
	if err != nil {
		return classify(fmt.Errorf("ftp: retrieve %s: %w", d.t.redacted(), err))
	}
	d.conns.Store(1)
	defer d.conns.Store(0)
	stopAbort := context.AfterFunc(ctx, func() { _ = resp.SetDeadline(time.Now()) }) // unblocks a read
	defer stopAbort()

	r := ratelimit.Reader(ctx, resp, d.spec.Limiter)
	buf := make([]byte, bufferSize)
	pos := offset
	var readErr error
	for {
		_ = resp.SetDeadline(time.Now().Add(d.e.opts.stallTimeout)) // stall watchdog
		n, err := r.Read(buf)
		if n > 0 {
			if st.size >= 0 && pos+int64(n) > st.size {
				c.drop()
				_ = resp.Close() // more data than the file has; the server is lying
				return fatalf("ftp: %s sent more than its %d bytes", d.t.redacted(), st.size)
			}
			if _, werr := d.f.WriteAt(buf[:n], pos); werr != nil {
				c.drop()
				_ = resp.Close() // stop the transfer; the write error is what matters
				return d.fileError("write", werr)
			}
			pos += int64(n)
			d.downloaded.Store(pos)
			prog.Add(int64(n))
			d.maybeCheckpoint(ctx)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				readErr = err
			}
			break
		}
	}
	if ctx.Err() != nil || readErr != nil {
		c.drop() // no closing reply will come for an aborted transfer
	}
	closeErr := resp.Close()
	switch {
	case ctx.Err() != nil:
		return ctx.Err() //nolint:wrapcheck // cancellation is reported as is
	case readErr != nil:
		return classify(fmt.Errorf("ftp: read %s: %w", d.t.redacted(), readErr))
	case st.size >= 0 && pos != st.size:
		return engine.Transient(domain.ErrorCodeNetwork, fmt.Errorf("ftp: %s ended after %d of %d bytes", d.t.redacted(), pos, st.size))
	case st.size < 0 && closeErr != nil:
		// Without a size, the server's closing reply is the only sign that
		// the transfer completed.
		return classify(fmt.Errorf("ftp: transfer of %s did not complete: %w", d.t.redacted(), closeErr))
	}
	if closeErr != nil {
		d.log.DebugContext(ctx, "ftp: closing reply failed after a complete transfer", "error", closeErr)
	}
	if st.size < 0 {
		d.size.Store(pos)
	}
	return nil
}

// finalize flushes the .part file and renames it into place.
func (d *Download) finalize() error {
	written := d.downloaded.Load()
	if size := d.size.Load(); size >= 0 && written != size {
		return fatalf("ftp: downloaded %d bytes, expected %d", written, size)
	}
	if err := d.f.Truncate(written); err != nil {
		return d.fileError("truncate", err)
	}
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
	d.size.Store(written)
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
		return engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("ftp: stat %q: %w", path, err))
	}
}

func (d *Download) fileError(op string, err error) error {
	err = fmt.Errorf("ftp: %s %q: %w", op, d.partPath, err)
	if platform.IsDiskFull(err) {
		return engine.Fatal(domain.ErrorCodeDiskFull, err)
	}
	return engine.Fatal(domain.ErrorCodeFatal, err)
}

func (d *Download) cleanup() {
	if d.f != nil {
		_ = d.f.Close() // error path: the .part file stays for resume
		d.f = nil
	}
	d.conns.Store(0)
}

func (d *Download) source() domain.Source {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.src
}

func (d *Download) updateSource(fn func(*domain.Source)) {
	d.mu.Lock()
	fn(&d.src)
	d.srcDirty = true
	d.mu.Unlock()
}

// setReady allows checkpoints: before the .part state is established a
// failed start must not erase the stored resume state.
func (d *Download) setReady() {
	d.mu.Lock()
	d.ckptReady = true
	d.mu.Unlock()
}

func (d *Download) snapshotCheckpoint() (engine.Checkpoint, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.ckptReady {
		return engine.Checkpoint{}, false
	}
	cp := engine.Checkpoint{Size: d.size.Load(), Downloaded: d.downloaded.Load()}
	if cp.Size > 0 {
		cp.Segments = []domain.Segment{{Start: 0, End: cp.Size, Done: min(cp.Downloaded, cp.Size)}}
	}
	if d.srcDirty {
		s := d.src
		cp.Source = &s
	}
	return cp, true
}

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
		d.log.WarnContext(ctx, "ftp: checkpoint failed", "error", err)
	}
}

func (d *Download) maybeCheckpoint(ctx context.Context) {
	if d.e.env.Clock().Sub(d.lastCkpt) >= d.e.opts.checkpointIntv {
		d.checkpoint(ctx)
	}
}

// finalCheckpoint writes the last checkpoint, with a short detached context
// when ctx is already cancelled.
func (d *Download) finalCheckpoint(ctx context.Context) {
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), finalCheckpointTimeout)
		defer cancel()
	}
	d.checkpoint(ctx)
}
