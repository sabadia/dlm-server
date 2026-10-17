//go:build windows

package platform

import (
	"errors"
	"syscall"
)

// Win32 error codes not exported by package syscall.
const (
	errorHandleDiskFull syscall.Errno = 39  // ERROR_HANDLE_DISK_FULL
	errorDiskFull       syscall.Errno = 112 // ERROR_DISK_FULL
)

func isDiskFull(err error) bool {
	return errors.Is(err, errorDiskFull) || errors.Is(err, errorHandleDiskFull) || errors.Is(err, syscall.ENOSPC)
}

// diskFullErrnos lists the errno values IsDiskFull recognises (for tests).
func diskFullErrnos() []syscall.Errno {
	return []syscall.Errno{errorDiskFull, errorHandleDiskFull, syscall.ENOSPC}
}
