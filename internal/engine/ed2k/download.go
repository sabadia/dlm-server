package ed2k

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/monkeyWie/goed2k"
	"github.com/monkeyWie/goed2k/protocol"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/platform"
)

// stateVersion is the format version of the engine state.
const stateVersion = 1

// state is the engine's resume data.
type state struct {
	V int `json:"v"`
	// Resume is the client's resume data: verified pieces, downloaded blocks
	// and known peers.
	Resume *protocol.TransferResumeData `json:"resume,omitempty"`
}

func decodeState(b []byte) (state, bool) {
	var st state
	if len(b) == 0 || json.Unmarshal(b, &st) != nil || st.V != stateVersion {
		return state{V: stateVersion}, false
	}
	return st, true
}

// Download is one run of an ed2k task; build it with Engine.NewDownload.
type Download struct {
	e        *Engine
	spec     engine.Spec
	l        *link
	log      *slog.Logger
	partPath string

	started    atomic.Bool
	downloaded atomic.Int64
	conns      atomic.Int32
	lastCkpt   time.Time // owned by the goroutine running Run
}

var _ engine.Download = (*Download)(nil)

func newDownload(e *Engine, spec engine.Spec, l *link) *Download {
	return &Download{
		e: e, spec: spec, l: l, partPath: spec.Path + PartSuffix,
		log: e.env.Logger.With("engine", EngineName, "task_id", spec.TaskID),
	}
}

// Progress implements engine.Download.
func (d *Download) Progress() engine.Progress {
	return engine.Progress{Downloaded: d.downloaded.Load(), Size: d.l.size, Connections: int(d.conns.Load())}
}

// Run downloads the file. It returns nil when the file is complete and in
// place, ctx.Err() when ctx is cancelled (pause), otherwise a classified
// error. Run may be called only once.
func (d *Download) Run(ctx context.Context) error {
	if !d.started.CompareAndSwap(false, true) {
		return engine.Fatal(domain.ErrorCodeFatal, errors.New("ed2k: download: Run called more than once"))
	}
	defer d.conns.Store(0)
	err := d.run(ctx)
	if err != nil && ctx.Err() != nil {
		return ctx.Err() //nolint:wrapcheck // contract: cancellation returns ctx.Err() as is
	}
	return err
}

func (d *Download) run(ctx context.Context) error {
	if _, err := os.Lstat(d.spec.Path); err == nil {
		return engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("%w: %q", ErrDestinationExists, d.spec.Path))
	} else if !errors.Is(err, fs.ErrNotExist) {
		return engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("ed2k: stat %q: %w", d.spec.Path, err))
	}
	dir := filepath.Dir(d.spec.Path)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // download directories are user-visible, like the files
		return d.fileError("create directory", err)
	}
	st, restored := decodeState(d.spec.EngineState)
	if _, err := os.Stat(d.partPath); err != nil {
		restored = false // nothing to continue
		st.Resume = nil
	}
	if !restored {
		st.Resume = nil
		if free, err := d.e.opts.freeSpace(dir); err == nil && free < uint64(d.l.size) { //nolint:gosec // size is positive
			return engine.Fatal(domain.ErrorCodeDiskFull, fmt.Errorf("ed2k: not enough free space in %q: need %d bytes, %d available", dir, d.l.size, free))
		}
	}
	flags := os.O_RDWR | os.O_CREATE
	if !restored {
		flags |= os.O_TRUNC // stale bytes without resume data cannot be trusted
	}
	f, err := os.OpenFile(d.partPath, flags, 0o644) //nolint:gosec // path chosen by the user; downloads are user-visible files
	if err != nil {
		return d.fileError("create", err)
	}
	// The client opens the file itself, by name.
	if err := f.Close(); err != nil {
		return d.fileError("close", err)
	}
	cl, err := d.e.getClient()
	if err != nil {
		return err
	}
	key := hashKey(d.l.hash)
	if !d.e.claim(key) {
		return engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("ed2k: the file %s is already being downloaded by another task", key))
	}
	defer d.e.unclaim(key)

	atp := goed2k.AddTransferParams{
		Hash: d.l.hash, CreateTime: d.e.env.Clock().UnixMilli(), Size: d.l.size, FilePath: d.partPath,
		ResumeData: d.resumeData(st.Resume),
	}
	h, err := cl.AddTransfer(atp)
	if err != nil {
		return engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("ed2k: add transfer: %w", err))
	}
	release := func() {
		if err := cl.RemoveTransfer(d.l.hash, false); err != nil {
			d.log.Warn("ed2k: cannot remove the transfer", "error", err)
		}
	}
	finished := false
	defer func() {
		if !finished {
			d.checkpoint(ctx, h) // the final checkpoint: resume data for the next run
		}
		release()
	}()

	tick := time.NewTicker(d.e.opts.poll)
	defer tick.Stop()
	for {
		s := statusOf(h)
		d.downloaded.Store(min(s.TotalDone, d.l.size))
		d.conns.Store(int32(s.NumPeers)) //nolint:gosec // a peer count
		if h.IsFinished() {
			finished = true
			break
		}
		if d.e.env.Clock().Sub(d.lastCkpt) >= d.e.opts.checkpointIntv {
			d.checkpoint(ctx, h)
		}
		select {
		case <-ctx.Done():
			return ctx.Err() //nolint:wrapcheck // cancellation is reported as is
		case <-tick.C:
		}
	}
	release()
	d.downloaded.Store(d.l.size)
	if err := d.finish(); err != nil {
		return err
	}
	if d.spec.Checkpointer != nil {
		cp := engine.Checkpoint{Downloaded: d.l.size, Size: d.l.size}
		if err := d.spec.Checkpointer.Checkpoint(ctx, cp); err != nil {
			d.log.WarnContext(ctx, "ed2k: final checkpoint failed", "error", err)
		}
	}
	return nil
}

// resumeData merges the saved resume data with the peers the link names.
func (d *Download) resumeData(saved *protocol.TransferResumeData) *protocol.TransferResumeData {
	if saved == nil && len(d.l.sources) == 0 {
		return nil
	}
	rd := &protocol.TransferResumeData{}
	if saved != nil {
		*rd = *saved
	}
	for _, ep := range d.l.sources {
		known := false
		for _, have := range rd.Peers {
			known = known || have.Equal(ep)
		}
		if !known {
			rd.Peers = append(rd.Peers, ep)
		}
	}
	return rd
}

// checkpoint writes progress and the resume data; failures are logged.
func (d *Download) checkpoint(ctx context.Context, h goed2k.TransferHandle) {
	if d.spec.Checkpointer == nil {
		return
	}
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), finalCheckpointTimeout)
		defer cancel()
	}
	d.lastCkpt = d.e.env.Clock()
	cp := engine.Checkpoint{Downloaded: d.downloaded.Load(), Size: d.l.size}
	if rd := resumeOf(h); rd != nil {
		blob, err := json.Marshal(state{V: stateVersion, Resume: rd})
		if err == nil {
			cp.EngineState = blob
		}
	}
	if err := d.spec.Checkpointer.Checkpoint(ctx, cp); err != nil {
		d.log.WarnContext(ctx, "ed2k: checkpoint failed", "error", err)
	}
}

// finish checks the size of the completed file and renames it into place. The
// client releases the file asynchronously, so the rename is retried for a
// moment.
func (d *Download) finish() error {
	var err error
	for range 50 {
		if err = d.rename(); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return d.fileError("finish", err)
}

func (d *Download) rename() error {
	fi, err := os.Stat(d.partPath)
	if err != nil {
		return err //nolint:wrapcheck // wrapped by finish
	}
	if fi.Size() != d.l.size {
		return fmt.Errorf("the file has %d bytes, the link says %d", fi.Size(), d.l.size)
	}
	if _, err := os.Lstat(d.spec.Path); err == nil {
		return fmt.Errorf("%w: %q", ErrDestinationExists, d.spec.Path)
	}
	return os.Rename(d.partPath, d.spec.Path) //nolint:wrapcheck // wrapped by finish
}

func (d *Download) fileError(op string, err error) error {
	err = fmt.Errorf("ed2k: %s %q: %w", op, d.partPath, err)
	if platform.IsDiskFull(err) {
		return engine.Fatal(domain.ErrorCodeDiskFull, err)
	}
	return engine.Fatal(domain.ErrorCodeFatal, err)
}

// The client's tick goroutine changes the transfer without locking against
// the handle's readers, so a read can see a torn value; a panic from that is
// turned into "no data this time" instead of taking the daemon down.

func statusOf(h goed2k.TransferHandle) (s goed2k.TransferStatus) {
	defer func() { _ = recover() }() //nolint:errcheck // see above
	return h.GetStatus()
}

func resumeOf(h goed2k.TransferHandle) (rd *protocol.TransferResumeData) {
	defer func() {
		if recover() != nil {
			rd = nil
		}
	}()
	return h.GetResumeData()
}
