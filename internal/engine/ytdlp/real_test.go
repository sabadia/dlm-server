package ytdlp

import (
	"bytes"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/ytdlp"
)

// realYtdlp returns a real yt-dlp (DLM_TEST_YTDLP, else on PATH) or skips.
func realYtdlp(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("DLM_TEST_YTDLP"); p != "" {
		return p
	}
	p, err := exec.LookPath("yt-dlp")
	if err != nil {
		t.Skip("yt-dlp is not installed (set DLM_TEST_YTDLP to its path)")
	}
	return p
}

// TestRealYtdlp resolves and downloads a direct media link served locally
// through the generic extractor of a real yt-dlp.
func TestRealYtdlp(t *testing.T) {
	bin := realYtdlp(t)
	data := make([]byte, 120_000)
	_, _ = rand.Read(data)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/clips/sample.mp4" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		http.ServeContent(w, r, "sample.mp4", time.Time{}, bytes.NewReader(data))
	}))
	t.Cleanup(srv.Close)

	loc := ytdlp.NewLocator(ytdlp.Options{Path: bin})
	e := New(engine.Env{}, loc, nil)
	s := testSettings()
	source := src(srv.URL + "/clips/sample.mp4")
	p, err := e.Resolve(t.Context(), engine.Request{Source: source, Settings: s})
	if err != nil {
		t.Fatal(err)
	}
	if p.Suggested != "sample.mp4" {
		t.Errorf("suggested = %q", p.Suggested)
	}
	dir := t.TempDir()
	sp := engine.Spec{TaskID: "t", Source: p.Source, Resource: &p.Resource, Path: filepath.Join(dir, p.Suggested), Settings: s}
	dl, err := e.NewDownload(sp)
	if err != nil {
		t.Fatal(err)
	}
	if err := dl.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "sample.mp4"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("content differs: %v (%d bytes)", err, len(got))
	}
	if _, err := os.Stat(filepath.Join(dir, "sample.mp4"+WorkSuffix)); !os.IsNotExist(err) {
		t.Errorf("work directory left behind: %v", err)
	}
	// A link that is not there is an error with a kind.
	_, err = e.Resolve(t.Context(), engine.Request{Source: src(srv.URL + "/missing.mp4"), Settings: s})
	if err == nil {
		t.Fatal("a missing page must fail")
	}
	t.Logf("missing page: %v (kind %v)", err, engine.Classify(err))
}
