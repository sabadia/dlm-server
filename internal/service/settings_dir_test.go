package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sabadia/dlm-server/internal/settings"
)

func TestCheckDownloadDir(t *testing.T) {
	root := t.TempDir()
	good := settings.Settings{DownloadDir: settings.Some(filepath.Join(root, "new", "dir"))}
	if err := checkDownloadDir(good); err != nil {
		t.Fatalf("a creatable directory was refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "new", "dir")); err != nil {
		t.Errorf("the directory was not created: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(root, "new", "dir")); len(entries) != 0 {
		t.Errorf("the write check left %d files behind", len(entries))
	}
	if err := checkDownloadDir(settings.Settings{}); err != nil {
		t.Errorf("unset directory: %v", err)
	}
	// A file where a directory is wanted cannot be used.
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkDownloadDir(settings.Settings{DownloadDir: settings.Some(filepath.Join(file, "sub"))}); err == nil {
		t.Error("a directory below a file was accepted")
	}
}
