package fsage

import (
	"os"
	"path/filepath"
)

// TryHubLock takes the singleton lock for this home. It stays held until
// UnlockFile or process exit, regardless of whether the hub is ready yet.
func TryHubLock(home string) (*os.File, error) {
	dir := filepath.Join(home, "hub")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	return TryLockFile(filepath.Join(dir, "hub.lock"))
}
