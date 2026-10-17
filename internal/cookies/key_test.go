package cookies

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/zalando/go-keyring"
)

type fakeKeyring struct {
	secret  string
	getErr  error // returned instead of keyring.ErrNotFound when no secret is set
	setErr  error
	sets    int
	hasData bool
}

func (f *fakeKeyring) Get(_, _ string) (string, error) {
	if f.hasData {
		return f.secret, nil
	}
	if f.getErr != nil {
		return "", f.getErr
	}
	return "", keyring.ErrNotFound
}

func (f *fakeKeyring) Set(_, _, secret string) error {
	f.sets++
	if f.setErr != nil {
		return f.setErr
	}
	f.secret, f.hasData = secret, true
	return nil
}

func TestLoadOrCreateKeyKeyring(t *testing.T) {
	dir := t.TempDir()
	kr := &fakeKeyring{}
	k1, err := LoadOrCreateKey(KeyOptions{Dir: dir, Keyring: kr})
	if err != nil {
		t.Fatal(err)
	}
	if len(k1) != 32 {
		t.Fatalf("key length = %d, want 32", len(k1))
	}
	if kr.sets != 1 {
		t.Fatalf("keyring sets = %d, want 1", kr.sets)
	}
	if _, err := os.Stat(filepath.Join(dir, KeyFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("key file exists although the keyring holds the key: %v", err)
	}
	k2, err := LoadOrCreateKey(KeyOptions{Dir: dir, Keyring: kr})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(k1, k2) {
		t.Error("second load returned a different key")
	}
	if kr.sets != 1 {
		t.Errorf("keyring sets = %d after reload, want 1", kr.sets)
	}
}

func TestLoadOrCreateKeyFileFallback(t *testing.T) {
	tests := []struct {
		name string
		opts func(dir string) KeyOptions
	}{
		{"keyring unavailable", func(dir string) KeyOptions {
			return KeyOptions{Dir: dir, Keyring: &fakeKeyring{getErr: errors.New("no dbus")}}
		}},
		{"keyring rejects set", func(dir string) KeyOptions {
			return KeyOptions{Dir: dir, Keyring: &fakeKeyring{setErr: errors.New("locked")}}
		}},
		{"file only", func(dir string) KeyOptions {
			return KeyOptions{Dir: dir, Keyring: &fakeKeyring{}, FileOnly: true}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "config") // created on demand
			opts := tt.opts(dir)
			k1, err := LoadOrCreateKey(opts)
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(filepath.Join(dir, KeyFileName))
			if err != nil {
				t.Fatal(err)
			}
			if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
				t.Errorf("key file mode = %v, want 0600", info.Mode().Perm())
			}
			k2, err := LoadOrCreateKey(opts)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(k1, k2) {
				t.Error("second load returned a different key")
			}
		})
	}
}

func TestLoadOrCreateKeyFileOnlyIgnoresKeyring(t *testing.T) {
	kr := &fakeKeyring{secret: "ignored", hasData: true}
	if _, err := LoadOrCreateKey(KeyOptions{Dir: t.TempDir(), Keyring: kr, FileOnly: true}); err != nil {
		t.Fatal(err)
	}
	if kr.sets != 0 {
		t.Errorf("FileOnly touched the keyring (%d sets)", kr.sets)
	}
}

func TestLoadOrCreateKeyCorrupt(t *testing.T) {
	t.Run("file", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, KeyFileName), []byte("not base64!"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadOrCreateKey(KeyOptions{Dir: dir, FileOnly: true}); err == nil {
			t.Fatal("want an error for a corrupt key file")
		}
	})
	t.Run("keyring", func(t *testing.T) {
		kr := &fakeKeyring{secret: "AAAA", hasData: true} // valid base64, 3 bytes
		if _, err := LoadOrCreateKey(KeyOptions{Dir: t.TempDir(), Keyring: kr}); err == nil {
			t.Fatal("want an error for a short keyring key")
		}
	})
}

func TestLoadOrCreateKeyRequiresDir(t *testing.T) {
	if _, err := LoadOrCreateKey(KeyOptions{}); err == nil {
		t.Fatal("want an error without a directory")
	}
}
