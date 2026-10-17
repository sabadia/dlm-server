package browser

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/sabadia/dlm-server/internal/platform"
)

// DefaultVersionsURL is the Chrome for Testing endpoint listing the latest
// stable build and its downloads.
const DefaultVersionsURL = "https://googlechromelabs.github.io/chrome-for-testing/last-known-good-versions-with-downloads.json"

// maxArchiveBytes bounds the downloaded archive and its extracted size.
const maxArchiveBytes = 1 << 30

// ErrUnsupportedPlatform is returned when Chrome for Testing publishes no
// chrome-headless-shell for this OS and architecture.
var ErrUnsupportedPlatform = errors.New("browser: no chrome-headless-shell build for this platform")

// Fetcher downloads chrome-headless-shell from Chrome for Testing into a
// cache directory. The zero value (with Dir set) uses the real endpoint.
type Fetcher struct {
	// Dir is the cache directory; the shell is stored below
	// Dir/chrome-headless-shell/<version>.
	Dir string
	// VersionsURL defaults to DefaultVersionsURL.
	VersionsURL string
	// Platform defaults to platform.ChromeForTestingPlatform().
	Platform string
	// Client defaults to a client with a generous timeout.
	Client *http.Client
}

type versionsDoc struct {
	Channels map[string]struct {
		Version   string `json:"version"`
		Downloads map[string][]struct {
			Platform string `json:"platform"`
			URL      string `json:"url"`
		} `json:"downloads"`
	} `json:"channels"`
}

func (f *Fetcher) client() *http.Client {
	if f.Client != nil {
		return f.Client
	}
	return &http.Client{Timeout: 10 * time.Minute}
}

// exeName is the executable name inside the archive.
func (f *Fetcher) exeName() string { return platform.HeadlessShellExecutable() }

func (f *Fetcher) root() string { return filepath.Join(f.Dir, "chrome-headless-shell") }

// Cached returns the newest chrome-headless-shell already in the cache, or
// false.
func (f *Fetcher) Cached() (Installation, bool) {
	entries, err := os.ReadDir(f.root())
	if err != nil {
		return Installation{}, false
	}
	var best Installation
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		exe, ok := findExecutable(filepath.Join(f.root(), e.Name()))
		if ok && compareVersions(e.Name(), best.Version) > 0 {
			best = Installation{Name: NameHeadlessShell, Path: exe, Version: e.Name()}
		}
	}
	return best, best.Path != ""
}

// Fetch returns the newest cached chrome-headless-shell, downloading the
// latest stable one first when the cache has none for that version.
func (f *Fetcher) Fetch(ctx context.Context) (Installation, error) {
	plat := f.Platform
	if plat == "" {
		plat = platform.ChromeForTestingPlatform()
	}
	if plat == "" {
		return Installation{}, ErrUnsupportedPlatform
	}
	version, url, err := f.latest(ctx, plat)
	if err != nil {
		return Installation{}, err
	}
	dest := filepath.Join(f.root(), version)
	if exe, ok := findExecutable(dest); ok {
		return Installation{Name: NameHeadlessShell, Path: exe, Version: version}, nil
	}
	if err := f.download(ctx, url, dest); err != nil {
		return Installation{}, err
	}
	exe, ok := findExecutable(dest)
	if !ok {
		return Installation{}, fmt.Errorf("browser: %s has no %s after download", dest, platform.HeadlessShellExecutable())
	}
	return Installation{Name: NameHeadlessShell, Path: exe, Version: version}, nil
}

func (f *Fetcher) latest(ctx context.Context, plat string) (version, url string, err error) {
	endpoint := f.VersionsURL
	if endpoint == "" {
		endpoint = DefaultVersionsURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", "", fmt.Errorf("browser: %w", err)
	}
	resp, err := f.client().Do(req)
	if err != nil {
		return "", "", fmt.Errorf("browser: fetch versions: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() // fully read below
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("browser: fetch versions: %s", resp.Status)
	}
	var doc versionsDoc
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return "", "", fmt.Errorf("browser: decode versions: %w", err)
	}
	stable, ok := doc.Channels["Stable"]
	if !ok || stable.Version == "" || !validVersion(stable.Version) {
		return "", "", errors.New("browser: versions document has no usable Stable channel")
	}
	for _, d := range stable.Downloads["chrome-headless-shell"] {
		if d.Platform == plat {
			return stable.Version, d.URL, nil
		}
	}
	return "", "", fmt.Errorf("%w (%s)", ErrUnsupportedPlatform, plat)
}

// download fetches the archive at url and extracts it into dest through a
// temporary directory, so dest only ever appears complete.
func (f *Fetcher) download(ctx context.Context, url, dest string) error {
	if err := os.MkdirAll(f.root(), 0o755); err != nil { //nolint:gosec // cache directory, user-readable
		return fmt.Errorf("browser: %w", err)
	}
	tmp, err := os.MkdirTemp(f.root(), ".download-")
	if err != nil {
		return fmt.Errorf("browser: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }() // best effort: it is a temporary directory
	archive := filepath.Join(tmp, "archive.zip")
	if err := f.saveURL(ctx, url, archive); err != nil {
		return err
	}
	staged := filepath.Join(tmp, "extracted")
	if err := unzip(archive, staged); err != nil {
		return err
	}
	if err := os.RemoveAll(dest); err != nil {
		return fmt.Errorf("browser: %w", err)
	}
	if err := os.Rename(staged, dest); err != nil {
		return fmt.Errorf("browser: install chrome-headless-shell: %w", err)
	}
	return nil
}

func (f *Fetcher) saveURL(ctx context.Context, url, file string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("browser: %w", err)
	}
	resp, err := f.client().Do(req)
	if err != nil {
		return fmt.Errorf("browser: download chrome-headless-shell: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() // fully read below
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("browser: download chrome-headless-shell: %s", resp.Status)
	}
	out, err := os.Create(file) //nolint:gosec // a fresh path inside our temporary directory
	if err != nil {
		return fmt.Errorf("browser: %w", err)
	}
	n, err := io.Copy(out, io.LimitReader(resp.Body, maxArchiveBytes+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("browser: download chrome-headless-shell: %w", err)
	}
	if n > maxArchiveBytes {
		return errors.New("browser: chrome-headless-shell archive is too large")
	}
	return nil
}

// unzip extracts archive into dest, refusing entries that would escape it.
func unzip(archive, dest string) error {
	r, err := zip.OpenReader(archive)
	if err != nil {
		return fmt.Errorf("browser: open archive: %w", err)
	}
	defer func() { _ = r.Close() }()                 // read-only
	if err := os.MkdirAll(dest, 0o755); err != nil { //nolint:gosec // cache directory, user-readable
		return fmt.Errorf("browser: %w", err)
	}
	var total uint64
	for _, zf := range r.File {
		target := filepath.Join(dest, filepath.FromSlash(zf.Name))
		if !filepath.IsLocal(filepath.FromSlash(zf.Name)) {
			return fmt.Errorf("browser: archive entry %q escapes the destination", zf.Name)
		}
		if zf.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil { //nolint:gosec // cache directory, user-readable
				return fmt.Errorf("browser: %w", err)
			}
			continue
		}
		if zf.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("browser: archive entry %q is a symbolic link", zf.Name)
		}
		total += zf.UncompressedSize64
		if total > maxArchiveBytes {
			return errors.New("browser: chrome-headless-shell archive expands too large")
		}
		if err := extractFile(zf, target); err != nil {
			return err
		}
	}
	return nil
}

func extractFile(zf *zip.File, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil { //nolint:gosec // cache directory, user-readable
		return fmt.Errorf("browser: %w", err)
	}
	src, err := zf.Open()
	if err != nil {
		return fmt.Errorf("browser: open %q: %w", zf.Name, err)
	}
	defer func() { _ = src.Close() }() // read-only
	mode := zf.Mode().Perm() | 0o600
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode) //nolint:gosec // path checked by IsLocal above
	if err != nil {
		return fmt.Errorf("browser: %w", err)
	}
	_, err = io.Copy(out, io.LimitReader(src, int64(min(zf.UncompressedSize64, maxArchiveBytes))+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("browser: extract %q: %w", zf.Name, err)
	}
	return nil
}

// findExecutable locates chrome-headless-shell below dir (it sits one
// directory down, in chrome-headless-shell-<platform>/).
func findExecutable(dir string) (string, bool) {
	name := platform.HeadlessShellExecutable()
	var found string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error { //nolint:errcheck // a walk failure just means "not found"
		if err != nil {
			return nil //nolint:nilerr // unreadable entries are skipped
		}
		if !d.IsDir() && d.Name() == name && strings.Count(strings.TrimPrefix(p, dir), string(filepath.Separator)) <= 2 {
			found = p
			return filepath.SkipAll
		}
		return nil
	})
	return found, found != ""
}

// validVersion accepts dotted numeric versions, which become directory names.
func validVersion(v string) bool {
	if v == "" || path.Clean(v) != v {
		return false
	}
	for _, r := range v {
		if (r < '0' || r > '9') && r != '.' {
			return false
		}
	}
	return !strings.HasPrefix(v, ".") && !strings.HasSuffix(v, ".")
}

// compareVersions compares dotted numeric versions; "" is the oldest.
func compareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < max(len(as), len(bs)); i++ {
		var x, y int
		if i < len(as) {
			x = atoi(as[i])
		}
		if i < len(bs) {
			y = atoi(bs[i])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}
