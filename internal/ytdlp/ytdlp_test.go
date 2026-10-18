package ytdlp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestParseVersion(t *testing.T) {
	for in, want := range map[string]string{"2026.08.19\n": "2026.08.19", "2026.08.19.1": "2026.08.19.1", "\t2025.01.02\r\n": "2025.01.02"} {
		if got, err := ParseVersion(in); err != nil || got != want {
			t.Errorf("ParseVersion(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "ffmpeg version 6", "yt-dlp 2026.08.19", "2026.8.19", "<html>"} {
		if _, err := ParseVersion(in); err == nil {
			t.Errorf("ParseVersion(%q) accepted", in)
		}
	}
}

func TestDefaultReleaseIsConsistent(t *testing.T) {
	r := DefaultRelease()
	sum := regexp.MustCompile(`^[0-9a-f]{64}$`)
	if len(r.Assets) != 6 {
		t.Errorf("%d assets", len(r.Assets))
	}
	for name, a := range r.Assets {
		if !sum.MatchString(a.SHA256) || !strings.HasSuffix(a.URL, "/"+r.Tag+"/"+name) {
			t.Errorf("asset %s = %+v", name, a)
		}
	}
}

func fakeServer(t *testing.T, body string) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/yt-dlp_test" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	h := sha256.Sum256([]byte(body))
	return srv, hex.EncodeToString(h[:])
}

func okVersion(context.Context, string) (string, error) { return "2026.08.19", nil }

func TestFetchVerifiesAndCaches(t *testing.T) {
	srv, sum := fakeServer(t, "#!/bin/sh\necho 2026.08.19\n")
	dir := t.TempDir()
	f := &Fetcher{Dir: dir, Asset: "yt-dlp_test", Release: Release{Tag: "t1", Assets: map[string]Asset{"yt-dlp_test": {URL: srv.URL + "/yt-dlp_test", SHA256: sum}}}}
	path, err := f.Fetch(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if path != f.Path() || !strings.Contains(path, filepath.Join("yt-dlp", "t1")) {
		t.Errorf("path = %q", path)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm()&0o100 == 0 && !strings.HasSuffix(path, ".exe") {
		t.Errorf("not an executable file: %v %v", fi, err)
	}
	srv.Close() // a second Fetch must use the cache
	if p2, err := f.Fetch(t.Context()); err != nil || p2 != path {
		t.Errorf("second fetch = %q, %v", p2, err)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".download-*")); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
}

func TestFetchRefusesBadDownloads(t *testing.T) {
	srv, _ := fakeServer(t, "evil")
	rel := func(sum string) Release {
		return Release{Tag: "t", Assets: map[string]Asset{"yt-dlp_test": {URL: srv.URL + "/yt-dlp_test", SHA256: sum}}}
	}
	f := &Fetcher{Dir: t.TempDir(), Asset: "yt-dlp_test", Release: rel(strings.Repeat("0", 64))}
	if _, err := f.Fetch(t.Context()); !errors.Is(err, ErrChecksum) {
		t.Errorf("err = %v, want ErrChecksum", err)
	}
	if _, ok := f.Cached(); ok {
		t.Error("a build that failed its checksum was installed")
	}
	f = &Fetcher{Dir: t.TempDir(), Asset: "nope", Release: rel("")}
	if _, err := f.Fetch(t.Context()); !errors.Is(err, ErrUnsupportedPlatform) {
		t.Errorf("err = %v, want ErrUnsupportedPlatform", err)
	}
	f = &Fetcher{Dir: t.TempDir(), Asset: "yt-dlp_test", MaxBytes: 2, Release: rel(strings.Repeat("0", 64))}
	if _, err := f.Fetch(t.Context()); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Errorf("err = %v, want too large", err)
	}
	if _, err := (&Fetcher{}).Fetch(t.Context()); err == nil {
		t.Error("no cache dir must fail")
	}
}

func TestLocateOrder(t *testing.T) {
	lookNone := func(string) (string, error) { return "", errors.New("not found") }
	lookPath := func(string) (string, error) { return "/usr/bin/yt-dlp", nil }
	// Configured wins and is never silently replaced.
	l := NewLocator(Options{Path: "/x/yt-dlp", LookPath: lookPath, Version: okVersion})
	if b, err := l.Locate(t.Context()); err != nil || b.Source != "configured" || b.Path != "/x/yt-dlp" {
		t.Errorf("configured = %+v, %v", b, err)
	}
	bad := func(context.Context, string) (string, error) { return "", errors.New("broken") }
	if _, err := NewLocator(Options{Path: "/x/yt-dlp", LookPath: lookPath, Version: bad}).Locate(t.Context()); err == nil {
		t.Error("a broken configured path must be an error")
	}
	// PATH next.
	if b, err := NewLocator(Options{LookPath: lookPath, Version: okVersion}).Locate(t.Context()); err != nil || b.Source != "path" {
		t.Errorf("path = %+v, %v", b, err)
	}
	// Then the cache, then a download (when allowed), and the result is remembered.
	srv, sum := fakeServer(t, "binary")
	dir := t.TempDir()
	f := &Fetcher{Dir: dir, Asset: "yt-dlp_test", Release: Release{Tag: "t", Assets: map[string]Asset{"yt-dlp_test": {URL: srv.URL + "/yt-dlp_test", SHA256: sum}}}}
	if _, err := NewLocator(Options{LookPath: lookNone, Version: okVersion, Fetcher: f}).Locate(t.Context()); !errors.Is(err, ErrNotFound) {
		t.Errorf("without auto-download: err = %v, want ErrNotFound", err)
	}
	l = NewLocator(Options{LookPath: lookNone, Version: okVersion, Fetcher: f, AutoDownload: true})
	if b, err := l.Locate(t.Context()); err != nil || b.Source != "download" {
		t.Errorf("download = %+v, %v", b, err)
	}
	if b, err := NewLocator(Options{LookPath: lookNone, Version: okVersion, Fetcher: f}).Locate(t.Context()); err != nil || b.Source != "cache" {
		t.Errorf("cache = %+v, %v", b, err)
	}
	srv.Close()
	if b, err := l.Locate(t.Context()); err != nil || b.Source != "download" {
		t.Errorf("remembered = %+v, %v", b, err)
	}
	// A download that does not run is removed and reported.
	srv2, sum2 := fakeServer(t, "junk")
	f2 := &Fetcher{Dir: t.TempDir(), Asset: "yt-dlp_test", Release: Release{Tag: "t", Assets: map[string]Asset{"yt-dlp_test": {URL: srv2.URL + "/yt-dlp_test", SHA256: sum2}}}}
	if _, err := NewLocator(Options{LookPath: lookNone, Version: bad, Fetcher: f2, AutoDownload: true}).Locate(t.Context()); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if _, ok := f2.Cached(); ok {
		t.Error("a build that does not run stayed in the cache")
	}
	if _, err := NewLocator(Options{LookPath: lookNone, Version: okVersion}).Locate(t.Context()); !errors.Is(err, ErrNotFound) {
		t.Errorf("no cache dir: err = %v, want ErrNotFound", err)
	}
}
