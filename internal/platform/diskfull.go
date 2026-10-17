package platform

// IsDiskFull reports whether err (or any error it wraps) means the
// destination volume has no free space left: ENOSPC on Unix-like systems,
// ERROR_DISK_FULL / ERROR_HANDLE_DISK_FULL (and ENOSPC) on Windows.
func IsDiskFull(err error) bool {
	if err == nil {
		return false
	}
	return isDiskFull(err)
}
