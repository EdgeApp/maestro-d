//go:build !windows

package daemon

import (
	"os/exec"
	"strings"
)

// sweepRunDirs: on Unix a daemon's liveness is testable (its flock, its
// pid), so report and run directories are swept.
var sweepRunDirs = true

// renameWhileLocked: a dir holding an open, locked file can be renamed, so
// a dead run dir is moved aside before its lock is released.
const renameWhileLocked = true

// processArgs lists running command lines with ps.
func processArgs() ([]string, bool) {
	out, err := exec.Command("ps", "-axww", "-o", "args=").Output()
	if err != nil {
		return nil, false
	}
	return strings.Split(string(out), "\n"), true
}
