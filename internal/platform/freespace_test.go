package platform

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFreeSpace(t *testing.T) {
	n, err := FreeSpace(t.TempDir())
	if errors.Is(err, errors.ErrUnsupported) {
		t.Skip("free space unsupported on this OS")
	}
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Error("FreeSpace = 0 for the temp dir")
	}
	if _, err := FreeSpace(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("FreeSpace of a missing dir succeeded")
	}
}

func TestPreallocate(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "f.part"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	const size = 1 << 20
	if err := Preallocate(f, size); err != nil {
		t.Fatal(err)
	}
	sizeOf := func() int64 {
		t.Helper()
		fi, err := f.Stat()
		if err != nil {
			t.Fatal(err)
		}
		return fi.Size()
	}
	if got := sizeOf(); got != size {
		t.Errorf("size = %d, want %d", got, size)
	}
	// A file already long enough is left unchanged; size <= 0 is a no-op.
	if err := Preallocate(f, 10); err != nil {
		t.Fatal(err)
	}
	if err := Preallocate(f, 0); err != nil {
		t.Fatal(err)
	}
	if got := sizeOf(); got != size {
		t.Errorf("size changed to %d", got)
	}
}
