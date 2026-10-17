package hls

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
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

func ffmpegOrSkip(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	return path
}

// hasX264 reports whether ffmpeg can encode H.264, the codec real HLS streams
// carry (MPEG-4 part 2 in MPEG-TS is rare and probes poorly).
func hasX264(ff string) bool {
	out, err := exec.Command(ff, "-hide_banner", "-encoders").Output() //nolint:gosec // test helper running the system ffmpeg
	return err == nil && contains(string(out), "libx264")
}

// makeStream has ffmpeg generate a 4 s test stream as HLS in dir. extra are
// additional muxer arguments.
func makeStream(t *testing.T, ff, dir string, extra ...string) {
	t.Helper()
	if !hasX264(ff) {
		t.Skip("this ffmpeg has no libx264 to generate a realistic test stream")
	}
	args := []string{
		"-y", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=160x120:rate=10:duration=4",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=4",
		"-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest",
		"-f", "hls", "-hls_time", "1", "-hls_playlist_type", "vod",
	}
	args = append(args, extra...)
	args = append(args, filepath.Join(dir, "index.m3u8"))
	cmd := exec.Command(ff, args...) //nolint:gosec // test helper running the system ffmpeg
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("this ffmpeg cannot generate the test stream: %v: %s", err, out)
	}
}

// verifyMedia decodes the whole file with ffmpeg; a corrupt merge fails.
func verifyMedia(t *testing.T, ff, path string) {
	t.Helper()
	out, err := exec.Command(ff, "-v", "error", "-i", path, "-f", "null", "-").CombinedOutput() //nolint:gosec // test helper running the system ffmpeg
	if err != nil || len(out) > 0 {
		t.Errorf("ffmpeg cannot decode %s cleanly: %v: %s", path, err, out)
	}
	probe, _ := exec.Command(ff, "-hide_banner", "-i", path).CombinedOutput() //nolint:gosec // test helper; ffmpeg exits 1 without an output file
	for _, want := range []string{"Video:", "Audio:"} {
		if !contains(string(probe), want) {
			t.Errorf("%s has no %s stream:\n%s", path, want, probe)
		}
	}
}

func realRun(t *testing.T, ff, srvDir, name string) string {
	t.Helper()
	srv := httptest.NewServer(http.FileServer(http.Dir(srvDir)))
	t.Cleanup(srv.Close)
	clients := netclient.New(netclient.Options{})
	t.Cleanup(func() { _ = clients.Close() })
	e := New(engine.Env{Clients: clients}, realFF{path: ff})
	dir := t.TempDir()
	v := &env{e: e, dir: dir}
	sp := v.spec(srv.URL+"/index.m3u8", name, nil, nil)
	dl, err := e.NewDownload(sp)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	if err := dl.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return filepath.Join(dir, name)
}

func TestRealFFmpegMPEGTS(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	makeStream(t, ff, dir)
	out := realRun(t, ff, dir, "ts.mp4")
	verifyMedia(t, ff, out)
}

func TestRealFFmpegFragmentedMP4(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	makeStream(t, ff, dir, "-hls_segment_type", "fmp4", "-hls_fmp4_init_filename", "init.mp4")
	out := realRun(t, ff, dir, "fmp4.mp4")
	verifyMedia(t, ff, out)
}

func TestRealFFmpegEncrypted(t *testing.T) {
	ff := ffmpegOrSkip(t)
	dir := t.TempDir()
	key := []byte("0123456789abcdef")
	if err := os.WriteFile(filepath.Join(dir, "enc.key"), key, 0o644); err != nil {
		t.Fatal(err)
	}
	info := filepath.Join(dir, "enc.keyinfo")
	// URI as written in the playlist, key file path, IV.
	if err := os.WriteFile(info, []byte("enc.key\n"+filepath.Join(dir, "enc.key")+"\n0123456789abcdef0123456789abcdef\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	makeStream(t, ff, dir, "-hls_key_info_file", info)
	pl, err := os.ReadFile(filepath.Join(dir, "index.m3u8"))
	if err != nil || !contains(string(pl), "METHOD=AES-128") {
		t.Skipf("ffmpeg did not encrypt the stream: %v", err)
	}
	out := realRun(t, ff, dir, "enc.mp4")
	verifyMedia(t, ff, out)
}
