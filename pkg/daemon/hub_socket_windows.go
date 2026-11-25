//go:build windows

package daemon

import "os"

// Windows uses the current user's temporary directory for the short socket path.
func privateHubSocketDir(info os.FileInfo) bool { return info.IsDir() }
