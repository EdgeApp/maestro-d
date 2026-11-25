//go:build !windows

package fsage

import (
	"os"
	"syscall"
)

func lockHandle(f *os.File, wait bool) error {
	mode := syscall.LOCK_EX
	if !wait {
		mode |= syscall.LOCK_NB
	}
	for {
		err := syscall.Flock(int(f.Fd()), mode)
		switch err {
		case syscall.EINTR:
			continue
		case syscall.EWOULDBLOCK:
			return ErrLocked
		default:
			return err
		}
	}
}

func unlockHandle(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
