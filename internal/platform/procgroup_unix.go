//go:build !windows

package platform

import (
	"os/exec"
	"syscall"
)

// OwnProcessGroup makes cmd the leader of a new process group, so that it and
// the children it starts (yt-dlp starts ffmpeg) can be stopped together by
// KillProcessGroup. Call it before Start.
func OwnProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// KillProcessGroup kills every process left in the group cmd leads. It is for
// cleaning up after cmd has exited or is being killed: a child that outlived it
// would otherwise go on writing into files that are about to be deleted. A
// group that is already gone is not an error.
func KillProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
