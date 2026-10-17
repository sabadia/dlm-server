// Package browserx is the engine for files the headless browser downloaded
// itself: JS-made blob: downloads and attachments it saved while a page ran
// (see browser.Resolver). Such a file only exists in the browser's staging
// directory, so the "download" is an import: the file is moved to its final
// place and the task completes at once. Everything the browser can serve over
// a plain URL is downloaded by the HTTP engine instead.
//
// A source names a staged file as dlm-browser://<result id>/<candidate index>
// (see URL); the files are looked up through a Provider.
package browserx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/sabadia/dlm-server/internal/browser"
	"github.com/sabadia/dlm-server/internal/domain"
	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/platform"
)

// EngineName is the name of the engine, stored on tasks.
const EngineName = "browser"

// Scheme is the URL scheme of staged browser files.
const Scheme = "dlm-browser"

// PartSuffix is appended to the destination path while the file is moved.
const PartSuffix = ".part"

// ErrGone is returned when the staged file is no longer there (the resolve
// result expired or was released).
var ErrGone = errors.New("browserx: the file downloaded by the browser is gone; resolve the page again")

// ErrDestinationExists is returned when the final file already exists.
var ErrDestinationExists = errors.New("browserx: destination file already exists")

// Provider finds the files the browser staged.
type Provider interface {
	// Candidate returns candidate index of the stored resolve result id, or an
	// error (browser.ErrResultGone, domain.ErrNotFound).
	Candidate(id string, index int) (browser.Candidate, error)
}

// URL returns the source URL of a staged candidate.
func URL(resultID string, index int) string {
	return Scheme + "://" + resultID + "/" + strconv.Itoa(index)
}

// ParseURL splits a source URL made by URL.
func ParseURL(raw string) (resultID string, index int, err error) {
	u, perr := url.Parse(strings.TrimSpace(raw))
	if perr != nil || u.Scheme != Scheme || u.Host == "" {
		return "", 0, domain.InvalidArgument("browserx: %q is not a %s:// URL", raw, Scheme)
	}
	index, perr = strconv.Atoi(strings.TrimPrefix(u.Path, "/"))
	if perr != nil || index < 0 {
		return "", 0, domain.InvalidArgument("browserx: %q has no candidate index", raw)
	}
	return u.Host, index, nil
}

// Engine imports staged browser files.
type Engine struct {
	files Provider
}

var _ engine.Engine = (*Engine)(nil)

// New returns the engine reading staged files from files.
func New(files Provider) *Engine { return &Engine{files: files} }

// Name implements engine.Engine.
func (*Engine) Name() string { return EngineName }

// Match implements engine.Engine: dlm-browser:// URLs.
func (*Engine) Match(src domain.Source) bool {
	u, err := url.Parse(strings.TrimSpace(src.CurrentURL))
	return err == nil && strings.EqualFold(u.Scheme, Scheme) && u.Host != ""
}

// Resolve implements engine.Engine: it describes the staged file.
func (e *Engine) Resolve(_ context.Context, req engine.Request) (*engine.Probe, error) {
	c, err := e.lookup(req.Source.CurrentURL)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(c.File)
	if err != nil {
		return nil, engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("%w: %w", ErrGone, err))
	}
	name := cmpOr(c.Filename, filepath.Base(c.File))
	src := req.Source
	src.Fingerprint = domain.Fingerprint{Size: info.Size(), Filename: name, ContentType: c.MIME}
	return &engine.Probe{
		Engine: EngineName, Source: src, Suggested: name,
		Resource: domain.Resource{Name: name, Size: info.Size(), Files: []domain.File{{Index: 0, Name: name, Size: info.Size(), Selected: true}}},
	}, nil
}

func (e *Engine) lookup(rawURL string) (browser.Candidate, error) {
	id, idx, err := ParseURL(rawURL)
	if err != nil {
		return browser.Candidate{}, engine.Fatal(domain.ErrorCodeFatal, err)
	}
	c, err := e.files.Candidate(id, idx)
	if err != nil {
		return browser.Candidate{}, engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("%w: %w", ErrGone, err))
	}
	if c.File == "" {
		return browser.Candidate{}, engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("browserx: candidate %s was not downloaded by the browser", rawURL))
	}
	return c, nil
}

// NewDownload implements engine.Engine. It takes ownership of spec.Probe.
func (e *Engine) NewDownload(spec engine.Spec) (engine.Download, error) {
	_ = spec.Probe.Close() // nil-safe; this engine keeps nothing open
	if spec.Path == "" || !filepath.IsAbs(spec.Path) {
		return nil, engine.Fatal(domain.ErrorCodeFatal, domain.InvalidArgument("browserx: download path %q must be absolute", spec.Path))
	}
	if !e.Match(spec.Source) {
		return nil, engine.Fatal(domain.ErrorCodeFatal, domain.InvalidArgument("browserx: %q is not a staged browser file", spec.Source.CurrentURL))
	}
	d := &Download{e: e, spec: spec, part: spec.Path + PartSuffix}
	d.size.Store(-1)
	if spec.Resource != nil && spec.Resource.Size > 0 {
		d.size.Store(spec.Resource.Size)
	}
	return d, nil
}

// Download moves one staged file into place.
type Download struct {
	e    *Engine
	spec engine.Spec
	part string

	started    atomic.Bool
	downloaded atomic.Int64
	size       atomic.Int64
}

// Progress implements engine.Download.
func (d *Download) Progress() engine.Progress {
	return engine.Progress{Downloaded: d.downloaded.Load(), Size: d.size.Load(), Connections: 0}
}

// Run implements engine.Download.
func (d *Download) Run(ctx context.Context) error {
	if !d.started.CompareAndSwap(false, true) {
		return engine.Fatal(domain.ErrorCodeFatal, errors.New("browserx: Run called more than once"))
	}
	if _, err := os.Lstat(d.spec.Path); err == nil {
		return engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("%w: %q", ErrDestinationExists, d.spec.Path))
	} else if !errors.Is(err, fs.ErrNotExist) {
		return engine.Fatal(domain.ErrorCodeFatal, fmt.Errorf("browserx: stat %q: %w", d.spec.Path, err))
	}
	if err := os.MkdirAll(filepath.Dir(d.spec.Path), 0o755); err != nil { //nolint:gosec // download directories are user-visible
		return d.fileError("create directory", err)
	}
	staged, err := d.stagedPath()
	if err != nil {
		return err
	}
	if staged != "" { // otherwise an earlier run already moved it into .part
		if err := d.move(ctx, staged); err != nil {
			return err
		}
	}
	info, err := os.Stat(d.part)
	if err != nil {
		return d.fileError("stat", err)
	}
	if err := os.Rename(d.part, d.spec.Path); err != nil {
		return d.fileError("rename", err)
	}
	d.size.Store(info.Size())
	d.downloaded.Store(info.Size())
	d.checkpoint(ctx, info.Size())
	return nil
}

// stagedPath returns the staged file, or "" when it is gone but a complete
// .part exists (the rename of an earlier run succeeded before the final one).
func (d *Download) stagedPath() (string, error) {
	c, err := d.e.lookup(d.spec.Source.CurrentURL)
	if err == nil {
		if _, serr := os.Stat(c.File); serr == nil {
			return c.File, nil
		}
	}
	if _, perr := os.Stat(d.part); perr == nil {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return "", engine.Fatal(domain.ErrorCodeFatal, ErrGone)
}

// move puts the staged file at .part: a rename when both are on one volume,
// else a copy (the staged file is removed afterwards).
func (d *Download) move(ctx context.Context, staged string) error {
	if err := os.Rename(staged, d.part); err == nil {
		return nil
	}
	in, err := os.Open(staged) //nolint:gosec // a path from the browser's own staging directory
	if err != nil {
		return d.fileError("open", err)
	}
	defer func() { _ = in.Close() }() // read-only
	if info, serr := in.Stat(); serr == nil {
		d.size.Store(info.Size())
	}
	out, err := os.OpenFile(d.part, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644) //nolint:gosec // user-visible download file
	if err != nil {
		return d.fileError("create", err)
	}
	if err := copyContext(ctx, out, in, &d.downloaded); err != nil {
		_ = out.Close() // the copy failed; the error is what matters
		if ctx.Err() != nil {
			return ctx.Err() //nolint:wrapcheck // cancellation is reported as is
		}
		return d.fileError("copy", err)
	}
	if err := out.Close(); err != nil {
		return d.fileError("write", err)
	}
	_ = os.Remove(staged) // the copy is complete; a leftover is cleaned with its result
	return nil
}

func (d *Download) checkpoint(ctx context.Context, size int64) {
	if d.spec.Checkpointer == nil {
		return
	}
	_ = d.spec.Checkpointer.Checkpoint(ctx, engine.Checkpoint{Downloaded: size, Size: size}) //nolint:errcheck // a failed final checkpoint must not fail a finished import
}

func (d *Download) fileError(op string, err error) error {
	err = fmt.Errorf("browserx: %s %q: %w", op, d.part, err)
	if platform.IsDiskFull(err) {
		return engine.Fatal(domain.ErrorCodeDiskFull, err)
	}
	return engine.Fatal(domain.ErrorCodeFatal, err)
}

// copyContext copies in to out, counting bytes, until EOF or ctx ends.
func copyContext(ctx context.Context, out io.Writer, in io.Reader, count *atomic.Int64) error {
	buf := make([]byte, 256<<10)
	for {
		if err := ctx.Err(); err != nil {
			return err //nolint:wrapcheck // cancellation is reported as is
		}
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return werr //nolint:wrapcheck // wrapped with the file name by the caller
			}
			count.Add(int64(n))
		}
		if errors.Is(rerr, io.EOF) {
			return nil
		}
		if rerr != nil {
			return rerr //nolint:wrapcheck // wrapped with the file name by the caller
		}
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
