//go:build windows

package platform

import (
	"fmt"
	"os"
	"os/exec"
)

// Reexec starts a fresh copy of the running process (same arguments and
// environment, same console) and returns; the caller then exits. Windows
// cannot replace a process image, so the pid changes.
func Reexec() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("platform: locate the executable: %w", err)
	}
	cmd := exec.Command(exe, os.Args[1:]...) //nolint:gosec // re-running our own executable with our own arguments
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("platform: restart %s: %w", exe, err)
	}
	return nil
}
