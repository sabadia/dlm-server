package ytdlp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ErrNotFound is returned when no usable yt-dlp was found and none can be
// downloaded.
var ErrNotFound = errors.New("ytdlp: yt-dlp was not found")

// Binary is a usable yt-dlp executable.
type Binary struct {
	Path string `json:"path"`
	// Version is the version yt-dlp reports ("2026.08.19").
	Version string `json:"version"`
	// Source is where it came from: "configured", "path", "cache" or "download".
	Source string `json:"source"`
}

// Options configures a Locator.
type Options struct {
	// Path is the yt-dlp executable to use; empty searches for one.
	Path string
	// CacheDir holds downloaded builds.
	CacheDir string
	// AutoDownload lets the Locator download the pinned build when none is
	// installed.
	AutoDownload bool
	// Fetcher overrides the downloader (tests); default Fetcher{Dir: CacheDir}.
	Fetcher *Fetcher
	// LookPath finds an executable on PATH (default exec.LookPath).
	LookPath func(string) (string, error)
	// Version runs `path --version` and returns the version (default: really
	// runs it).
	Version func(ctx context.Context, path string) (string, error)
	Logger  *slog.Logger
}

// Locator finds yt-dlp once and remembers it. It is safe for concurrent use.
type Locator struct {
	opts Options
	log  *slog.Logger

	mu    sync.Mutex
	found *Binary
}

// NewLocator creates a Locator. It does nothing until Locate.
func NewLocator(opts Options) *Locator {
	if opts.LookPath == nil {
		opts.LookPath = exec.LookPath
	}
	if opts.Version == nil {
		opts.Version = runVersion
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Locator{opts: opts, log: log}
}

// Locate returns the yt-dlp to use: the configured path, else yt-dlp on PATH,
// else a downloaded build in the cache, else (with AutoDownload) a freshly
// downloaded one. A configured path that does not work is an error: it is not
// silently replaced. The result is remembered.
func (l *Locator) Locate(ctx context.Context) (Binary, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.found != nil {
		return *l.found, nil
	}
	b, err := l.find(ctx)
	if err != nil {
		return Binary{}, err
	}
	l.found = &b
	l.log.Info("ytdlp: using", "path", b.Path, "version", b.Version, "source", b.Source)
	return b, nil
}

func (l *Locator) find(ctx context.Context) (Binary, error) {
	if l.opts.Path != "" {
		v, err := l.opts.Version(ctx, l.opts.Path)
		if err != nil {
			return Binary{}, fmt.Errorf("ytdlp: the configured path %q does not work: %w", l.opts.Path, err)
		}
		return Binary{Path: l.opts.Path, Version: v, Source: "configured"}, nil
	}
	if path, err := l.opts.LookPath("yt-dlp"); err == nil {
		if v, verr := l.opts.Version(ctx, path); verr == nil {
			return Binary{Path: path, Version: v, Source: "path"}, nil
		}
	}
	f := l.fetcher()
	if f.Dir == "" {
		return Binary{}, fmt.Errorf("%w: install yt-dlp or set its path", ErrNotFound)
	}
	if path, ok := f.Cached(); ok {
		if v, err := l.opts.Version(ctx, path); err == nil {
			return Binary{Path: path, Version: v, Source: "cache"}, nil
		}
		l.log.Warn("ytdlp: the cached build does not run; ignoring it", "path", path)
	}
	if !l.opts.AutoDownload {
		return Binary{}, fmt.Errorf("%w: install yt-dlp, set its path, or allow downloading it", ErrNotFound)
	}
	l.log.Info("ytdlp: downloading yt-dlp")
	path, err := f.Fetch(ctx)
	if err != nil {
		return Binary{}, fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	v, err := l.opts.Version(ctx, path)
	if err != nil {
		_ = os.Remove(path) // an unusable download must not be reused
		return Binary{}, fmt.Errorf("%w: the downloaded build does not run: %w", ErrNotFound, err)
	}
	return Binary{Path: path, Version: v, Source: "download"}, nil
}

func (l *Locator) fetcher() *Fetcher {
	if l.opts.Fetcher != nil {
		return l.opts.Fetcher
	}
	return &Fetcher{Dir: l.opts.CacheDir}
}

// versionTimeout bounds `yt-dlp --version`: the standalone builds unpack
// themselves on first start.
const versionTimeout = 60 * time.Second

func runVersion(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, path, "--ignore-config", "--version") //nolint:gosec // the user's or our own yt-dlp
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("run %s --version: %w", path, err)
	}
	return ParseVersion(out.String())
}

var versionRE = regexp.MustCompile(`^\d{4}\.\d{2}\.\d{2}(\.\d+)?$`)

// ParseVersion checks the output of `yt-dlp --version` and returns the
// version (a date such as 2026.08.19, with a revision suffix for rebuilds).
func ParseVersion(out string) (string, error) {
	v := strings.TrimSpace(out)
	if !versionRE.MatchString(v) {
		return "", fmt.Errorf("not yt-dlp: unexpected output %q", truncate(v, 60))
	}
	return v, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
