package instance

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// File names inside the configuration directory.
const (
	// RuntimeFile is the published description of the running daemon.
	RuntimeFile = "runtime.json"
	// LockFile is locked for as long as a daemon runs.
	LockFile = "dlm.lock"
)

// Info describes a running daemon.
type Info struct {
	PID int `json:"pid"`
	// Address is the host:port the daemon listens on (the real port, even
	// when it was started with port 0).
	Address string `json:"address"`
	// URL is the API base URL a local client uses.
	URL        string    `json:"url"`
	Version    string    `json:"version"`
	APIVersion int       `json:"apiVersion"`
	StartedAt  time.Time `json:"startedAt"`
	// TLSFingerprint is "sha256:<hex>" of the leaf certificate, empty when
	// the daemon serves plain HTTP.
	TLSFingerprint string `json:"tlsFingerprint,omitempty"`
}

// ErrNotRunning is returned by Discover when no daemon holds the lock of the
// directory.
var ErrNotRunning = errors.New("instance: no daemon is running")

// RuntimePath returns the path of the runtime file in dir.
func RuntimePath(dir string) string { return filepath.Join(dir, RuntimeFile) }

// Write publishes info atomically: readers see the previous file or the new
// one, never a partial write.
func Write(dir string, info Info) error {
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return fmt.Errorf("instance: encode runtime file: %w", err)
	}
	tmp, err := os.CreateTemp(dir, RuntimeFile+".*.tmp")
	if err != nil {
		return fmt.Errorf("instance: create runtime file: %w", err)
	}
	name := tmp.Name()
	cleanup := func(cause error) error {
		_ = tmp.Close()
		_ = os.Remove(name)
		return cause
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		return cleanup(fmt.Errorf("instance: write runtime file: %w", err))
	}
	if err := tmp.Sync(); err != nil {
		return cleanup(fmt.Errorf("instance: sync runtime file: %w", err))
	}
	if err := tmp.Close(); err != nil {
		return cleanup(fmt.Errorf("instance: close runtime file: %w", err))
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return cleanup(fmt.Errorf("instance: chmod runtime file: %w", err))
	}
	if err := os.Rename(name, RuntimePath(dir)); err != nil {
		return cleanup(fmt.Errorf("instance: publish runtime file: %w", err))
	}
	return nil
}

// Read returns the published description of the daemon of dir. It does not
// check that the daemon is still alive; use Discover for that.
func Read(dir string) (Info, error) {
	data, err := os.ReadFile(RuntimePath(dir))
	if err != nil {
		return Info{}, fmt.Errorf("instance: read runtime file: %w", err)
	}
	var info Info
	if err := json.Unmarshal(data, &info); err != nil {
		return Info{}, fmt.Errorf("instance: decode runtime file: %w", err)
	}
	return info, nil
}

// Remove deletes the runtime file if it still describes pid (a newer daemon
// may already have replaced it).
func Remove(dir string, pid int) error {
	info, err := Read(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if info.PID != pid {
		return nil
	}
	if err := os.Remove(RuntimePath(dir)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("instance: remove runtime file: %w", err)
	}
	return nil
}

// Discover returns the description of the daemon running for dir. It is
// ErrNotRunning when no process holds the directory's lock, which also covers
// a runtime file left behind by a crashed daemon.
func Discover(dir string) (Info, error) {
	held, err := Held(dir)
	if err != nil {
		return Info{}, err
	}
	if !held {
		return Info{}, ErrNotRunning
	}
	info, err := Read(dir)
	if err != nil {
		// The daemon holds the lock but has not published yet (or is
		// shutting down).
		return Info{}, fmt.Errorf("%w: %w", ErrNotRunning, err)
	}
	return info, nil
}

// Fingerprint returns "sha256:<hex>" of the leaf certificate in certFile
// (PEM, together with its key in keyFile).
func Fingerprint(certFile, keyFile string) (string, error) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return "", fmt.Errorf("instance: load certificate: %w", err)
	}
	if len(pair.Certificate) == 0 {
		return "", errors.New("instance: certificate file has no certificate")
	}
	sum := sha256.Sum256(pair.Certificate[0])
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
