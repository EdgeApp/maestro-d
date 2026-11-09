//go:build !windows

package daemon

import (
	"errors"
	"os"
	"syscall"
)

// errRunLocked: another process holds the daemon's run-dir lock.
var errRunLocked = errors.New("run dir locked")

// lockRunDir takes the exclusive lock a daemon holds on its run dir for as
// long as it runs, without waiting: errRunLocked when another process (or
// another Server in this one) holds it. The kernel drops the lock when the
// process exits, so a crashed daemon leaves none behind.
func lockRunDir(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errRunLocked
		}
		return nil, err
	}
	return f, nil
}

// unlockRunDir releases a lock lockRunDir took.
func unlockRunDir(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}
