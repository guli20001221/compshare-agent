//go:build windows

package sshops

import "os/exec"

// Windows has no POSIX process groups. Production runs on Linux, so this is a dev-only fallback: it
// kills the direct child (the same as exec.CommandContext's default). Reaping grandchildren on Windows
// would require a Job Object; it is intentionally out of scope because the lane never ships on Windows.
func setProcGroup(cmd *exec.Cmd) {}

func killProcGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
