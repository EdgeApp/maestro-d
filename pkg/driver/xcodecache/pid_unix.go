//go:build !windows

package xcodecache

import (
	"errors"
	"os"
	"syscall"
)

// pidAlive reports whether a process with pid exists. A variable so tests
// can decide liveness.
var pidAlive = func(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
