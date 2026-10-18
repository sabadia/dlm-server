package ytdlp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/platform"
)

// WorkSuffix is appended to the destination path for the directory yt-dlp
// downloads into.
const WorkSuffix = ".part"

const (
	progressPrefix = "DLM|"
	filePrefix     = "DLMFILE|"
	// progressTemplate prints one line per update: status, downloaded bytes,
	// total bytes, estimated total bytes, file name (last: it may hold "|").
	progressTemplate = "download:" + progressPrefix + "%(progress.status)s|%(progress.downloaded_bytes)s|%(progress.total_bytes)s|%(progress.total_bytes_estimate)s|%(progress.filename)s"
	outputName       = "dl"
	maxLineBytes     = 1 << 20
)

// Download is one run of a yt-dlp task; build it with Engine.NewDownload.
type Download struct {
	e       *Engine
	spec    engine.Spec
	log     *slog.Logger
	workDir string

	started    atomic.Bool
	downloaded atomic.Int64
	size       atomic.Int64
	conns      atomic.Int32

	lastCkpt time.Time // owned by the goroutine running Run
	mu       sync.Mutex
	streams  map[string]*stream // by file name: video and audio come separately
}

type stream struct{ done, total int64 }

var _ engine.Download = (*Download)(nil)

func newDownload(e *Engine, spec engine.Spec) *Download {
	d := &Download{
		e: e, spec: spec, workDir: spec.Path + WorkSuffix, streams: map[string]*stream{},
		log: e.env.Logger.With("engine", EngineName, "task_id", spec.TaskID),
	}
	d.size.Store(-1)
	if spec.Resource != nil && spec.Resource.Size > 0 {
		d.size.Store(spec.Resource.Size)
	}
	return d
}

// Progress implements engine.Download.
func (d *Download) Progress() engine.Progress {
	return engine.Progress{Downloaded: d.downloaded.Load(), Size: d.size.Load(), Connections: int(d.conns.Load())}
}

// Run downloads the media. It returns nil when the file is in place,
// ctx.Err() when ctx is cancelled (pause), otherwise a classified error. Run
// may be called only once.
func (d *Download) Run(ctx context.Context) error {
	if !d.started.CompareAndSwap(false, true) {
		return engine.Fatal(domain.ErrorCodeFatal, errors.New("ytdlp: download: Run called more than once"))
	}
	defer d.conns.Store(0)
	err := d.run(ctx)
	d.checkpoint(ctx)
	if err != nil && ctx.Err() != nil {
		return ctx.Err() //nolint:wrapcheck // contract: cancellation returns ctx.Err() as is
	}
	return err
}

func (d *Download) run(ctx context.Context) error {
	if _, err := os.Lstat(d.spec.Path); err == nil {
		return engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("%w: %q", ErrDestinationExists, d.spec.Path))
	} else if !errors.Is(err, fs.ErrNotExist) {
		return engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("ytdlp: stat %q: %w", d.spec.Path, err))
	}
	dir := filepath.Dir(d.spec.Path)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // download directories are user-visible, like the files
		return d.fileError("create directory", err)
	}
	if need := d.size.Load(); need > 0 {
		if free, err := d.e.opts.freeSpace(dir); err == nil && free < uint64(need) { //nolint:gosec // need is positive here
			return engine.Fatal(domain.ErrorCodeDiskFull, fmt.Errorf("ytdlp: not enough free space in %q: need about %d bytes, %d available", dir, need, free))
		}
	}
	bin, err := d.e.binary(ctx)
	if err != nil {
		return err
	}
	cs, err := d.e.prepare(ctx, d.spec.Source, d.spec.Settings)
	if err != nil {
		return err
	}
	defer cs.cleanup()
	if err := os.MkdirAll(d.workDir, 0o755); err != nil { //nolint:gosec // beside the final file
		return d.fileError("create work directory", err)
	}
	args := cs.args()
	if d.e.ff != nil {
		if fb, err := d.e.ff.Locate(ctx); err == nil {
			args = append(args, "--ffmpeg-location="+fb.Path)
		} else if ctx.Err() == nil {
			d.log.WarnContext(ctx, "ytdlp: no ffmpeg; merging video and audio may fail", "error", err)
		}
	}
	args = append(args,
		"--no-playlist", "--newline", "--quiet", "--progress", "--continue", "--no-simulate",
		"--progress-template="+progressTemplate,
		"--print=after_move:"+filePrefix+"%(filepath)s",
		"--output="+filepath.Join(d.workDir, outputName+".%(ext)s"),
		"--", strings.TrimSpace(d.spec.Source.CurrentURL),
	)
	file, err := d.runProcess(ctx, bin.Path, args)
	if err != nil {
		return err
	}
	return d.finish(file)
}

// runProcess runs yt-dlp and returns the path of the file it reports.
func (d *Download) runProcess(ctx context.Context, bin string, args []string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // the user's or our own yt-dlp
	// An interrupt lets yt-dlp keep its partial files; after the grace period
	// it is killed.
	cmd.Cancel = func() error { return platform.InterruptProcess(cmd.Process) }
	cmd.WaitDelay = interruptGrace
	var stderr bytes.Buffer
	cmd.Stderr = &limitWriter{w: &stderr, n: 1 << 20}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return "", engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("ytdlp: %w", err))
	}
	if err := cmd.Start(); err != nil {
		return "", engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("ytdlp: start %s: %w", bin, err))
	}
	d.conns.Store(1)
	var file string
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 64<<10), maxLineBytes)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, progressPrefix):
			d.progressLine(strings.TrimPrefix(line, progressPrefix))
			d.maybeCheckpoint(ctx)
		case strings.HasPrefix(line, filePrefix):
			file = strings.TrimPrefix(line, filePrefix)
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		_, _ = io.Copy(io.Discard, out) // let yt-dlp finish writing before it is waited for
	}
	waitErr := cmd.Wait()
	d.conns.Store(0)
	switch {
	case ctx.Err() != nil:
		return "", ctx.Err() //nolint:wrapcheck // cancellation is reported as is
	case waitErr != nil:
		return "", classifyFailure(stderr.String(), waitErr)
	case file == "":
		return "", engine.Fatal(domain.ErrorCodeFatal, errNoOutput)
	}
	return file, nil
}

// progressLine reads "status|downloaded|total|estimate|filename".
func (d *Download) progressLine(rest string) {
	f := strings.SplitN(rest, "|", 5)
	if len(f) != 5 {
		return
	}
	done := parseCount(f[1])
	total := parseCount(f[2])
	if total == 0 {
		total = parseCount(f[3])
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.streams[f[4]]
	if s == nil {
		s = &stream{}
		d.streams[f[4]] = s
	}
	if f[0] == "finished" && total == 0 {
		total = done
	}
	s.done, s.total = done, max(total, done)
	var sumDone, sumTotal int64
	for _, st := range d.streams {
		sumDone += st.done
		sumTotal += st.total
	}
	if sumTotal > d.size.Load() {
		d.size.Store(sumTotal)
	}
	if size := d.size.Load(); size > 0 {
		sumDone = min(sumDone, size)
	}
	d.downloaded.Store(sumDone)
}

// parseCount reads a byte count that yt-dlp prints as an integer, a float or
// "NA"; unknown is 0.
func parseCount(s string) int64 {
	s = strings.TrimSpace(s)
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
		return n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && f > 0 {
		return int64(f)
	}
	return 0
}

// finish moves the file yt-dlp made to the task's path and removes the work
// directory.
func (d *Download) finish(file string) error {
	file = filepath.Clean(file)
	if rel, err := filepath.Rel(d.workDir, file); err != nil || strings.HasPrefix(rel, "..") {
		return engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("ytdlp: yt-dlp wrote %q outside of its work directory", file))
	}
	fi, err := os.Stat(file)
	if err != nil {
		return d.fileError("find the finished file", err)
	}
	if _, err := os.Lstat(d.spec.Path); err == nil {
		return engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("%w: %q", ErrDestinationExists, d.spec.Path))
	}
	if err := os.Rename(file, d.spec.Path); err != nil {
		return d.fileError("move the finished file", err)
	}
	if err := os.RemoveAll(d.workDir); err != nil {
		d.log.Warn("ytdlp: cannot remove the work directory", "path", d.workDir, "error", err)
	}
	d.size.Store(fi.Size())
	d.downloaded.Store(fi.Size())
	return nil
}

func (d *Download) fileError(op string, err error) error {
	err = fmt.Errorf("ytdlp: %s: %w", op, err)
	if platform.IsDiskFull(err) {
		return engine.Fatal(domain.ErrorCodeDiskFull, err)
	}
	return engine.Fatal(domain.ErrorCodeFatal, err)
}

// checkpoint writes progress; failures are logged, never fatal.
func (d *Download) checkpoint(ctx context.Context) {
	if d.spec.Checkpointer == nil {
		return
	}
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), finalCheckpointTimeout)
		defer cancel()
	}
	d.lastCkpt = d.e.env.Clock()
	cp := engine.Checkpoint{Downloaded: d.downloaded.Load(), Size: d.size.Load()}
	if err := d.spec.Checkpointer.Checkpoint(ctx, cp); err != nil {
		d.log.WarnContext(ctx, "ytdlp: checkpoint failed", "error", err)
	}
}

func (d *Download) maybeCheckpoint(ctx context.Context) {
	if d.e.env.Clock().Sub(d.lastCkpt) >= d.e.opts.checkpointIntv {
		d.checkpoint(ctx)
	}
}
