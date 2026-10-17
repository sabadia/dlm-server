//go:build !linux && !darwin

package platform

import "os"

// preallocate has no reservation primitive here; Preallocate extends the
// file with Truncate (sparse where the file system supports it).
func preallocate(_ *os.File, _, _ int64) error { return nil }
