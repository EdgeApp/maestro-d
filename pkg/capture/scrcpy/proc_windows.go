//go:build windows

package scrcpy

import (
	"errors"
	"os"
	"os/exec"
)

// setProcessGroup does nothing on Windows: the adb shell is killed by
// itself.
func setProcessGroup(*exec.Cmd) {}

// killProcessGroup kills the adb shell process. One that has already
// exited is not an error.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}
