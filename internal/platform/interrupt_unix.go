//go:build !windows

package platform

import (
	"os"
	"syscall"
)

// InterruptProcess asks a child process to stop gracefully (SIGINT). When the
// child leads its own process group (see OwnProcessGroup) the whole group gets
// the signal, so helpers it started stop with it.
func InterruptProcess(p *os.Process) error {
	if err := syscall.Kill(-p.Pid, syscall.SIGINT); err == nil {
		return nil
	}
	return p.Signal(os.Interrupt) //nolint:wrapcheck // the process is gone or about to be
}
