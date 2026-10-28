//go:build !windows

package platform

import (
	"fmt"
	"os"
	"syscall"
)

// Reexec replaces the running process with a fresh copy of itself (same
// arguments and environment). The pid stays the same, so a supervisor
// watching it sees a restart without a new process. It only returns on error.
func Reexec() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("platform: locate the executable: %w", err)
	}
	if err := syscall.Exec(exe, os.Args, os.Environ()); err != nil { //nolint:gosec // re-running our own executable with our own arguments
		return fmt.Errorf("platform: re-exec %s: %w", exe, err)
	}
	return nil
}
