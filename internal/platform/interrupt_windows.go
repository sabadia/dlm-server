//go:build windows

package platform

import "os"

// InterruptProcess stops a child process: Windows has no interrupt signal for
// one, so it is killed.
func InterruptProcess(p *os.Process) error {
	return p.Kill() //nolint:wrapcheck // the process is gone or about to be
}
