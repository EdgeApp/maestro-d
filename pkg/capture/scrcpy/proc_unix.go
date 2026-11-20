//go:build !windows

package scrcpy

import (
	"os/exec"
	"syscall"
)

// setProcessGroup starts the adb shell in its own process group, so
// stopping it also stops anything adb started.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup kills the adb shell's process group. A group that has
// already exited is not an error.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}
