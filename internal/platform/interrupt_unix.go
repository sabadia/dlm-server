//go:build !windows

package platform

import "os"

// InterruptProcess asks a child process to stop gracefully (SIGINT).
func InterruptProcess(p *os.Process) error {
	return p.Signal(os.Interrupt) //nolint:wrapcheck // the process is gone or about to be
}
