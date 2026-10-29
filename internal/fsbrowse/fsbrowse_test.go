package fsbrowse

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/sabadia/dlm-server/internal/domain"
)

func mkdirs(t *testing.T, base string, rel ...string) {
	t.Helper()
	for _, r := range rel {
		if err := os.MkdirAll(filepath.Join(base, r), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func code(err error) string {
	c, _, _ := domain.Describe(err)
	return c
}

func TestDisabledWithoutRoots(t *testing.T) {
	b, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	if b.Enabled() {
		t.Error("no roots, yet enabled")
	}
	if _, err := b.List(""); code(err) != "fs.disabled" || !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("List: %v", err)
	}
	if _, err := b.Free(t.TempDir()); code(err) != "fs.disabled" {
		t.Errorf("Free: %v", err)
	}
}

func TestNewRejectsBadRoots(t *testing.T) {
	if _, err := New([]string{"relative/dir"}); err == nil {
		t.Error("relative root accepted")
	}
	if _, err := New([]string{filepath.Join(t.TempDir(), "missing")}); err == nil {
		t.Error("missing root accepted")
	}
}

func TestListWithinRoots(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "b/inner", "a", "C")
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := New([]string{root})
	if err != nil {
		t.Fatal(err)
	}

	roots, err := b.List("")
	if err != nil || len(roots.Entries) != 1 || roots.Entries[0].Path != filepath.Clean(root) {
		t.Fatalf("roots = %+v, %v", roots, err)
	}

	l, err := b.List(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range l.Entries {
		names = append(names, e.Name)
	}
	if got := names; len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "C" {
		t.Errorf("entries = %v, want directories only, sorted without regard to case", got)
	}
	if l.Parent != "" {
		t.Errorf("a root has parent %q", l.Parent)
	}

	sub, err := b.List(filepath.Join(root, "b"))
	if err != nil || len(sub.Entries) != 1 || sub.Entries[0].Name != "inner" || sub.Parent != filepath.Clean(root) {
		t.Errorf("sub = %+v, %v", sub, err)
	}
}

func TestPathsOutsideTheRootsAreRefused(t *testing.T) {
	root, other := t.TempDir(), t.TempDir()
	mkdirs(t, root, "in")
	b, err := New([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"another directory":   other,
		"dotdot out of root":  filepath.Join(root, "in", "..", "..", filepath.Base(other)),
		"the parent of root":  filepath.Dir(root),
		"a sibling by prefix": root + "-evil",
	} {
		if _, err := b.List(path); code(err) != "fs.outside_roots" {
			t.Errorf("%s: List err = %v", name, err)
		}
	}
	if _, err := b.List("relative"); code(err) != "fs.not_absolute" {
		t.Errorf("relative: %v", err)
	}
	if _, err := b.List(filepath.Join(root, "nope")); code(err) != "fs.path_not_found" {
		t.Errorf("missing: %v", err)
	}
}

func TestSymlinksCannotLeaveTheRoots(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symbolic links needs privileges on Windows")
	}
	root, outside := t.TempDir(), t.TempDir()
	mkdirs(t, root, "real")
	mkdirs(t, outside, "secret")
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "inside")); err != nil {
		t.Fatal(err)
	}
	b, err := New([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	l, err := b.List(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range l.Entries {
		names = append(names, e.Name)
	}
	if len(names) != 2 || names[0] != "inside" || names[1] != "real" {
		t.Errorf("entries = %v: the link out of the root must not be listed, the link inside may", names)
	}
	if _, err := b.List(filepath.Join(root, "escape")); code(err) != "fs.outside_roots" {
		t.Errorf("walking through the link: %v", err)
	}
	if _, err := b.Free(filepath.Join(root, "escape", "secret")); code(err) != "fs.outside_roots" {
		t.Errorf("free space through the link: %v", err)
	}
}

func TestFreeAndNotADirectory(t *testing.T) {
	root := t.TempDir()
	f := filepath.Join(root, "f")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := New([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := b.Free(root); err != nil || n == 0 {
		t.Errorf("Free = %d, %v", n, err)
	}
	if _, err := b.Free(f); code(err) != "fs.not_directory" {
		t.Errorf("Free of a file: %v", err)
	}
	if _, err := b.List(f); code(err) != "fs.not_directory" {
		t.Errorf("List of a file: %v", err)
	}
}
