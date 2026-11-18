package xcodecache

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"howett.net/plist"

	"github.com/devicelab-dev/maestro-runner/pkg/config"
	"github.com/devicelab-dev/maestro-runner/pkg/fsage"
)

// DerivedDataRoot is Xcode's default DerivedData directory, where a
// test run without -derivedDataPath writes. A variable so tests can point
// it elsewhere.
var DerivedDataRoot = func() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "Developer", "Xcode", "DerivedData")
}

// RemoveStaleDerivedData deletes each <DerivedDataRoot>/<prefix>* folder
// whose info.plist WorkspacePath no longer exists, or that maestro-d's own
// test runs made (runnerOwned) and nothing has written for maxAge. Runners
// now pass -derivedDataPath, so only an older binary still writes there;
// maxAge spares a folder such a run is using.
func RemoveStaleDerivedData(prefix string, maxAge time.Duration) {
	root := DerivedDataRoot()
	if root == "" {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if workspaceGone(dir) || (fsage.IdleFor(dir, maxAge) && runnerOwned(dir)) {
			_ = os.RemoveAll(dir)
		}
	}
}

// workspaceGone reports whether a DerivedData folder's info.plist names a
// WorkspacePath that no longer exists. An unreadable plist is not "gone".
func workspaceGone(dir string) bool {
	ws, ok := workspacePath(dir)
	if !ok {
		return false
	}
	_, err := os.Stat(ws)
	return os.IsNotExist(err)
}

// runnerOwned reports whether a DevicelabIOSRunner-* DerivedData folder
// was made by maestro-d's own test runs: it has no info.plist (a test run
// writes only Logs/Test), or its workspace lies under maestro-d's home or
// an embedded-runner-src extraction. A contributor's folder for the
// project opened in Xcode is not.
func runnerOwned(dir string) bool {
	ws, ok := workspacePath(dir)
	if !ok {
		return true
	}
	if strings.Contains(ws, string(filepath.Separator)+"embedded-runner-src"+string(filepath.Separator)) {
		return true
	}
	home := config.GetHome()
	return home != "" && strings.HasPrefix(ws, home+string(filepath.Separator))
}

// workspacePath reads a DerivedData folder's info.plist WorkspacePath;
// false when there is none.
func workspacePath(dir string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(dir, "info.plist"))
	if err != nil {
		return "", false
	}
	var info struct {
		WorkspacePath string `plist:"WorkspacePath"`
	}
	if _, err := plist.Unmarshal(data, &info); err != nil || info.WorkspacePath == "" {
		return "", false
	}
	return info.WorkspacePath, true
}
