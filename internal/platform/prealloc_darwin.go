//go:build darwin

package platform

import (
	"os"

	"golang.org/x/sys/unix"
)

// preallocate reserves size-cur bytes past the physical end of file with
// F_PREALLOCATE, trying a contiguous allocation first. It does not change
// the file length; Preallocate truncates afterwards.
func preallocate(f *os.File, cur, size int64) error {
	st := unix.Fstore_t{
		Flags:   unix.F_ALLOCATECONTIG | unix.F_ALLOCATEALL,
		Posmode: unix.F_PEOFPOSMODE,
		Length:  size - cur,
	}
	if err := unix.FcntlFstore(f.Fd(), unix.F_PREALLOCATE, &st); err == nil {
		return nil
	}
	st.Flags = unix.F_ALLOCATEALL
	return unix.FcntlFstore(f.Fd(), unix.F_PREALLOCATE, &st) //nolint:wrapcheck // Preallocate wraps the error
}
