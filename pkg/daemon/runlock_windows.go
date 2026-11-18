//go:build windows

package daemon

import (
	"errors"
	"os"
)

var errRunLocked = errors.New("run dir locked")

// lockRunDir opens path; Windows has no flock, so it guards nothing there.
func lockRunDir(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
}

// unlockRunDir closes a file lockRunDir opened.
func unlockRunDir(f *os.File) { _ = f.Close() }
