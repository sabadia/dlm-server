package ffmpeg

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sabadia/dlm-server/internal/platform"
)

func TestDefaultReleaseIsWellFormed(t *testing.T) {
	r := DefaultRelease()
	hex64 := regexp.MustCompile(`^[0-9a-f]{64}$`)
	for plat, a := range r.Assets {
		if !hex64.MatchString(a.SHA256) {
			t.Errorf("%s: digest %q is not 64 lower-case hex digits", plat, a.SHA256)
		}
		if want := "https://github.com/eugeneware/ffmpeg-static/releases/download/" + r.Tag + "/ffmpeg-" + plat + ".gz"; a.URL != want {
			t.Errorf("%s: URL = %q, want %q", plat, a.URL, want)
		}
	}
	// Every platform dlm can run on has a build.
	for _, p := range []string{"linux-x64", "linux-arm64", "darwin-arm64", "darwin-x64", "win32-x64"} {
		if _, ok := r.Assets[p]; !ok {
			t.Errorf("no build for %s", p)
		}
	}
	if _, ok := r.Assets[platform.FFmpegPlatform()]; platform.FFmpegPlatform() != "" && !ok {
		t.Errorf("no build for this machine (%s)", platform.FFmpegPlatform())
	}
}

func TestParseVersion(t *testing.T) {
	for in, want := range map[string]string{
		"ffmpeg version 6.1.1 Copyright (c) 2000-2023 the FFmpeg developers\nbuilt with gcc": "6.1.1",
		"ffmpeg version N-117000-gabc123 Copyright":                                          "N-117000-gabc123",
		"ffmpeg version 9.0.2\n":                                                             "9.0.2",
	} {
		if got, err := ParseVersion(in); err != nil || got != want {
			t.Errorf("ParseVersion(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "ffprobe version 6.0", "not ffmpeg at all", "ffmpeg version \n"} {
		if _, err := ParseVersion(bad); err == nil {
			t.Errorf("ParseVersion(%q) accepted", bad)
		}
	}
}

// fakeRelease serves a gzip-compressed "executable" and returns the release
// pointing at it.
func fakeRelease(t *testing.T, content string, tamper bool) (Release, *atomic.Int32) {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte(content))
	_ = zw.Close()
	sum := sha256.Sum256(buf.Bytes())
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		body := buf.Bytes()
		if tamper {
			body = append(bytes.Clone(body), 0)
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return Release{Tag: "test", Assets: map[string]Asset{"testos": {URL: srv.URL + "/ffmpeg.gz", SHA256: hex.EncodeToString(sum[:])}}}, &hits
}

func TestFetch(t *testing.T) {
	ctx := context.Background()
	t.Run("downloads, verifies and installs once", func(t *testing.T) {
		rel, hits := fakeRelease(t, "#!/bin/sh\necho hi\n", false)
		f := &Fetcher{Dir: t.TempDir(), Release: rel, Platform: "testos"}
		if _, ok := f.Cached(); ok {
			t.Fatal("Cached found something in an empty directory")
		}
		path, err := f.Fetch(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(f.Dir, "ffmpeg", "test", platform.FFmpegExecutable()); path != want {
			t.Errorf("path = %q, want %q", path, want)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != "#!/bin/sh\necho hi\n" {
			t.Errorf("installed file = %q, %v", got, err)
		}
		if info, _ := os.Stat(path); info.Mode().Perm()&0o100 == 0 && !strings.HasSuffix(path, ".exe") {
			t.Errorf("not executable: %v", info.Mode())
		}
		if again, err := f.Fetch(ctx); err != nil || again != path || hits.Load() != 1 {
			t.Errorf("second Fetch = %q, %v after %d downloads", again, err, hits.Load())
		}
		entries, _ := os.ReadDir(filepath.Dir(path))
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".") {
				t.Errorf("leftover %s", e.Name())
			}
		}
	})
	t.Run("rejects a download that does not match its checksum", func(t *testing.T) {
		rel, _ := fakeRelease(t, "payload", true)
		f := &Fetcher{Dir: t.TempDir(), Release: rel, Platform: "testos"}
		if _, err := f.Fetch(ctx); !errors.Is(err, ErrChecksum) {
			t.Errorf("Fetch = %v, want ErrChecksum", err)
		}
		if _, ok := f.Cached(); ok {
			t.Error("a corrupt download was installed")
		}
	})
	t.Run("unsupported platform", func(t *testing.T) {
		f := &Fetcher{Dir: t.TempDir(), Release: Release{Tag: "t", Assets: map[string]Asset{}}, Platform: "plan9"}
		if _, err := f.Fetch(ctx); !errors.Is(err, ErrUnsupportedPlatform) {
			t.Errorf("Fetch = %v, want ErrUnsupportedPlatform", err)
		}
	})
	t.Run("size limits", func(t *testing.T) {
		rel, _ := fakeRelease(t, strings.Repeat("x", 100_000), false)
		f := &Fetcher{Dir: t.TempDir(), Release: rel, Platform: "testos", MaxBytes: 1000}
		if _, err := f.Fetch(ctx); err == nil {
			t.Error("an oversized unpacked file was accepted")
		}
	})
	t.Run("server error and cancellation", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		t.Cleanup(srv.Close)
		rel := Release{Tag: "t", Assets: map[string]Asset{"testos": {URL: srv.URL, SHA256: strings.Repeat("0", 64)}}}
		if _, err := (&Fetcher{Dir: t.TempDir(), Release: rel, Platform: "testos"}).Fetch(ctx); err == nil {
			t.Error("a 404 was accepted")
		}
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		good, _ := fakeRelease(t, "x", false)
		if _, err := (&Fetcher{Dir: t.TempDir(), Release: good, Platform: "testos"}).Fetch(cctx); err == nil {
			t.Error("a cancelled context was ignored")
		}
		if _, err := (&Fetcher{}).Fetch(ctx); err == nil {
			t.Error("Fetch without a directory succeeded")
		}
	})
}

func versionOK(string) func(context.Context, string) (string, error) {
	return func(_ context.Context, path string) (string, error) { return "9.9 from " + filepath.Base(path), nil }
}

func TestLocateOrder(t *testing.T) {
	ctx := context.Background()
	rel, hits := fakeRelease(t, "binary", false)
	newFetcher := func(dir string) *Fetcher { return &Fetcher{Dir: dir, Release: rel, Platform: "testos"} }
	notOnPath := func(string) (string, error) { return "", exec.ErrNotFound }
	onPath := func(string) (string, error) { return "/usr/bin/ffmpeg", nil }
	version := func(_ context.Context, path string) (string, error) { return "1.0:" + path, nil }

	t.Run("configured path wins", func(t *testing.T) {
		l := NewLocator(Options{Path: "/opt/ff", LookPath: onPath, Version: version, CacheDir: t.TempDir(), Fetcher: newFetcher(t.TempDir())})
		b, err := l.Locate(ctx)
		if err != nil || b.Source != "configured" || b.Path != "/opt/ff" {
			t.Errorf("Locate = %+v, %v", b, err)
		}
	})
	t.Run("a broken configured path is an error, not a fallback", func(t *testing.T) {
		bad := func(context.Context, string) (string, error) { return "", errors.New("exec format error") }
		l := NewLocator(Options{Path: "/opt/ff", LookPath: onPath, Version: bad, CacheDir: t.TempDir(), AutoDownload: true})
		if _, err := l.Locate(ctx); err == nil || !strings.Contains(err.Error(), "/opt/ff") {
			t.Errorf("Locate = %v", err)
		}
	})
	t.Run("PATH", func(t *testing.T) {
		b, err := NewLocator(Options{LookPath: onPath, Version: version}).Locate(ctx)
		if err != nil || b.Source != "path" || b.Path != "/usr/bin/ffmpeg" {
			t.Errorf("Locate = %+v, %v", b, err)
		}
	})
	t.Run("a PATH entry that is not ffmpeg is skipped", func(t *testing.T) {
		v := func(_ context.Context, path string) (string, error) {
			if path == "/usr/bin/ffmpeg" {
				return "", errors.New("not ffmpeg")
			}
			return "2", nil
		}
		dir := t.TempDir()
		f := newFetcher(dir)
		if _, err := f.Fetch(ctx); err != nil {
			t.Fatal(err)
		}
		b, err := NewLocator(Options{LookPath: onPath, Version: v, CacheDir: dir, Fetcher: f}).Locate(ctx)
		if err != nil || b.Source != "cache" {
			t.Errorf("Locate = %+v, %v", b, err)
		}
	})
	t.Run("cache before download", func(t *testing.T) {
		dir := t.TempDir()
		f := newFetcher(dir)
		if _, err := f.Fetch(ctx); err != nil {
			t.Fatal(err)
		}
		before := hits.Load()
		b, err := NewLocator(Options{LookPath: notOnPath, Version: versionOK(""), CacheDir: dir, Fetcher: f, AutoDownload: true}).Locate(ctx)
		if err != nil || b.Source != "cache" || hits.Load() != before {
			t.Errorf("Locate = %+v, %v (%d new downloads)", b, err, hits.Load()-before)
		}
	})
	t.Run("download when allowed", func(t *testing.T) {
		b, err := NewLocator(Options{LookPath: notOnPath, Version: versionOK(""), CacheDir: t.TempDir(), Fetcher: newFetcher(t.TempDir()), AutoDownload: true}).Locate(ctx)
		if err != nil || b.Source != "download" {
			t.Errorf("Locate = %+v, %v", b, err)
		}
	})
	t.Run("nothing found and no download", func(t *testing.T) {
		_, err := NewLocator(Options{LookPath: notOnPath, Version: versionOK(""), CacheDir: t.TempDir(), Fetcher: newFetcher(t.TempDir())}).Locate(ctx)
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("Locate = %v, want ErrNotFound", err)
		}
		if _, err := NewLocator(Options{LookPath: notOnPath}).Locate(ctx); !errors.Is(err, ErrNotFound) {
			t.Errorf("Locate without a cache directory = %v, want ErrNotFound", err)
		}
	})
	t.Run("a download that does not run is removed", func(t *testing.T) {
		bad := func(context.Context, string) (string, error) { return "", errors.New("exec format error") }
		f := newFetcher(t.TempDir())
		_, err := NewLocator(Options{LookPath: notOnPath, Version: bad, CacheDir: t.TempDir(), Fetcher: f, AutoDownload: true}).Locate(ctx)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("Locate = %v, want ErrNotFound", err)
		}
		if _, ok := f.Cached(); ok {
			t.Error("the unusable download stayed in the cache")
		}
	})
	t.Run("the result is remembered", func(t *testing.T) {
		var calls atomic.Int32
		l := NewLocator(Options{LookPath: func(string) (string, error) { calls.Add(1); return "/x/ffmpeg", nil }, Version: version})
		for range 3 {
			if _, err := l.Locate(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if calls.Load() != 1 {
			t.Errorf("PATH searched %d times, want 1", calls.Load())
		}
	})
}

// TestSystemFFmpeg runs the real `ffmpeg -version` when ffmpeg is installed.
func TestSystemFFmpeg(t *testing.T) {
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	b, err := NewLocator(Options{}).Locate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if b.Source != "path" || b.Version == "" {
		t.Errorf("Locate = %+v, want the ffmpeg on PATH (%s) with a version", b, path)
	}
}

// TestDownloadReal downloads the pinned build for this machine and runs it. It
// needs the network and about 30 MB, so it only runs with DLM_TEST_FFMPEG_DOWNLOAD=1.
func TestDownloadReal(t *testing.T) {
	if os.Getenv("DLM_TEST_FFMPEG_DOWNLOAD") != "1" {
		t.Skip("set DLM_TEST_FFMPEG_DOWNLOAD=1 to download the real ffmpeg build")
	}
	b, err := NewLocator(Options{LookPath: func(string) (string, error) { return "", exec.ErrNotFound }, CacheDir: t.TempDir(), AutoDownload: true}).Locate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if b.Source != "download" || b.Version == "" {
		t.Errorf("Locate = %+v", b)
	}
}

func TestProbeNeverDownloads(t *testing.T) {
	ctx := context.Background()
	rel, hits := fakeRelease(t, "binary", false)
	f := &Fetcher{Dir: t.TempDir(), Release: rel, Platform: "testos"}
	notOnPath := func(string) (string, error) { return "", exec.ErrNotFound }
	l := NewLocator(Options{LookPath: notOnPath, Version: versionOK(""), CacheDir: f.Dir, Fetcher: f, AutoDownload: true})

	if _, err := l.Probe(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Probe with nothing installed = %v, want ErrNotFound", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("Probe downloaded %d times", hits.Load())
	}
	if _, ok := f.Cached(); ok {
		t.Fatal("Probe left a download in the cache")
	}

	// A cached build is found without a download and without being remembered.
	if _, err := f.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	before := hits.Load()
	b, err := l.Probe(ctx)
	if err != nil || b.Source != "cache" || hits.Load() != before {
		t.Errorf("Probe = %+v, %v", b, err)
	}

	// Locate's remembered result is what Probe reports afterwards.
	located, err := l.Locate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := l.Probe(ctx); err != nil || again != located {
		t.Errorf("Probe after Locate = %+v, %v; want %+v", again, err, located)
	}
}
