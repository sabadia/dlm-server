package auth

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLoadOrCreateTokenCreatesAndReuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config", "token")

	first, err := LoadOrCreateToken(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(first)
	if err != nil {
		t.Fatalf("token is not base64url without padding: %v", err)
	}
	if len(raw) != tokenBytes {
		t.Fatalf("token has %d random bytes, want %d", len(raw), tokenBytes)
	}

	second, err := LoadOrCreateToken(path)
	if err != nil {
		t.Fatalf("reuse: %v", err)
	}
	if first != second {
		t.Fatal("second call generated a new token instead of reusing the stored one")
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("token dir has %d entries, want only the token file (temp file leaked?)", len(entries))
	}
}

func TestLoadOrCreateTokenPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows ignores Unix permission bits")
	}
	dir := filepath.Join(t.TempDir(), "config")
	path := filepath.Join(dir, "token")
	if _, err := LoadOrCreateToken(path); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		path string
		want os.FileMode
	}{
		{"file", path, 0o600},
		{"dir", dir, 0o700},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fi, err := os.Stat(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			if got := fi.Mode().Perm(); got != tc.want {
				t.Fatalf("mode = %o, want %o", got, tc.want)
			}
		})
	}
}

func TestLoadOrCreateTokenReadsExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("  my-secret\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadOrCreateToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != "my-secret" {
		t.Fatalf("token = %q, want trimmed %q", got, "my-secret")
	}
}

func TestLoadOrCreateTokenRejectsEmptyFile(t *testing.T) {
	for _, content := range []string{"", " \n\t"} {
		path := filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadOrCreateToken(path); !errors.Is(err, ErrEmptyToken) {
			t.Fatalf("content %q: err = %v, want ErrEmptyToken", content, err)
		}
	}
}

func TestReadTokenNeverCreates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if _, err := ReadToken(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadToken(missing) error = %v, want fs.ErrNotExist", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadToken created the token file (stat err = %v)", err)
	}
	want, err := LoadOrCreateToken(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadToken(path)
	if err != nil || got != want {
		t.Fatalf("ReadToken = %q, %v; want %q", got, err, want)
	}
}
