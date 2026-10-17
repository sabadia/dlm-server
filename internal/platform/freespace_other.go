//go:build !(linux || darwin || freebsd || windows)

package platform

import (
	"errors"
	"fmt"
)

func freeSpace(dir string) (uint64, error) {
	return 0, fmt.Errorf("platform: free space %q: %w", dir, errors.ErrUnsupported)
}
