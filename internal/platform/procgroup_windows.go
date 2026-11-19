//go:build windows

package platform

import (
	"os/exec"
	"strconv"
)

// OwnProcessGroup is a no-op on Windows: KillProcessGroup uses taskkill /T.
func OwnProcessGroup(*exec.Cmd) {}

// KillProcessGroup kills the process cmd started and its children.
func KillProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run() //nolint:gosec // the pid is our own child's
}
