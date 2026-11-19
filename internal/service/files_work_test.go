package service

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveWorkClearsFilesAndDirectories(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "video.mp4")
	// yt-dlp's work directory, an HLS segment directory, a plain .part file.
	for _, d := range []string{final + ".part", final + ".hls"} {
		if err := os.MkdirAll(filepath.Join(d, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "sub", "seg"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	other := filepath.Join(dir, "other.mp4.part")
	if err := os.WriteFile(other, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeWork(final); err != nil {
		t.Fatalf("removeWork: %v", err)
	}
	for _, p := range []string{final + ".part", final + ".hls"} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s still exists (err=%v)", p, err)
		}
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("another task's file was touched: %v", err)
	}
	if err := removeWork(final); err != nil {
		t.Errorf("second removeWork on missing paths: %v", err)
	}
}
