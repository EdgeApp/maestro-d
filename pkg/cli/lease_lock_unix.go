//go:build !windows

package cli

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive advisory lock on path (created if missing),
// waiting for another process that holds it.
func lockFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// unlockFile releases a lock lockFile took.
func unlockFile(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}
