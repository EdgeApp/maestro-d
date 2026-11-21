//go:build !windows

package daemon

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
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

// lockLedger takes the cloud ledger's lock (cloudledger.go), waiting for
// its holder: it retries every 50 ms until wait has passed, then fails.
// The holder keeps it only for one read-modify-write, never across a call
// to a cloud provider.
func lockLedger(path string, wait time.Duration) (*os.File, error) {
	deadline := time.Now().Add(wait)
	for {
		f, err := lockRunDir(path)
		if !errors.Is(err, errRunLocked) {
			return f, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the cloud ledger %s is locked by another process", path)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
