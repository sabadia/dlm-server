//go:build !windows

package httpx

import "syscall"

func diskFullErrno() error { return syscall.ENOSPC }
