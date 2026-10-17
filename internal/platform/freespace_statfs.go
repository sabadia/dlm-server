//go:build linux || darwin || freebsd

package platform

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func freeSpace(dir string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, fmt.Errorf("platform: statfs %q: %w", dir, err)
	}
	return toUint64(st.Bavail) * toUint64(st.Bsize), nil
}

// toUint64 converts the Statfs_t field types, which differ between
// operating systems and architectures, without tripping "unnecessary
// conversion" lints where they already are uint64. Negative values
// (impossible in practice) become 0.
func toUint64[T ~int32 | ~uint32 | ~int64 | ~uint64](v T) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v)
}
