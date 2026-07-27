//go:build !windows

package sshops

import (
	"os/exec"
	"syscall"
)

// setProcGroup puts the harness in its OWN process group (pgid == its pid). Everything it spawns — the
// SDK's claude CLI and that CLI's node child — inherits the group, so the whole tree can be signalled
// at once. Without this, killing the direct python child orphans the grandchildren (a resource/PID
// leak that accumulates across sessions on a shared pod; item 4).
func setProcGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcGroup SIGKILLs the whole process group (negative pid). Safe to call after the process has
// already exited — the resulting ESRCH is ignored by the caller.
func killProcGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
