package ffmpeg

import (
	"compress/gzip"
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

// Asset is one downloadable build: a gzip-compressed executable.
type Asset struct {
	URL string
	// SHA256 is the hex digest of the downloaded (compressed) file.
	SHA256 string
}

// Release is a set of static builds, one per platform.
type Release struct {
	// Tag names the release; it is the cache directory of its builds.
	Tag    string
	Assets map[string]Asset
}

// DefaultRelease is the pinned set of static ffmpeg builds (the
// eugeneware/ffmpeg-static release b6.1.1) with the SHA-256 of each file.
// Updating it means changing the tag, the URLs and the digests together.
func DefaultRelease() Release {
	const base = "https://github.com/eugeneware/ffmpeg-static/releases/download/b6.1.1/ffmpeg-"
	asset := func(platform, sum string) Asset { return Asset{URL: base + platform + ".gz", SHA256: sum} }
	return Release{Tag: "b6.1.1", Assets: map[string]Asset{
		"darwin-arm64": asset("darwin-arm64", "8923876afa8db5585022d7860ec7e589af192f441c56793971276d450ed3bbfa"),
		"darwin-x64":   asset("darwin-x64", "929b375c1182d956c51f7ac25e0b2b0411fb01f6f407aa15c9758efeb4242106"),
		"linux-arm64":  asset("linux-arm64", "754a678672298bc68156adff58aa7385a592c2b30b1d0ae8750c45c915c4bac0"),
		"linux-x64":    asset("linux-x64", "bfe8a8fc511530457b528c48d77b5737527b504a3797a9bc4866aeca69c2dffa"),
		"linux-arm":    asset("linux-arm", "64b115a12f0ab77c277e3c418aae8b40ef881e75e746a0e2d066a206b9bc5172"),
		"linux-ia32":   asset("linux-ia32", "169b27c078a8ecedb814cac67afccf15a9868d63e9d74ef86088adefaa500d00"),
		"win32-x64":    asset("win32-x64", "8883a3dffbd0a16cf4ef95206ea05283f78908dbfb118f73c83f4951dcc06d77"),
	}}
}

// ErrUnsupportedPlatform is returned when the release has no build for this
// OS and architecture.
var ErrUnsupportedPlatform = errors.New("ffmpeg: no static build for this platform")

// ErrChecksum is returned when a download does not match its pinned SHA-256.
var ErrChecksum = errors.New("ffmpeg: downloaded file does not match its checksum")

// defaultMaxBytes bounds the compressed and the unpacked size.
const defaultMaxBytes = 512 << 20

// Fetcher downloads a pinned ffmpeg build into a cache directory.
type Fetcher struct {
	// Dir is the cache directory; builds live in Dir/ffmpeg/<tag>/.
	Dir string
	// Release defaults to DefaultRelease().
	Release Release
	// Platform defaults to platform.FFmpegPlatform().
	Platform string
	// Client defaults to a client with a generous timeout.
	Client *http.Client
	// MaxBytes bounds the compressed and the unpacked size (default 512 MiB).
	MaxBytes int64
}

func (f *Fetcher) release() Release {
	if f.Release.Tag == "" {
		return DefaultRelease()
	}
	return f.Release
}

func (f *Fetcher) plat() string {
	if f.Platform != "" {
		return f.Platform
	}
	return platform.FFmpegPlatform()
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
	return filepath.Join(f.Dir, "ffmpeg", f.release().Tag, platform.FFmpegExecutable())
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
// the checksum of the download, unpacks it and returns its path.
func (f *Fetcher) Fetch(ctx context.Context) (string, error) {
	if f.Dir == "" {
		return "", errors.New("ffmpeg: no cache directory configured")
	}
	if path, ok := f.Cached(); ok {
		return path, nil
	}
	plat := f.plat()
	asset, ok := f.release().Assets[plat]
	if plat == "" || !ok {
		return "", fmt.Errorf("%w (%q)", ErrUnsupportedPlatform, plat)
	}
	dest := f.Path()
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil { //nolint:gosec // cache directory
		return "", fmt.Errorf("ffmpeg: %w", err)
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dest), ".download-")
	if err != nil {
		return "", fmt.Errorf("ffmpeg: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }() // best effort: a temporary directory
	archive := filepath.Join(tmp, "ffmpeg.gz")
	if err := f.download(ctx, asset, archive); err != nil {
		return "", err
	}
	unpacked := filepath.Join(tmp, platform.FFmpegExecutable())
	if err := f.gunzip(archive, unpacked); err != nil {
		return "", err
	}
	if err := os.Chmod(unpacked, 0o755); err != nil { //nolint:gosec // an executable is the point
		return "", fmt.Errorf("ffmpeg: %w", err)
	}
	if err := os.Rename(unpacked, dest); err != nil {
		return "", fmt.Errorf("ffmpeg: install: %w", err)
	}
	return dest, nil
}

func (f *Fetcher) download(ctx context.Context, a Asset, file string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return fmt.Errorf("ffmpeg: %w", err)
	}
	resp, err := f.client().Do(req)
	if err != nil {
		return fmt.Errorf("ffmpeg: download: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() // fully read below
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ffmpeg: download %s: %s", a.URL, resp.Status)
	}
	out, err := os.Create(file) //nolint:gosec // a fresh path in our temporary directory
	if err != nil {
		return fmt.Errorf("ffmpeg: %w", err)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(resp.Body, f.maxBytes()+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("ffmpeg: download: %w", err)
	}
	if n > f.maxBytes() {
		return errors.New("ffmpeg: the download is too large")
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, a.SHA256) {
		return fmt.Errorf("%w: got %s, want %s", ErrChecksum, got, a.SHA256)
	}
	return nil
}

func (f *Fetcher) gunzip(archive, dest string) error {
	in, err := os.Open(archive) //nolint:gosec // our own temporary file
	if err != nil {
		return fmt.Errorf("ffmpeg: %w", err)
	}
	defer func() { _ = in.Close() }() // read-only
	zr, err := gzip.NewReader(in)
	if err != nil {
		return fmt.Errorf("ffmpeg: unpack: %w", err)
	}
	defer func() { _ = zr.Close() }()                                       // read-only
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700) //nolint:gosec // a fresh path in our temporary directory
	if err != nil {
		return fmt.Errorf("ffmpeg: %w", err)
	}
	n, err := io.Copy(out, io.LimitReader(zr, f.maxBytes()+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("ffmpeg: unpack: %w", err)
	}
	if n > f.maxBytes() {
		return errors.New("ffmpeg: the unpacked file is too large")
	}
	return nil
}
