// Package auth manages the API bearer token shared by the daemon and its
// local clients (CLI, desktop app, browser-extension native host).
//
// The token is a random secret stored in a file only the owner can read. It
// must never be logged or returned by the API.
package auth

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// tokenBytes is the amount of entropy in a generated token.
const tokenBytes = 32

// ErrEmptyToken is returned when the token file exists but holds no token.
var ErrEmptyToken = errors.New("auth: token file is empty")

// LoadOrCreateToken returns the bearer token stored at path. If the file does
// not exist, a new token (32 random bytes, base64url without padding) is
// generated and written atomically with mode 0600; missing parent directories
// are created with mode 0700.
//
// On Windows the Unix permission bits are ignored by the OS; the file then
// inherits the ACL of its directory, which for the per-user config directory
// is owner-only by default.
func LoadOrCreateToken(path string) (string, error) {
	token, err := ReadToken(path)
	if errors.Is(err, fs.ErrNotExist) {
		return createToken(path)
	}
	return token, err
}

// ReadToken returns the bearer token stored at path without ever creating
// it; clients use it so that only the daemon generates the token. A missing
// file yields an error matching fs.ErrNotExist, an empty one ErrEmptyToken.
func ReadToken(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path comes from the boot configuration, not from a request
	if err != nil {
		return "", fmt.Errorf("auth: read token file %q: %w", path, err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("%w: %q", ErrEmptyToken, path)
	}
	return token, nil
}

func createToken(path string) (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: generate token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(buf)
	if err := writeFileAtomic(path, []byte(token+"\n")); err != nil {
		return "", err
	}
	return token, nil
}

// writeFileAtomic writes data to a temporary file in the target directory
// and renames it into place, so readers never observe a partial token.
func writeFileAtomic(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("auth: create token dir %q: %w", dir, err)
	}
	// CreateTemp opens the file with mode 0600.
	tmp, err := os.CreateTemp(dir, ".token-*.tmp")
	if err != nil {
		return fmt.Errorf("auth: create temp token file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			// Best-effort cleanup; the original error is what matters.
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		_ = tmp.Close() // already failing; the chmod error is reported
		return fmt.Errorf("auth: chmod temp token file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close() // already failing; the write error is reported
		return fmt.Errorf("auth: write temp token file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close() // already failing; the sync error is reported
		return fmt.Errorf("auth: sync temp token file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("auth: close temp token file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("auth: install token file %q: %w", path, err)
	}
	return nil
}
