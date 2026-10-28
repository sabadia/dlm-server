package instance

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrLocked is returned by Acquire when another daemon already runs for the
// directory. The error text names its pid and address when known.
var ErrLocked = errors.New("instance: another dlm daemon is already running for this configuration")

// Lock is the exclusive claim of a configuration directory by one daemon. The
// operating system releases it when the process dies, so a crash never leaves
// the directory locked.
type Lock struct {
	file *os.File
}

// Acquire claims dir for the calling process, or fails with ErrLocked.
func Acquire(dir string) (*Lock, error) {
	f, err := os.OpenFile(filepath.Join(dir, LockFile), os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // the lock file lives in the daemon's own config directory
	if err != nil {
		return nil, fmt.Errorf("instance: open lock file: %w", err)
	}
	ok, err := tryLock(f)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("instance: lock: %w", err)
	}
	if !ok {
		_ = f.Close()
		if info, rerr := Read(dir); rerr == nil {
			return nil, fmt.Errorf("%w (pid %d, %s)", ErrLocked, info.PID, info.Address)
		}
		return nil, ErrLocked
	}
	return &Lock{file: f}, nil
}

// Release gives the directory up; it is idempotent.
func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	f := l.file
	l.file = nil
	unlock(f)
	if err := f.Close(); err != nil {
		return fmt.Errorf("instance: close lock file: %w", err)
	}
	return nil
}

// Held reports whether some process holds the lock of dir.
func Held(dir string) (bool, error) {
	f, err := os.OpenFile(filepath.Join(dir, LockFile), os.O_RDWR, 0o600) //nolint:gosec // the lock file lives in the daemon's own config directory
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("instance: open lock file: %w", err)
	}
	defer func() { _ = f.Close() }()
	ok, err := tryLock(f)
	if err != nil {
		return false, fmt.Errorf("instance: probe lock: %w", err)
	}
	if ok {
		unlock(f)
	}
	return !ok, nil
}
