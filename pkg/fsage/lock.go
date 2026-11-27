package fsage

import (
	"errors"
	"os"
)

// ErrLocked means another caller holds the lock requested by TryLockFile.
var ErrLocked = errors.New("file is locked")

// LockFile takes an exclusive interprocess lock, waiting until it is available.
func LockFile(path string) (*os.File, error) { return openLock(path, true) }

// TryLockFile returns ErrLocked immediately if another caller holds the lock.
func TryLockFile(path string) (*os.File, error) { return openLock(path, false) }

func openLock(path string, wait bool) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockHandle(f, wait); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// UnlockFile releases the lock and closes its handle. The lock file stays in
// place so other callers opening the path continue to lock the same file.
func UnlockFile(f *os.File) {
	unlockHandle(f)
	_ = f.Close()
}
