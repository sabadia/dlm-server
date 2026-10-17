package dash

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sabadia/dlm-server/internal/engine"
	"github.com/sabadia/dlm-server/internal/ffmpeg"
	"github.com/sabadia/dlm-server/internal/netclient"
)

type realFF struct{ path string }

func (r realFF) Locate(context.Context) (ffmpeg.Binary, error) {
	return ffmpeg.Binary{Path: r.path, Version: "system", Source: "path"}, nil
}

// TestRealFFmpegDASH has ffmpeg make a DASH stream, downloads it through the
// engine, and checks that ffmpeg can decode the result.
func TestRealFFmpegDASH(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	if out, err := exec.Command(ff, "-hide_banner", "-encoders").Output(); err != nil || !strings.Contains(string(out), "libx264") { //nolint:gosec // test helper running the system ffmpeg
		t.Skip("this ffmpeg has no libx264")
	}
	src := t.TempDir()
	args := []string{
		"-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=160x120:rate=10:duration=4",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=4",
		"-map", "0:v", "-map", "1:a", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest",
		"-f", "dash", "-seg_duration", "1", "-use_template", "1", "-use_timeline", "1",
		filepath.Join(src, "manifest.mpd"),
	}
	if out, err := exec.Command(ff, args...).CombinedOutput(); err != nil { //nolint:gosec // test helper running the system ffmpeg
		t.Skipf("this ffmpeg cannot generate DASH: %v: %s", err, out)
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(src)))
	t.Cleanup(srv.Close)

	clients := netclient.New(netclient.Options{})
	t.Cleanup(func() { _ = clients.Close() })
	e := New(engine.Env{Clients: clients}, realFF{ff})
	dir := t.TempDir()
	dl, err := e.NewDownload(spec(dir, srv.URL+"/manifest.mpd", "out.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := dl.Run(ctx); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := filepath.Join(dir, "out.mp4")
	if st, err := os.Stat(out); err != nil || st.Size() == 0 {
		t.Fatalf("output: %v", err)
	}
	if b, err := exec.Command(ff, "-v", "error", "-i", out, "-f", "null", "-").CombinedOutput(); err != nil || len(b) > 0 { //nolint:gosec // test helper running the system ffmpeg
		t.Fatalf("ffmpeg cannot decode the result: %v: %s", err, b)
	}
}
