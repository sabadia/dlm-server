//go:build linux

package platform

import (
	"os"

	"golang.org/x/sys/unix"
)

// preallocate reserves [cur, size) with fallocate(2). Mode 0 also extends
// the file length.
func preallocate(f *os.File, cur, size int64) error {
	return unix.Fallocate(int(f.Fd()), 0, cur, size-cur) //nolint:gosec,wrapcheck // fds fit in int; Preallocate wraps the error
}
