//go:build windows

package daemon

// sweepRunDirs: on Windows lockRunDir guards nothing and PIDAlive reads
// every process as dead, so no daemon's liveness can be tested; report and
// run directories are left alone there.
var sweepRunDirs = false

// renameWhileLocked: Windows cannot rename a dir holding an open file.
const renameWhileLocked = false

// processArgs cannot list command lines here; profiles are swept by a
// longer age alone (Windows refuses to delete files Chrome holds open).
func processArgs() ([]string, bool) { return nil, false }
