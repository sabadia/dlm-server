package cookies

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/zalando/go-keyring"
)

// Names of the OS keyring entry and of the fallback key file.
const (
	KeyringService = "dlm-server"
	KeyringUser    = "cookie-encryption-key"
	KeyFileName    = "cookies.key"

	keySize = 32 // AES-256
)

// Keyring is the part of an OS keyring used to hold the encryption key.
// Get must return an error matching keyring.ErrNotFound (errors.Is) when
// there is no entry; any other error means the keyring is unavailable.
type Keyring interface {
	Get(service, user string) (string, error)
	Set(service, user, secret string) error
}

// SystemKeyring is the OS keyring (Keychain, Secret Service, Credential
// Manager) through github.com/zalando/go-keyring.
type SystemKeyring struct{}

// Get implements Keyring.
func (SystemKeyring) Get(service, user string) (string, error) {
	return keyring.Get(service, user) //nolint:wrapcheck // callers match keyring.ErrNotFound
}

// Set implements Keyring.
func (SystemKeyring) Set(service, user, secret string) error {
	return keyring.Set(service, user, secret) //nolint:wrapcheck // callers only log the failure
}

// KeyOptions configures LoadOrCreateKey.
type KeyOptions struct {
	// Dir holds the fallback key file (KeyFileName); it is required and is
	// created (mode 0700) when missing.
	Dir string
	// Keyring defaults to SystemKeyring.
	Keyring Keyring
	// FileOnly skips the keyring and keeps the key in the file only, so a
	// portable installation keeps all of its state below one directory.
	FileOnly bool
	// Logger defaults to a discarding logger.
	Logger *slog.Logger
}

// LoadOrCreateKey returns the 32-byte cookie encryption key. The OS keyring
// is tried first; when it is unavailable the key lives in the file
// Dir/KeyFileName (mode 0600). A missing key is generated and stored in the
// keyring, or in the file if the keyring cannot hold it. A key that exists
// but is corrupt is an error: replacing it would silently orphan every stored
// cookie.
func LoadOrCreateKey(opts KeyOptions) ([]byte, error) {
	if opts.Dir == "" {
		return nil, errors.New("cookies: key directory is required")
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	kr := opts.Keyring
	if kr == nil {
		kr = SystemKeyring{}
	}

	keyringUsable := !opts.FileOnly
	if keyringUsable {
		secret, err := kr.Get(KeyringService, KeyringUser)
		switch {
		case err == nil:
			key, derr := decodeKey(secret)
			if derr != nil {
				return nil, fmt.Errorf("cookies: keyring entry %s/%s: %w", KeyringService, KeyringUser, derr)
			}
			return key, nil
		case errors.Is(err, keyring.ErrNotFound):
		default:
			log.Warn("cookies: OS keyring unavailable, using the key file", "error", err)
			keyringUsable = false
		}
	}

	path := filepath.Join(opts.Dir, KeyFileName)
	key, err := readKeyFile(path)
	switch {
	case err == nil:
		return key, nil
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}

	key, err = newKey()
	if err != nil {
		return nil, err
	}
	encoded := base64.StdEncoding.EncodeToString(key)
	if keyringUsable {
		err := kr.Set(KeyringService, KeyringUser, encoded)
		if err == nil {
			log.Info("cookies: created encryption key in the OS keyring")
			return key, nil
		}
		log.Warn("cookies: cannot store the key in the OS keyring, using the key file", "error", err)
	}
	return writeKeyFile(path, encoded, key)
}

func newKey() ([]byte, error) {
	key := make([]byte, keySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("cookies: generate key: %w", err)
	}
	return key, nil
}

func decodeKey(s string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("not valid base64: %w", err)
	}
	if len(key) != keySize {
		return nil, fmt.Errorf("key is %d bytes, want %d", len(key), keySize)
	}
	return key, nil
}

func readKeyFile(path string) ([]byte, error) {
	data, err := os.ReadFile(path) //nolint:gosec // fixed file name inside the config directory
	if err != nil {
		return nil, fmt.Errorf("cookies: read key file: %w", err)
	}
	key, err := decodeKey(string(data))
	if err != nil {
		return nil, fmt.Errorf("cookies: key file %s: %w", path, err)
	}
	return key, nil
}

// writeKeyFile creates the key file exclusively. If another process created
// it first, that key wins so both use the same one.
func writeKeyFile(path, encoded string, key []byte) ([]byte, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("cookies: create key directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // fixed file name inside the config directory
	if errors.Is(err, fs.ErrExist) {
		return readKeyFile(path)
	}
	if err != nil {
		return nil, fmt.Errorf("cookies: create key file: %w", err)
	}
	if _, err := f.WriteString(encoded + "\n"); err != nil {
		return nil, errors.Join(fmt.Errorf("cookies: write key file: %w", err), f.Close(), os.Remove(path))
	}
	if err := f.Close(); err != nil {
		return nil, errors.Join(fmt.Errorf("cookies: write key file: %w", err), os.Remove(path))
	}
	return key, nil
}
