//go:build !windows

package platform

import (
	"errors"
	"syscall"
)

func isDiskFull(err error) bool {
	return errors.Is(err, syscall.ENOSPC)
}

// diskFullErrnos lists the errno values IsDiskFull recognises (for tests).
func diskFullErrnos() []syscall.Errno { return []syscall.Errno{syscall.ENOSPC} }
