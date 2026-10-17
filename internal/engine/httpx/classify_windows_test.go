//go:build windows

package httpx

import "syscall"

// diskFullErrno returns ERROR_DISK_FULL (112).
func diskFullErrno() error { return syscall.Errno(112) }
