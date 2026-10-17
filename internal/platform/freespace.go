package platform

import (
	"errors"
	"os"
)

// FreeSpace returns the number of bytes available to the current
// (unprivileged) user on the volume holding dir, which must exist. On
// systems where this cannot be determined it returns an error wrapping
// errors.ErrUnsupported; callers treat that as "no information".
func FreeSpace(dir string) (uint64, error) {
	return freeSpace(dir)
}

// Preallocate reserves size bytes for f and sets its length to size, so a
// download fails early when the volume is full and fragmentation is
// reduced. It uses fallocate(2) on Linux and F_PREALLOCATE on macOS, and
// extends the file with Truncate (a sparse file where supported) on other
// systems or when real reservation is unsupported by the file system. A
// disk-full condition is returned (see IsDiskFull); other reservation
// failures fall back to Truncate. A file that is already at least size bytes
// long is left unchanged.
func Preallocate(f *os.File, size int64) error {
	if size <= 0 {
		return nil
	}
	fi, err := f.Stat()
	if err != nil {
		return pathErr("stat", f, err)
	}
	if fi.Size() >= size {
		return nil
	}
	if err := preallocate(f, fi.Size(), size); err != nil && IsDiskFull(err) {
		return pathErr("preallocate", f, err)
	}
	// Any other reservation failure is ignored: reservation is an
	// optimisation and a sparse file is still correct.
	if err := f.Truncate(size); err != nil {
		return pathErr("truncate", f, err)
	}
	return nil
}

// pathErr returns err unchanged when it already is an *os.PathError, else
// wraps it in one naming the operation and file.
func pathErr(op string, f *os.File, err error) error {
	if _, ok := errors.AsType[*os.PathError](err); ok {
		return err
	}
	return &os.PathError{Op: op, Path: f.Name(), Err: err}
}
