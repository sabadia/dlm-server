package browser

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/sabadia/dlm-server/internal/platform"
)

func makeZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, body := range files {
		f, err := w.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// cftServer serves a Chrome for Testing versions document and the archive.
type cftServer struct {
	*httptest.Server
	downloads atomic.Int32
}

func newCFT(t *testing.T, version string, archive []byte) *cftServer {
	t.Helper()
	s := &cftServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/versions.json", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"channels":{"Stable":{"version":%q,"downloads":{"chrome-headless-shell":[
		  {"platform":"linux64","url":"%s/dl/linux64.zip"},{"platform":"mac-arm64","url":"%s/dl/mac.zip"}]}}}}`, version, s.URL, s.URL)
	})
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, _ *http.Request) {
		s.downloads.Add(1)
		_, _ = w.Write(archive)
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func shellZip(t *testing.T) []byte {
	exe := platform.HeadlessShellExecutable()
	return makeZip(t, map[string]string{
		"chrome-headless-shell-linux64/" + exe:        "#!/bin/sh\n",
		"chrome-headless-shell-linux64/resources.pak": "data",
	})
}

func TestFetcherDownloadsOnceAndCaches(t *testing.T) {
	srv := newCFT(t, "155.0.1.2", shellZip(t))
	f := &Fetcher{Dir: t.TempDir(), VersionsURL: srv.URL + "/versions.json", Platform: "linux64"}
	if _, ok := f.Cached(); ok {
		t.Fatal("Cached found something in an empty directory")
	}
	inst, err := f.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if inst.Name != NameHeadlessShell || inst.Version != "155.0.1.2" || filepath.Base(inst.Path) != platform.HeadlessShellExecutable() {
		t.Errorf("installation = %+v", inst)
	}
	if info, err := os.Stat(inst.Path); err != nil || info.Size() == 0 {
		t.Errorf("executable missing: %v", err)
	}
	// A second fetch of the same version reuses the cache.
	again, err := f.Fetch(context.Background())
	if err != nil || again.Path != inst.Path || srv.downloads.Load() != 1 {
		t.Errorf("second Fetch = %+v, %v after %d downloads", again, err, srv.downloads.Load())
	}
	if cached, ok := f.Cached(); !ok || cached.Path != inst.Path || cached.Version != "155.0.1.2" {
		t.Errorf("Cached = %+v, %v", cached, ok)
	}
	// No temporary download directories are left behind.
	entries, _ := os.ReadDir(filepath.Join(f.Dir, "chrome-headless-shell"))
	for _, e := range entries {
		if e.Name()[0] == '.' {
			t.Errorf("leftover %s", e.Name())
		}
	}
}

func TestFetcherCachedPicksTheNewestVersion(t *testing.T) {
	f := &Fetcher{Dir: t.TempDir()}
	exe := platform.HeadlessShellExecutable()
	for _, v := range []string{"9.0.0.1", "10.0.0.1", "10.0.0.2"} {
		p := filepath.Join(f.root(), v, "chrome-headless-shell-x", exe)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if got, ok := f.Cached(); !ok || got.Version != "10.0.0.2" {
		t.Errorf("Cached = %+v, %v; want 10.0.0.2 (numeric, not lexical)", got, ok)
	}
}

func TestFetcherErrors(t *testing.T) {
	ctx := context.Background()
	t.Run("unsupported platform", func(t *testing.T) {
		srv := newCFT(t, "155.0.1.2", shellZip(t))
		f := &Fetcher{Dir: t.TempDir(), VersionsURL: srv.URL + "/versions.json", Platform: "win64"}
		if _, err := f.Fetch(ctx); !errors.Is(err, ErrUnsupportedPlatform) {
			t.Errorf("Fetch = %v, want ErrUnsupportedPlatform", err)
		}
	})
	t.Run("no executable in the archive", func(t *testing.T) {
		srv := newCFT(t, "155.0.1.2", makeZip(t, map[string]string{"readme.txt": "x"}))
		f := &Fetcher{Dir: t.TempDir(), VersionsURL: srv.URL + "/versions.json", Platform: "linux64"}
		if _, err := f.Fetch(ctx); err == nil {
			t.Error("Fetch accepted an archive without the executable")
		}
	})
	t.Run("archive entry escapes", func(t *testing.T) {
		srv := newCFT(t, "155.0.1.2", makeZip(t, map[string]string{"../evil": "x"}))
		dir := t.TempDir()
		f := &Fetcher{Dir: dir, VersionsURL: srv.URL + "/versions.json", Platform: "linux64"}
		if _, err := f.Fetch(ctx); err == nil {
			t.Fatal("Fetch accepted a path-traversing archive")
		}
		if _, err := os.Stat(filepath.Join(dir, "evil")); err == nil {
			t.Error("a file escaped the destination")
		}
	})
	t.Run("hostile version string", func(t *testing.T) {
		srv := newCFT(t, "../../etc", shellZip(t))
		f := &Fetcher{Dir: t.TempDir(), VersionsURL: srv.URL + "/versions.json", Platform: "linux64"}
		if _, err := f.Fetch(ctx); err == nil {
			t.Error("Fetch accepted a version that is not numeric")
		}
	})
	t.Run("server errors", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		t.Cleanup(srv.Close)
		f := &Fetcher{Dir: t.TempDir(), VersionsURL: srv.URL, Platform: "linux64"}
		if _, err := f.Fetch(ctx); err == nil {
			t.Error("Fetch succeeded against a 404")
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		srv := newCFT(t, "155.0.1.2", shellZip(t))
		ctx, cancel := context.WithCancel(ctx)
		cancel()
		f := &Fetcher{Dir: t.TempDir(), VersionsURL: srv.URL + "/versions.json", Platform: "linux64"}
		if _, err := f.Fetch(ctx); err == nil {
			t.Error("Fetch ignored a cancelled context")
		}
	})
}

func TestVersions(t *testing.T) {
	for v, want := range map[string]bool{"155.0.8059.84": true, "1": true, "": false, ".1": false, "1.": false, "1.a": false, "../1": false, "1/2": false} {
		if got := validVersion(v); got != want {
			t.Errorf("validVersion(%q) = %v, want %v", v, got, want)
		}
	}
	if compareVersions("10.0", "9.9") <= 0 || compareVersions("1.2", "1.2.0") != 0 || compareVersions("", "0.1") >= 0 {
		t.Error("compareVersions misorders")
	}
}
