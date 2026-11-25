package daemon

import "strings"

// The computer device (pkg/driver/computer): the Mac the daemon runs on. It
// is local only: never in a hub's pool, never attached by a remote caller.
const (
	ComputerID       = "mac"
	ComputerPlatform = "macos"
	ComputerKind     = "computer"
)

func IsComputer(d DeviceInfo) bool {
	return strings.EqualFold(d.Kind, ComputerKind) || strings.EqualFold(d.Platform, ComputerPlatform)
}
