package segmented

import (
	"context"
	"crypto/sha1" //nolint:gosec // names files; not a security use
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/engine/retry"
	"github.com/sabadia/dlm-server/internal/platform"
	"github.com/sabadia/dlm-server/internal/ratelimit"
	"github.com/sabadia/dlm-server/internal/settings"
)

var (
	// ErrDestinationExists is returned when the final file already exists.
	ErrDestinationExists = errors.New("hls: destination file already exists")
	// ErrContentChanged is returned when the stream behind a refreshed link is
	// not the one that was partly downloaded (other number or length of
	// segments).
	ErrContentChanged = errors.New("hls: the stream changed since the download started")
	// ErrFFmpeg is returned when ffmpeg is needed but not available.
	ErrFFmpeg = errors.New("hls: ffmpeg is required to save this stream")
)

// journalVersion is the version of the resume state.
const journalVersion = 1

// journal is the resume state stored as the engine state: which segments of
// each track are on disk (with their size), and what the stream looked like
// when they were fetched.
type journal struct {
	V      int            `json:"v"`
	Tracks []trackJournal `json:"tracks"`
}

type trackJournal struct {
	Done     map[uint64]int64 `json:"done"`
	Count    int              `json:"count,omitempty"`    // segments of a finished (VOD) playlist
	Duration float64          `json:"duration,omitempty"` // their total duration
}

// Download is one run of an HLS task. See Run.
type Download struct {
	r    *Runner
	fe   Frontend
	spec engine.Spec

	started atomic.Bool
	work    string // directory of the Segment files: <path>.hls
	part    string // <path>.part: the merged file before it is renamed

	client  Client
	policy  retry.Policy
	plan    *Plan
	log     *slog.Logger
	workers int

	downloaded atomic.Int64
	doneDurMs  atomic.Int64 // duration of the finished segments, in ms
	totalDurMs atomic.Int64 // duration of a finished playlist, 0 for a live one
	finished   atomic.Bool

	keyMu sync.Mutex
	keys  map[string][]byte

	initMu sync.Mutex
	inits  map[string]string // init section key -> file

	jmu sync.Mutex
	j   journal
}

func newDownload(r *Runner, spec engine.Spec, fe Frontend) *Download {
	log := r.env.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	d := &Download{
		r: r, fe: fe, spec: spec, log: log, keys: map[string][]byte{}, inits: map[string]string{},
		work: spec.Path + ".hls", part: spec.Path + ".part", policy: retry.FromSettings(spec.Settings.Retry),
	}
	d.workers = min(max(spec.Settings.MaxConnections.OrElse(defaultWorkers), 1), maxWorkers)
	return d
}

// Progress implements engine.Download. The size is an estimate while the
// stream is downloading (the Segment sizes are only known once fetched) and
// unknown (-1) for a live stream.
func (d *Download) Progress() engine.Progress {
	got := d.downloaded.Load()
	size := int64(-1)
	switch {
	case d.finished.Load():
		size = got
	case d.totalDurMs.Load() > 0:
		if done := d.doneDurMs.Load(); done > 0 && got > 0 {
			size = int64(float64(got) * float64(d.totalDurMs.Load()) / float64(done))
		} else if d.plan != nil {
			size = estimateSize(d.plan.Bandwidth, float64(d.totalDurMs.Load())/1000)
		}
	}
	return engine.Progress{Downloaded: got, Size: size, Connections: d.workers}
}

// Run downloads the stream: every Segment of the chosen variant (and audio
// rendition) is fetched, decrypted and kept in <path>.hls; when all are there
// ffmpeg merges them into <path>.part, which is renamed to <path>. A live
// stream is recorded until the playlist ends or stops growing. The playlist
// is read again on every run, so a resumed task works with refreshed links; a
// Segment whose link expired is retried with the URL of a reloaded playlist
// first. A final checkpoint is written before Run returns.
func (d *Download) Run(ctx context.Context) error {
	if !d.started.CompareAndSwap(false, true) {
		return engine.Fatal(domain.ErrorCodeFatal, errors.New("hls: Run called more than once"))
	}
	if _, err := os.Lstat(d.spec.Path); err == nil {
		return engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("%w: %q", ErrDestinationExists, d.spec.Path))
	} else if !errors.Is(err, fs.ErrNotExist) {
		return d.fileError("stat", d.spec.Path, err)
	}
	client, err := d.r.Client(d.spec.Source, d.spec.Settings)
	if err != nil {
		return err
	}
	d.client = client
	p, err := d.fe.Build(ctx, client)
	if err != nil {
		return err
	}
	d.plan = p
	if err := os.MkdirAll(filepath.Dir(d.spec.Path), 0o755); err != nil { //nolint:gosec // download directories are user-visible
		return d.fileError("create directory", filepath.Dir(d.spec.Path), err)
	}
	if err := d.restore(p); err != nil {
		return err
	}
	if err := os.MkdirAll(d.work, 0o755); err != nil { //nolint:gosec // download directories are user-visible
		return d.fileError("create directory", d.work, err)
	}
	var ff string
	if d.needsFFmpeg(p) {
		if ff, err = d.locateFFmpeg(ctx); err != nil {
			return err
		}
	}
	d.verifyDone(p)

	ckptCtx, stopCkpt := context.WithCancel(context.WithoutCancel(ctx))
	var ckptDone sync.WaitGroup
	ckptDone.Add(1)
	go func() { defer ckptDone.Done(); d.checkpointLoop(ckptCtx) }()
	err = d.downloadAll(ctx, p)
	stopCkpt()
	ckptDone.Wait()
	d.checkpoint(ctx) // the final state, also after a pause
	if err != nil {
		return err
	}
	if err := d.merge(ctx, p, ff); err != nil {
		return err
	}
	_ = os.RemoveAll(d.work) // best effort: the merged file is in place
	if info, err := os.Stat(d.spec.Path); err == nil {
		d.downloaded.Store(info.Size()) // the size of the result, not of the pieces it was made from
	}
	d.finished.Store(true)
	d.checkpoint(ctx)
	return nil
}

// ---- journal ----

// restore loads the resume state and compares it with the stream. A finished
// playlist that has other segments than when the download started is a
// different stream: refresh.onMismatch decides between starting over and
// failing with a link-expired error (so the refresh layer can look for the
// right link).
func (d *Download) restore(p *Plan) error {
	d.j = journal{V: journalVersion, Tracks: make([]trackJournal, len(p.Tracks))}
	for i := range d.j.Tracks {
		d.j.Tracks[i].Done = map[uint64]int64{}
	}
	if len(d.spec.EngineState) > 0 {
		var old journal
		if err := json.Unmarshal(d.spec.EngineState, &old); err == nil && old.V == journalVersion && len(old.Tracks) == len(p.Tracks) {
			d.j = old
			for i := range d.j.Tracks {
				if d.j.Tracks[i].Done == nil {
					d.j.Tracks[i].Done = map[uint64]int64{}
				}
			}
		}
	}
	for i, t := range p.Tracks {
		tj := &d.j.Tracks[i]
		if tj.Count > 0 && t.isClosed() {
			count, dur := len(t.snapshot()), t.totalDuration()
			if count != tj.Count || math.Abs(dur-tj.Duration) > 1 {
				if d.spec.Settings.Refresh.OnMismatch.OrElse(settings.MismatchFail) != settings.MismatchRestart {
					return engine.LinkExpired(fmt.Errorf("%w: %d segments of %.0fs, was %d of %.0fs", ErrContentChanged, count, dur, tj.Count, tj.Duration))
				}
				d.log.Info("hls: the stream changed; starting over", "task_id", d.spec.TaskID)
				_ = os.RemoveAll(d.work) // best effort: stale pieces of another stream
				d.j = journal{V: journalVersion, Tracks: make([]trackJournal, len(p.Tracks))}
				for k := range d.j.Tracks {
					d.j.Tracks[k].Done = map[uint64]int64{}
				}
				break
			}
		}
	}
	for i, t := range p.Tracks {
		if t.isClosed() {
			d.j.Tracks[i].Count, d.j.Tracks[i].Duration = len(t.snapshot()), t.totalDuration()
		}
	}
	return nil
}

// verifyDone drops journal entries whose file is missing or has another size,
// and counts what is on disk.
func (d *Download) verifyDone(p *Plan) {
	d.jmu.Lock()
	defer d.jmu.Unlock()
	var bytes, durMs int64
	for i, t := range p.Tracks {
		done := d.j.Tracks[i].Done
		for _, seq := range sortedSeqs(done) {
			info, err := os.Stat(d.segPath(t.Index, seq))
			s, known := t.get(seq)
			if err != nil || info.Size() != done[seq] || !known {
				delete(done, seq)
				continue
			}
			bytes += done[seq]
			durMs += int64(s.Duration * 1000)
		}
	}
	d.downloaded.Store(bytes)
	d.doneDurMs.Store(durMs)
	var total float64
	live := false
	for _, t := range p.Tracks {
		if !t.isClosed() {
			live = true
		}
	}
	if !live && len(p.Tracks) > 0 {
		total = p.Tracks[0].totalDuration()
		d.totalDurMs.Store(int64(total * 1000))
	}
}

func (d *Download) marshalJournal() []byte {
	d.jmu.Lock()
	defer d.jmu.Unlock()
	data, err := json.Marshal(d.j)
	if err != nil {
		return nil
	}
	return data
}

func (d *Download) isDone(idx int, seq uint64) bool {
	d.jmu.Lock()
	defer d.jmu.Unlock()
	_, ok := d.j.Tracks[idx].Done[seq]
	return ok
}

func (d *Download) markDone(idx int, seq uint64, size int64) {
	d.jmu.Lock()
	d.j.Tracks[idx].Done[seq] = size
	d.jmu.Unlock()
}

// ---- checkpoints ----

func (d *Download) checkpointLoop(ctx context.Context) {
	t := time.NewTicker(d.r.opts.ckptIntv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.checkpoint(ctx)
		}
	}
}

// checkpoint writes the current state; failures are logged, never fatal.
func (d *Download) checkpoint(ctx context.Context) {
	if d.spec.Checkpointer == nil {
		return
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	prog := d.Progress()
	err := d.spec.Checkpointer.Checkpoint(cctx, engine.Checkpoint{Downloaded: prog.Downloaded, Size: prog.Size, EngineState: d.marshalJournal()})
	if err != nil {
		d.log.Warn("hls: checkpoint failed", "task_id", d.spec.TaskID, "error", err)
	}
}

// ---- downloading ----

type job struct {
	t   *Track
	Seq uint64
}

// downloadAll fetches every Segment of the plan; for a live stream it keeps
// following the playlist until it ends or stalls.
func (d *Download) downloadAll(ctx context.Context, p *Plan) error {
	lastNew := d.r.env.Clock()
	failures := 0
	for {
		if err := d.runJobs(ctx, d.pending(p)); err != nil {
			return err
		}
		if !p.live() {
			return nil
		}
		// A live stream: wait a little, then see what the playlist added.
		wait := d.liveWait(p)
		if err := d.r.opts.sleep(ctx, wait); err != nil {
			return err //nolint:wrapcheck // cancellation is reported as is
		}
		added := 0
		for _, t := range p.Tracks {
			if t.isClosed() {
				continue
			}
			ok, n, err := d.reload(ctx, t, true)
			if err != nil || !ok {
				if ctx.Err() != nil {
					return ctx.Err() //nolint:wrapcheck // cancellation is reported as is
				}
				if failures++; failures >= 3 {
					return err
				}
				continue
			}
			failures = 0
			added += n
		}
		now := d.r.env.Clock()
		if added > 0 {
			lastNew = now
		} else if stall := d.stallLimit(p); now.Sub(lastNew) > stall && p.live() {
			d.log.Info("hls: the live stream stopped growing; finishing", "task_id", d.spec.TaskID)
			return nil
		}
	}
}

func (d *Download) liveWait(p *Plan) time.Duration {
	var target time.Duration
	for _, t := range p.Tracks {
		if td := t.targetDuration(); td > target {
			target = td
		}
	}
	if f := d.r.opts.liveWait; f != nil {
		return f(target)
	}
	return max(target/2, minLiveReload)
}

// stallLimit is how long a live stream may add nothing before it counts as
// over: three target durations, at least 30 seconds.
func (d *Download) stallLimit(p *Plan) time.Duration {
	var target time.Duration
	for _, t := range p.Tracks {
		target = max(target, t.targetDuration())
	}
	if f := d.r.opts.liveWait; f != nil {
		return 3 * f(target)
	}
	return max(3*target, 30*time.Second)
}

func (d *Download) pending(p *Plan) []job {
	var jobs []job
	for _, t := range p.Tracks {
		for _, s := range t.snapshot() {
			if !d.isDone(t.Index, s.Seq) {
				jobs = append(jobs, job{t: t, Seq: s.Seq})
			}
		}
	}
	return jobs
}

func (d *Download) runJobs(ctx context.Context, jobs []job) error {
	if len(jobs) == 0 {
		return nil
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(d.workers)
	for _, j := range jobs {
		if gctx.Err() != nil {
			break
		}
		g.Go(func() error { return d.doSegment(gctx, j) })
	}
	err := g.Wait()
	if ctx.Err() != nil {
		return ctx.Err() //nolint:wrapcheck // cancellation is reported as is
	}
	return err //nolint:wrapcheck // the Segment errors are classified and returned as they are
}

func (d *Download) doSegment(ctx context.Context, j job) error {
	seg, ok := j.t.get(j.Seq)
	if !ok {
		return nil
	}
	size, err := d.fetchSegment(ctx, j.t, seg)
	if engine.Classify(err) == engine.KindLinkExpired {
		// The URL (or key) of the Segment no longer works: the playlist may
		// hand out a fresh one.
		if ok, _, rerr := d.reload(ctx, j.t, false); ok && rerr == nil {
			if fresh, found := j.t.get(j.Seq); found {
				size, err = d.fetchSegment(ctx, j.t, fresh)
			}
		}
	}
	if err != nil {
		return err
	}
	d.markDone(j.t.Index, j.Seq, size)
	d.downloaded.Add(size)
	d.doneDurMs.Add(int64(seg.Duration * 1000))
	return nil
}

// fetchSegment gets one Segment (retrying transient failures), decrypts it
// and stores it. It returns the stored size.
func (d *Download) fetchSegment(ctx context.Context, t *Track, seg Segment) (int64, error) {
	if seg.Init != nil {
		if err := d.ensureInit(ctx, *seg.Init); err != nil {
			return 0, err
		}
	}
	var size int64
	err := retry.Do(ctx, d.policy, func(ctx context.Context, _ int) error {
		data, _, err := fetch(ctx, d.client, seg.Ref.URL, seg.Ref.Range, maxSegmentBytes,
			func(r io.Reader) io.Reader { return ratelimit.Reader(ctx, r, d.spec.Limiter) })
		if err != nil {
			return err
		}
		if seg.Key != nil {
			key, err := d.key(ctx, seg.Key)
			if err != nil {
				return err
			}
			if data, err = decryptAES128(data, key, ivFor(seg.Key, seg.Seq)); err != nil {
				return engine.Fatal(domain.ErrorCodeFatal, err)
			}
		}
		if err := d.writeFile(d.segPath(t.Index, seg.Seq), data); err != nil {
			return err
		}
		size = int64(len(data))
		return nil
	}, retry.WithSleeper(d.r.opts.sleep), retry.OnRetry(func(attempt int, delay time.Duration, err error) {
		d.log.Warn("hls: retrying a segment", "task_id", d.spec.TaskID, "seq", seg.Seq, "attempt", attempt, "delay", delay, "error", err)
	}))
	return size, err
}

// key returns the decryption key at k.URL, fetching it once.
func (d *Download) key(ctx context.Context, k *Key) ([]byte, error) {
	d.keyMu.Lock()
	defer d.keyMu.Unlock()
	if key, ok := d.keys[k.URL]; ok {
		return key, nil
	}
	var key []byte
	err := retry.Do(ctx, d.policy, func(ctx context.Context, _ int) error {
		data, _, err := fetch(ctx, d.client, k.URL, nil, maxKeyBytes, nil)
		key = data
		return err
	}, retry.WithSleeper(d.r.opts.sleep))
	if err != nil {
		return nil, err
	}
	if len(key) != 16 {
		return nil, engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("hls: the AES-128 key at %s is %d bytes, want 16", Redact(k.URL), len(key)))
	}
	d.keys[k.URL] = key
	return key, nil
}

// ensureInit fetches an fMP4 initialisation section once and returns its file.
func (d *Download) ensureInit(ctx context.Context, ref FileRef) error {
	id := initID(ref)
	d.initMu.Lock()
	defer d.initMu.Unlock()
	if _, ok := d.inits[id]; ok {
		return nil
	}
	path := filepath.Join(d.work, "init-"+id+".bin")
	if info, err := os.Stat(path); err == nil && info.Size() > 0 {
		d.inits[id] = path
		return nil
	}
	var data []byte
	err := retry.Do(ctx, d.policy, func(ctx context.Context, _ int) error {
		var err error
		data, _, err = fetch(ctx, d.client, ref.URL, ref.Range, maxSegmentBytes, nil)
		return err
	}, retry.WithSleeper(d.r.opts.sleep))
	if err != nil {
		return err
	}
	if err := d.writeFile(path, data); err != nil {
		return err
	}
	d.inits[id] = path
	return nil
}

func initID(ref FileRef) string {
	key := ref.URL
	if ref.Range != nil {
		key += "#" + strconv.FormatInt(ref.Range.Offset, 10) + "+" + strconv.FormatInt(ref.Range.Length, 10)
	}
	sum := sha1.Sum([]byte(key)) //nolint:gosec // a file name, not security
	return hex.EncodeToString(sum[:6])
}

func (d *Download) segPath(trackIdx int, seq uint64) string {
	return filepath.Join(d.work, fmt.Sprintf("t%d-%d.seg", trackIdx, seq))
}

// writeFile stores data at path through a temporary file, so a half-written
// Segment is never mistaken for a finished one.
func (d *Download) writeFile(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil { //nolint:gosec // user-visible download pieces
		return d.fileError("write", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return d.fileError("rename", path, err)
	}
	return nil
}

func (d *Download) fileError(op, path string, err error) error {
	err = fmt.Errorf("hls: %s %q: %w", op, path, err)
	if platform.IsDiskFull(err) {
		return engine.Fatal(domain.ErrorCodeDiskFull, err)
	}
	return engine.Fatal(domain.ErrorCodeFatal, err)
}

// estimateSize guesses the output size from the bandwidth and the duration;
// it is only used to show progress.
func estimateSize(bandwidth int64, seconds float64) int64 {
	if bandwidth <= 0 || seconds <= 0 || math.IsInf(seconds, 0) {
		return -1
	}
	return int64(float64(bandwidth) / 8 * seconds)
}
