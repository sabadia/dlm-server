package platform

import (
	"path/filepath"
	"testing"
)

func TestUserDataDirIsAbsolute(t *testing.T) {
	dir, err := UserDataDir()
	if err != nil {
		t.Skipf("no data dir in this environment: %v", err)
	}
	if !filepath.IsAbs(dir) {
		t.Fatalf("UserDataDir() = %q, want absolute path", dir)
	}
}

func TestUserDownloadsDirIsAbsolute(t *testing.T) {
	dir, err := UserDownloadsDir()
	if err != nil {
		t.Skipf("no home dir in this environment: %v", err)
	}
	if !filepath.IsAbs(dir) {
		t.Fatalf("UserDownloadsDir() = %q, want absolute path", dir)
	}
}
