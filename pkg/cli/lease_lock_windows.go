//go:build windows

package cli

import "os"

// lockFile opens path; Windows has no flock, so leases.json is serialised
// only within one process there.
func lockFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
}

// unlockFile closes a file lockFile opened.
func unlockFile(f *os.File) { _ = f.Close() }
