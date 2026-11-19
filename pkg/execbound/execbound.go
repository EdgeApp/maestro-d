// Package execbound runs a command under a time bound. maestro-d's device
// listings read adb and simctl on a path a hub gives each peer 3 s to
// answer, so no read may outlive its bound.
package execbound

import (
	"errors"
	"os/exec"
	"time"
)

// ErrTimeout is the error of a command killed at its bound.
var ErrTimeout = errors.New("timed out")

// waitDelay bounds the wait for a finished or killed command's output to
// close: its children (a wrapper script that does not exec the tool) may
// hold stdout open after it is gone.
const waitDelay = 100 * time.Millisecond

// Run starts cmd and waits for it, killing it once timeout passes. It
// returns cmd's own error, or ErrTimeout when it was killed. A command that
// exits 0 but leaves a child holding stdout succeeds: its output is
// complete, and only the wait for the child's copy of the pipe is cut off.
func Run(cmd *exec.Cmd, timeout time.Duration) error {
	cmd.WaitDelay = waitDelay
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if errors.Is(err, exec.ErrWaitDelay) {
			return nil
		}
		return err
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		<-done
		return ErrTimeout
	}
}
