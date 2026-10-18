package ytdlp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sabadia/dlm-server/internal/platform"
)

// Asset is one downloadable build.
type Asset struct {
	URL string
	// SHA256 is the hex digest of the file.
	SHA256 string
}

// Release is a set of standalone builds, by release file name.
type Release struct {
	// Tag names the release; it is the cache directory of its build.
	Tag    string
	Assets map[string]Asset
}

// DefaultRelease is the pinned yt-dlp release 2026.08.19 with the SHA-256 of
// each build (from the digests GitHub reports for the release assets).
// Updating it means changing the tag, the URLs and the digests together.
func DefaultRelease() Release {
	const base = "https://github.com/yt-dlp/yt-dlp/releases/download/2026.08.19/"
	asset := func(name, sum string) Asset { return Asset{URL: base + name, SHA256: sum} }
	return Release{Tag: "2026.08.19", Assets: map[string]Asset{
		"yt-dlp_linux":         asset("yt-dlp_linux", "58162f9bfdc27458ea47bfcb311cf47028f17d8154a8bf7d689861d46399230a"),
		"yt-dlp_linux_aarch64": asset("yt-dlp_linux_aarch64", "b16e4dab368a816cd05d477d698a605a6ae87ccee1c8ffd38fa21d7254141fcc"),
		"yt-dlp_macos":         asset("yt-dlp_macos", "0f192b7ec147ab6288885d6351d9ab67367640029b4377576ef46dd79cf7b202"),
		"yt-dlp.exe":           asset("yt-dlp.exe", "66674953fe251b89f4d08c5f0e35e0728679bd67ab3d7d05c0562af101dd3e7a"),
		"yt-dlp_arm64.exe":     asset("yt-dlp_arm64.exe", "05b438997bafc3affdfda9d041353c9d73e04dc842207254b655b0887c4445b0"),
		"yt-dlp_x86.exe":       asset("yt-dlp_x86.exe", "a8f91bd41452506bc81ebd2f369b186fea0ee7075413ba00cef9fd346a0a5d0c"),
	}}
}

// ErrUnsupportedPlatform is returned when the release has no build for this
// OS and architecture.
var ErrUnsupportedPlatform = errors.New("ytdlp: no yt-dlp build for this platform")

// ErrChecksum is returned when a download does not match its pinned SHA-256.
var ErrChecksum = errors.New("ytdlp: downloaded file does not match its checksum")

// defaultMaxBytes bounds a download.
const defaultMaxBytes = 128 << 20

// Fetcher downloads the pinned yt-dlp build into a cache directory.
type Fetcher struct {
	// Dir is the cache directory; the build lives in Dir/yt-dlp/<tag>/.
	Dir string
	// Release defaults to DefaultRelease().
	Release Release
	// Asset is the release file name for this platform; it defaults to
	// platform.YtdlpAsset().
	Asset string
	// Client defaults to a client with a generous timeout.
	Client *http.Client
	// MaxBytes bounds the download (default 128 MiB).
	MaxBytes int64
}

func (f *Fetcher) release() Release {
	if f.Release.Tag == "" {
		return DefaultRelease()
	}
	return f.Release
}

func (f *Fetcher) asset() string {
	if f.Asset != "" {
		return f.Asset
	}
	return platform.YtdlpAsset()
}

func (f *Fetcher) client() *http.Client {
	if f.Client != nil {
		return f.Client
	}
	return &http.Client{Timeout: 15 * time.Minute}
}

func (f *Fetcher) maxBytes() int64 {
	if f.MaxBytes > 0 {
		return f.MaxBytes
	}
	return defaultMaxBytes
}

// Path is where the build of the release is (or will be) stored.
func (f *Fetcher) Path() string {
	return filepath.Join(f.Dir, "yt-dlp", f.release().Tag, platform.YtdlpExecutable())
}

// Cached returns the path of the downloaded build, when present.
func (f *Fetcher) Cached() (string, bool) {
	info, err := os.Stat(f.Path())
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	return f.Path(), true
}

// Fetch downloads the build for this platform unless it is cached, verifies
// its checksum and returns its path.
func (f *Fetcher) Fetch(ctx context.Context) (string, error) {
	if f.Dir == "" {
		return "", errors.New("ytdlp: no cache directory configured")
	}
	if path, ok := f.Cached(); ok {
		return path, nil
	}
	name := f.asset()
	asset, ok := f.release().Assets[name]
	if name == "" || !ok {
		return "", fmt.Errorf("%w (%q)", ErrUnsupportedPlatform, name)
	}
	dest := f.Path()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil { //nolint:gosec // cache directory
		return "", fmt.Errorf("ytdlp: %w", err)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dest), ".download-")
	if err != nil {
		return "", fmt.Errorf("ytdlp: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }() // best effort: a temporary directory
	file := filepath.Join(tmp, platform.YtdlpExecutable())
	if err := f.download(ctx, asset, file); err != nil {
		return "", err
	}
	if err := os.Chmod(file, 0o755); err != nil { //nolint:gosec // an executable is the point
		return "", fmt.Errorf("ytdlp: %w", err)
	}
	if err := os.Rename(file, dest); err != nil {
		return "", fmt.Errorf("ytdlp: install: %w", err)
	}
	return dest, nil
}

func (f *Fetcher) download(ctx context.Context, a Asset, file string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return fmt.Errorf("ytdlp: %w", err)
	}
	resp, err := f.client().Do(req)
	if err != nil {
		return fmt.Errorf("ytdlp: download: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() // fully read below
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ytdlp: download %s: %s", a.URL, resp.Status)
	}
	out, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700) //nolint:gosec // a fresh path in our temporary directory
	if err != nil {
		return fmt.Errorf("ytdlp: %w", err)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(resp.Body, f.maxBytes()+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("ytdlp: download: %w", err)
	}
	if n > f.maxBytes() {
		return errors.New("ytdlp: the download is too large")
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, a.SHA256) {
		return fmt.Errorf("%w: got %s, want %s", ErrChecksum, got, a.SHA256)
	}
	return nil
}
