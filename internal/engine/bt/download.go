package bt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"github.com/anacrolix/torrent/types"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/platform"
	"github.com/sabadia/dlm-server/internal/settings"
)

// stateVersion is the format version of the engine state.
const stateVersion = 1

// state is the engine's resume data.
type state struct {
	V int `json:"v"`
	// Metainfo is the bencoded torrent (a magnet link's metadata is kept
	// here, so a restart does not have to fetch it again).
	Metainfo []byte `json:"metainfo,omitempty"`
	// Complete is set once the selected files were finished.
	Complete bool `json:"complete,omitempty"`
	// Uploaded and SeedMillis are what earlier runs uploaded and how long
	// they seeded; the seeding limits count them.
	Uploaded   int64 `json:"uploaded,omitempty"`
	SeedMillis int64 `json:"seedMillis,omitempty"`
}

func decodeState(b []byte) (state, bool) {
	var st state
	if len(b) == 0 || json.Unmarshal(b, &st) != nil || st.V != stateVersion {
		return state{V: stateVersion}, false
	}
	return st, true
}

// Download is one run of a torrent task; build it with Engine.NewDownload.
type Download struct {
	e    *Engine
	spec engine.Spec
	log  *slog.Logger
	mi   *metainfo.MetaInfo // from the probe, may be nil

	started    atomic.Bool
	downloaded atomic.Int64
	size       atomic.Int64
	conns      atomic.Int32
	writeErr   atomic.Pointer[error]

	fresh    bool      // the metadata was not in the engine state; owned by Run
	lastCkpt time.Time // owned by the goroutine running Run
	mu       sync.Mutex
	st       state
	ready    bool // checkpoints may be written
}

var _ engine.Download = (*Download)(nil)

func newDownload(e *Engine, spec engine.Spec, mi *metainfo.MetaInfo) *Download {
	d := &Download{e: e, spec: spec, mi: mi, st: state{V: stateVersion}, log: e.env.Logger.With("engine", EngineName, "task_id", spec.TaskID)}
	d.size.Store(-1)
	if spec.Resource != nil && spec.Resource.Size > 0 {
		d.size.Store(spec.Resource.SelectedSize())
	}
	return d
}

// Progress implements engine.Download.
func (d *Download) Progress() engine.Progress {
	return engine.Progress{Downloaded: d.downloaded.Load(), Size: d.size.Load(), Connections: int(d.conns.Load())}
}

// Run downloads the selected files and then seeds them as the settings say.
// It returns nil when that is over, ctx.Err() when ctx is cancelled (pause),
// otherwise a classified error. Run may be called only once.
func (d *Download) Run(ctx context.Context) error {
	if !d.started.CompareAndSwap(false, true) {
		return engine.Fatal(domain.ErrorCodeFatal, errors.New("bt: download: Run called more than once"))
	}
	defer d.conns.Store(0)
	err := d.run(ctx)
	d.checkpoint(ctx, true)
	if err != nil && ctx.Err() != nil {
		return ctx.Err() //nolint:wrapcheck // contract: cancellation returns ctx.Err() as is
	}
	return err
}

// seedRules are the limits of the seeding phase.
type seedRules struct {
	keep  bool
	ratio float64
	time  time.Duration
}

func rulesOf(t settings.TorrentSettings) seedRules {
	return seedRules{keep: t.SeedKeep.OrElse(false), ratio: t.SeedRatio.OrElse(0), time: t.SeedTime.OrElse(0).Std()}
}

// seeds reports whether a finished torrent is seeded at all.
func (r seedRules) seeds() bool { return r.keep || r.ratio > 0 || r.time > 0 }

// over reports whether the limits are reached.
func (r seedRules) over(uploaded, size int64, elapsed time.Duration) bool {
	if r.keep {
		return false
	}
	return (r.ratio > 0 && float64(uploaded) >= r.ratio*float64(size)) || (r.time > 0 && elapsed >= r.time)
}

func (d *Download) run(ctx context.Context) error {
	st, restored := decodeState(d.spec.EngineState)
	d.mu.Lock()
	d.st = st
	d.mu.Unlock()

	mi, err := d.metainfo(ctx, st)
	if err != nil {
		return err
	}
	info, err := mi.UnmarshalInfo()
	if err != nil {
		return engine.Fatal(domain.ErrorCodeFatal, domain.InvalidArgument("bt: invalid torrent: %v", err))
	}
	hash := mi.HashInfoBytes()
	res := resourceOf(&info, hash)
	selected, err := selection(d.spec.Resource, &res)
	if err != nil {
		return err
	}
	var size int64
	for i, f := range res.Files {
		if selected[f.Index] {
			size += res.Files[i].Size
		}
	}
	d.size.Store(size)
	if err := d.checkDestination(&info, restored, size); err != nil {
		return err
	}
	if len(st.Metainfo) == 0 {
		d.fresh = true
		var buf bytes.Buffer
		if err := mi.Write(&buf); err != nil {
			return engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("bt: encode the torrent: %w", err))
		}
		d.mu.Lock()
		d.st.Metainfo = buf.Bytes()
		d.mu.Unlock()
	}

	cl, err := d.e.getClient()
	if err != nil {
		return err
	}
	store := fileStorage(d.spec.Path)
	defer func() { _ = store.Close() }() // runs after the torrent is dropped
	var impl storage.ClientImpl = store
	if d.spec.Limiter != nil {
		impl = limitedStorage{ClientImpl: store, ctx: ctx, lim: d.spec.Limiter}
	}
	if err := d.transfer(ctx, cl, mi, &info, hash, selected, size, impl); err != nil {
		return err
	}
	// The storage makes finished files read-only to protect them while they
	// are seeded; the user gets files they can edit.
	for _, p := range finalFiles(d.spec.Path, &info, selected) {
		if err := makeWritable(p); err != nil {
			d.log.WarnContext(ctx, "bt: cannot make a finished file writable", "path", p, "error", err)
		}
	}
	return nil
}

// finalFiles lists the paths of the selected files of the torrent.
func finalFiles(path string, info *metainfo.Info, selected map[int]bool) []string {
	var out []string
	for i, fi := range info.UpvertedFiles() {
		if selected[i] {
			out = append(out, finalPath(path, info, &fi))
		}
	}
	return out
}

// makeWritable gives the owner write permission.
func makeWritable(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err //nolint:wrapcheck // logged with the path by the caller
	}
	if fi.Mode().Perm()&0o200 != 0 {
		return nil
	}
	return os.Chmod(path, fi.Mode().Perm()|0o200) //nolint:wrapcheck // logged with the path by the caller
}

// metainfo returns the torrent: from the engine state, the probe, or the
// source (fetching a magnet link's metadata from the swarm).
func (d *Download) metainfo(ctx context.Context, st state) (*metainfo.MetaInfo, error) {
	if len(st.Metainfo) > 0 {
		if mi, err := metainfo.Load(bytes.NewReader(st.Metainfo)); err == nil {
			return mi, nil
		}
		d.log.WarnContext(ctx, "bt: the stored torrent is unreadable; reading the source again")
	}
	if d.mi != nil {
		return d.mi, nil
	}
	src, err := d.e.load(ctx, d.spec.Source, d.spec.Settings)
	if err != nil {
		return nil, err
	}
	if src.mi != nil {
		return src.mi, nil
	}
	return d.e.fetchMetadata(ctx, src)
}

// selection maps the files of the resource to the torrent's files: every
// file is selected unless the task's resource says otherwise.
func selection(have, res *domain.Resource) (map[int]bool, error) {
	selected := map[int]bool{}
	for _, f := range res.Files {
		selected[f.Index] = true
	}
	if have != nil && len(have.Files) == len(res.Files) {
		for _, f := range have.Files {
			if _, ok := selected[f.Index]; ok {
				selected[f.Index] = f.Selected
			}
		}
	}
	for _, on := range selected {
		if on {
			return selected, nil
		}
	}
	return nil, engine.Fatal(domain.ErrorCodeFatal, domain.InvalidArgument("bt: no file of the torrent is selected"))
}

// checkDestination refuses to start over an existing file or to leave the
// disk without room. A restored download legitimately finds its files.
func (d *Download) checkDestination(info *metainfo.Info, restored bool, need int64) error {
	exists, err := pathExists(d.spec.Path)
	if err != nil {
		return engine.Fatal(domain.ErrorCodeFatal, err)
	}
	if exists && !restored {
		fi, err := os.Stat(d.spec.Path)
		switch {
		case err != nil:
			return engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("bt: stat %q: %w", d.spec.Path, err))
		case !info.IsDir():
			return engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("%w: %q", ErrDestinationExists, d.spec.Path))
		case !fi.IsDir():
			return engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("%w: %q is a file, but the torrent has several files", ErrDestinationExists, d.spec.Path))
		}
	}
	dir := filepath.Dir(d.spec.Path)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // download directories are user-visible, like the files
		return d.fileError("create directory", err)
	}
	if !restored && need > 0 {
		free, err := d.e.opts.freeSpace(dir)
		if err == nil && free < uint64(need) { //nolint:gosec // need is positive here
			return engine.Fatal(domain.ErrorCodeDiskFull, fmt.Errorf("bt: not enough free space in %q: need %d bytes, %d available", dir, need, free))
		}
	}
	return nil
}

func (d *Download) fileError(op string, err error) error {
	err = fmt.Errorf("bt: %s: %w", op, err)
	if platform.IsDiskFull(err) {
		return engine.Fatal(domain.ErrorCodeDiskFull, err)
	}
	return engine.Fatal(domain.ErrorCodeFatal, err)
}

func (d *Download) setReady() {
	d.mu.Lock()
	d.ready = true
	d.mu.Unlock()
}

// transfer adds the torrent to the client, downloads the selected files and
// seeds them. It returns when seeding is over, ctx ends or something fails.
func (d *Download) transfer(ctx context.Context, cl *torrent.Client, mi *metainfo.MetaInfo, info *metainfo.Info, hash metainfo.Hash,
	selected map[int]bool, size int64, store storage.ClientImpl,
) error {
	ent, err := d.e.claim(ctx, hash, func() (*torrent.Torrent, error) {
		spec := torrent.TorrentSpecFromMetaInfo(mi)
		spec.Storage = store
		// Extra trackers go in with the torrent (adding them later races with
		// the library's own announcing); private torrents keep their own.
		if trackers := d.spec.Settings.Torrent.Trackers.OrElse(nil); len(trackers) > 0 && (info.Private == nil || !*info.Private) {
			spec.Trackers = append(spec.Trackers, trackers)
		}
		if m := d.magnet(); m != nil {
			// What a magnet link adds to the torrent file's own data: the
			// trackers, peers and web seeds it names.
			spec.Trackers = append(spec.Trackers, m.Trackers...)
			spec.PeerAddrs = append(spec.PeerAddrs, m.PeerAddrs...)
			spec.Webseeds = append(spec.Webseeds, m.Webseeds...)
			spec.DhtNodes = append(spec.DhtNodes, m.DhtNodes...)
			spec.Sources = append(spec.Sources, m.Sources...)
		}
		t, _, err := cl.AddTorrentSpec(spec)
		if err != nil {
			return nil, engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("bt: add torrent: %w", err))
		}
		return t, nil
	})
	if err != nil {
		return err
	}
	defer d.e.release(hash, ent, true)
	d.setReady() // from here on checkpoints describe a download that started
	t := ent.t
	t.SetOnWriteChunkError(func(err error) {
		d.writeErr.CompareAndSwap(nil, &err)
	})
	select {
	case <-t.GotInfo():
	case <-ctx.Done():
		return ctx.Err() //nolint:wrapcheck // cancellation is reported as is
	}
	files := t.Files()
	var chosen []*torrent.File
	for i, f := range files {
		if selected[i] {
			f.SetPriority(types.PiecePriorityNormal)
			chosen = append(chosen, f)
		} else {
			f.SetPriority(types.PiecePriorityNone)
		}
	}
	// Persist the metadata before the first byte is requested, so a restart
	// of a magnet task does not need the swarm again. (A restored run keeps
	// quiet until it counted what is on disk, or its first checkpoint would
	// report no progress.)
	if d.fresh {
		d.checkpoint(ctx, true)
	}

	finals := finalFiles(d.spec.Path, info, selected)
	var pieces []int
	seen := map[int]bool{}
	for _, f := range chosen {
		for i := range f.PieceIndices() {
			if !seen[i] {
				seen[i] = true
				pieces = append(pieces, i)
			}
		}
	}

	rules := rulesOf(d.spec.Settings.Torrent)
	d.mu.Lock()
	st := d.st
	d.mu.Unlock()
	var (
		seedStart time.Time // zero while downloading
		lastState = d.e.env.Clock()
		tick      = time.NewTicker(d.e.opts.poll)
		// A file counts as finished when every piece it covers is verified:
		// BytesCompleted also counts chunks of unverified pieces.
		completeAt = func() bool {
			for _, i := range pieces {
				if ps := t.PieceState(i); !ps.Ok || !ps.Complete || ps.Marking {
					return false
				}
			}
			// The storage renames a file from .part to its name when its
			// last piece is marked; wait until that has happened.
			for _, p := range finals {
				if _, err := os.Stat(p); err != nil {
					return false
				}
			}
			return true
		}
	)
	defer tick.Stop()
	activity := activityMark{at: d.e.env.Clock()}
	for {
		var got int64
		for _, f := range chosen {
			got += f.BytesCompleted()
		}
		d.downloaded.Store(got)
		stats := t.Stats()
		d.conns.Store(int32(stats.ActivePeers)) //nolint:gosec // a peer count
		if seedStart.IsZero() && activity.stalled(d.e.env.Clock(), d.e.opts.stallReset, got, stats.BytesReadData.Int64(), stats.BytesHashed.Int64()) && stats.ActivePeers > 0 {
			d.log.WarnContext(ctx, "bt: no progress; resetting the peer connections", "peers", stats.ActivePeers, "stalled_for", d.e.opts.stallReset)
			for _, pc := range t.PeerConns() {
				pc.Close() //nolint:gosec,errcheck // dropping the connection is the point; it returns no error
			}
		}
		if errp := d.writeErr.Load(); errp != nil {
			return d.fileError("write", *errp)
		}
		now := d.e.env.Clock()
		if seedStart.IsZero() && completeAt() {
			seedStart = now
			lastState = now
			d.update(func(s *state) { s.Complete = true })
			d.checkpoint(ctx, true)
			if !rules.seeds() {
				return nil
			}
			d.log.InfoContext(ctx, "bt: download finished; seeding", "keep", rules.keep, "ratio", rules.ratio, "time", rules.time)
		}
		if !seedStart.IsZero() {
			uploaded := st.Uploaded + stats.BytesWrittenData.Int64()
			elapsed := time.Duration(st.SeedMillis)*time.Millisecond + now.Sub(seedStart)
			if rules.over(uploaded, size, elapsed) {
				return nil
			}
			if now.Sub(lastState) >= stateInterval {
				lastState = now
				d.recordSeeding(t, st, seedStart)
				d.checkpoint(ctx, true)
			}
		}
		d.maybeCheckpoint(ctx)
		select {
		case <-ctx.Done():
			if !seedStart.IsZero() {
				d.recordSeeding(t, st, seedStart)
			}
			return ctx.Err() //nolint:wrapcheck // cancellation is reported as is
		case <-t.Closed():
			return engine.Transient(domain.ErrorCodeNetwork, errors.New("bt: the torrent was removed from the client"))
		case <-tick.C:
		}
	}
}

// magnet returns the spec of the task's magnet link, or nil when the source
// is a torrent file.
func (d *Download) magnet() *torrent.TorrentSpec {
	src, err := parseMagnet(d.spec.Source.CurrentURL)
	if err != nil {
		return nil
	}
	return src.magnet
}

// activityMark notices a download that stopped moving.
type activityMark struct {
	at                time.Time
	got, read, hashed int64
}

// stalled records the counters and reports whether none changed for d; it
// then starts waiting afresh.
func (m *activityMark) stalled(now time.Time, d time.Duration, got, read, hashed int64) bool {
	if got != m.got || read != m.read || hashed != m.hashed {
		m.at, m.got, m.read, m.hashed = now, got, read, hashed
		return false
	}
	if now.Sub(m.at) < d {
		return false
	}
	m.at = now
	return true
}

// recordSeeding stores the uploaded bytes and the seeding time of this run
// on top of what earlier runs recorded.
func (d *Download) recordSeeding(t *torrent.Torrent, before state, since time.Time) {
	stats := t.Stats()
	up := before.Uploaded + stats.BytesWrittenData.Int64()
	ms := before.SeedMillis + d.e.env.Clock().Sub(since).Milliseconds()
	d.update(func(s *state) { s.Uploaded, s.SeedMillis = up, ms })
}

func (d *Download) update(f func(*state)) {
	d.mu.Lock()
	f(&d.st)
	d.mu.Unlock()
}

// checkpoint writes progress; withState adds the engine state. Failures are
// logged, never fatal.
func (d *Download) checkpoint(ctx context.Context, withState bool) {
	if d.spec.Checkpointer == nil {
		return
	}
	d.mu.Lock()
	ready := d.ready
	var blob []byte
	if withState && len(d.st.Metainfo) > 0 {
		blob, _ = json.Marshal(d.st) //nolint:errchkjson,errcheck // plain data; cannot fail
	}
	d.mu.Unlock()
	if !ready {
		return
	}
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), finalCheckpointTimeout)
		defer cancel()
	}
	d.lastCkpt = d.e.env.Clock()
	cp := engine.Checkpoint{Downloaded: d.downloaded.Load(), Size: d.size.Load(), EngineState: blob}
	if err := d.spec.Checkpointer.Checkpoint(ctx, cp); err != nil {
		d.log.WarnContext(ctx, "bt: checkpoint failed", "error", err)
	}
}

func (d *Download) maybeCheckpoint(ctx context.Context) {
	if d.e.env.Clock().Sub(d.lastCkpt) >= d.e.opts.checkpointIntv {
		d.checkpoint(ctx, false)
	}
}
