package daemon

// housekeeping.go: the sweep every daemon runs shortly after start and hourly, and
// `maestro-d gc` runs on demand. It deletes what no later run reads: old
// blobs, report directories idle for days, temporary Chrome profiles a
// crashed web session left, and the run directories of daemons long gone.
// bin/, drivers/, browsers/, profiles/, hub/ and leases.json are never
// touched; the build cache belongs to the iOS runners' own prune.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/fsage"
	"github.com/devicelab-dev/maestro-runner/pkg/logger"
)

const (
	// reportMaxAge is how long a report directory no live session uses is
	// kept after its last write.
	reportMaxAge = 3 * 24 * time.Hour
	// runDirMaxAge is how long a dead daemon's run directory is kept.
	runDirMaxAge = 7 * 24 * time.Hour
	// rodProfileMaxAge is how long an orphaned go-rod Chrome profile is
	// kept; rodProfileMaxAgeNoPS where running processes cannot be listed.
	rodProfileMaxAge     = 24 * time.Hour
	rodProfileMaxAgeNoPS = 7 * 24 * time.Hour
	// EnvHousekeepingInterval overrides how often a running daemon sweeps
	// (a Go duration; default an hour).
	EnvHousekeepingInterval = "MAESTRO_D_HOUSEKEEPING_INTERVAL"
	defaultSweepInterval    = time.Hour
	housekeepingLockFile    = "housekeeping.lock"
)

// SweepOptions tunes one Sweep.
type SweepOptions struct {
	// DryRun lists what would be deleted without deleting it.
	DryRun bool
	// TempDir is where go-rod keeps rod/user-data; os.TempDir() when "".
	TempDir string
	// BlobsDir is the blob store to expire; "" means <home>/blobs.
	BlobsDir string
	// KeepRunDir names run directories the sweep never removes whole: a
	// daemon with an installed launchd service, whose log launchd writes
	// to run/<name>/daemon.log and cannot recreate the directory for.
	KeepRunDir func(name string) bool
	// procArgs lists the command lines of running processes; nil uses
	// processArgs, the platform's lister.
	procArgs func() ([]string, bool)
}

// SweepEntry is one path a sweep deleted, or would delete.
type SweepEntry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Reason string `json:"reason"`
}

// SweepResult is what one Sweep did.
type SweepResult struct {
	Home    string       `json:"home"`
	DryRun  bool         `json:"dryRun"`
	Removed []SweepEntry `json:"removed"`
	Skipped bool         `json:"skipped,omitempty"` // another sweep of this home was running
}

// Bytes is the total size of what the sweep removed.
func (r SweepResult) Bytes() int64 {
	var n int64
	for _, e := range r.Removed {
		n += e.Size
	}
	return n
}

// Sweep deletes, under home (a MAESTRO_D_HOME):
//   - blobs and partial blobs unused for BlobExpiry;
//   - report directories run/<name>/reports/<device>/<ts>/ idle for
//     reportMaxAge that no live daemon lists in its devices.json;
//   - run/<name>/ of a daemon that is not live, idle for runDirMaxAge;
//   - go-rod Chrome profiles under TempDir idle for rodProfileMaxAge that
//     no running process names in --user-data-dir.
//
// A daemon is live when its daemon.lock is held or its daemon.json pid is
// alive. Where neither test works (Windows, sweepRunDirs false), report and
// run directories are not swept. A sweep already running in another
// process for the same home makes this one return Skipped.
func Sweep(home string, opts SweepOptions) SweepResult {
	res := SweepResult{Home: home, DryRun: opts.DryRun, Removed: []SweepEntry{}}
	lock, err := lockRunDir(filepath.Join(home, housekeepingLockFile))
	if errors.Is(err, errRunLocked) {
		res.Skipped = true
		return res
	}
	if err == nil {
		defer unlockRunDir(lock)
	}

	s := &sweeper{opts: opts, res: &res}
	blobsDir := opts.BlobsDir
	if blobsDir == "" {
		blobsDir = filepath.Join(home, "blobs")
	}
	s.blobs(blobsDir)
	if sweepRunDirs {
		s.runDirs(filepath.Join(home, "run"))
	}
	tmp := opts.TempDir
	if tmp == "" {
		tmp = os.TempDir()
	}
	s.rodProfiles(filepath.Join(tmp, "rod", "user-data"))
	return res
}

type sweeper struct {
	opts SweepOptions
	res  *SweepResult
}

// remove records path and, unless dry, deletes it.
func (s *sweeper) remove(path, reason string) {
	size := fsage.Size(path)
	if !s.opts.DryRun {
		if err := os.RemoveAll(path); err != nil {
			logger.Warn("housekeeping: remove %s: %v", path, err)
			return
		}
	}
	s.res.Removed = append(s.res.Removed, SweepEntry{Path: path, Size: size, Reason: reason})
}

func (s *sweeper) blobs(root string) {
	cutoff := time.Now().Add(-BlobExpiry)
	for _, dir := range []string{"sha256", "partial"} {
		entries, _ := os.ReadDir(filepath.Join(root, dir))
		for _, e := range entries {
			fi, err := e.Info()
			if err != nil || fi.IsDir() || !fi.ModTime().Before(cutoff) {
				continue
			}
			s.remove(filepath.Join(root, dir, e.Name()), fmt.Sprintf("blob unused for %v", BlobExpiry))
		}
	}
}

// runDirs sweeps whole dead run dirs first (their reports go with them),
// then the idle report directories of the run dirs that remain.
func (s *sweeper) runDirs(runRoot string) {
	names, _ := os.ReadDir(runRoot)
	inUse := map[string]bool{}
	live := map[string]bool{}
	for _, n := range names {
		if !n.IsDir() {
			continue
		}
		dir := filepath.Join(runRoot, n.Name())
		if !daemonLive(dir) {
			continue
		}
		live[n.Name()] = true
		for _, d := range readDevicesAt(dir) {
			if d.ReportDir != "" {
				inUse[filepath.Clean(d.ReportDir)] = true
			}
		}
	}
	for _, n := range names {
		if n.IsDir() && strings.HasPrefix(n.Name(), tombstonePrefix) {
			s.remove(filepath.Join(runRoot, n.Name()), "dead run dir left mid-delete")
			continue
		}
		if !n.IsDir() || live[n.Name()] || (s.opts.KeepRunDir != nil && s.opts.KeepRunDir(n.Name())) {
			continue
		}
		s.deadRunDir(filepath.Join(runRoot, n.Name()))
	}
	for _, n := range names {
		if !n.IsDir() || (!s.opts.DryRun && !present(filepath.Join(runRoot, n.Name()))) {
			continue
		}
		reports, _ := filepath.Glob(filepath.Join(runRoot, n.Name(), reportsDirName, "*", "*"))
		for _, r := range reports {
			if fi, err := os.Stat(r); err != nil || !fi.IsDir() || inUse[filepath.Clean(r)] {
				continue
			}
			if s.listed(filepath.Join(runRoot, n.Name())) {
				continue // a dry run already lists the whole run dir
			}
			if fsage.IdleFor(r, reportMaxAge) {
				s.remove(r, fmt.Sprintf("report idle for %v", reportMaxAge))
			}
		}
	}
}

// listed reports whether path is already in the result.
func (s *sweeper) listed(path string) bool {
	for _, e := range s.res.Removed {
		if e.Path == path {
			return true
		}
	}
	return false
}

func present(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// deadRunDir removes a dead daemon's run dir idle for runDirMaxAge. It
// holds the dir's lock while it removes the dir (lock file included), so a
// daemon of that name starting meanwhile fails its start once and a retry
// makes a fresh dir; two daemons of one name never coexist.
func (s *sweeper) deadRunDir(dir string) {
	if !fsage.IdleFor(dir, runDirMaxAge) || recordsBootedDevices(dir) {
		return
	}
	reason := fmt.Sprintf("daemon gone, run dir idle for %v", runDirMaxAge)
	if s.opts.DryRun {
		// Taking the lock would create daemon.lock and make the dir look
		// fresh to the real sweep that follows.
		if !pidLive(dir) {
			s.res.Removed = append(s.res.Removed, SweepEntry{Path: dir, Size: fsage.Size(dir), Reason: reason})
		}
		return
	}
	lock, err := lockRunDir(filepath.Join(dir, "daemon.lock"))
	if err != nil {
		return // held (live) or unreadable: leave it
	}
	// Re-check under the lock: a daemon of this name may have started (or
	// been spawned, writing startup.log) since the first look.
	if pidLive(dir) || !fsage.IdleFor(dir, runDirMaxAge) {
		unlockRunDir(lock)
		return
	}
	size := fsage.Size(dir)
	// Move the dir aside to a tombstone before deleting it, so a daemon of
	// this name that starts meanwhile always makes (and locks) a fresh dir
	// rather than one being deleted under it. On Unix the rename happens
	// under the lock; Windows cannot rename a dir with an open file.
	tomb := filepath.Join(filepath.Dir(dir), fmt.Sprintf("%s%s-%d", tombstonePrefix, filepath.Base(dir), time.Now().UnixNano()))
	if renameWhileLocked {
		err = os.Rename(dir, tomb)
		unlockRunDir(lock)
	} else {
		unlockRunDir(lock)
		err = os.Rename(dir, tomb)
	}
	if err != nil {
		logger.Warn("housekeeping: move %s aside: %v", dir, err)
		return
	}
	if err := os.RemoveAll(tomb); err != nil {
		logger.Warn("housekeeping: remove %s: %v", tomb, err)
	}
	s.res.Removed = append(s.res.Removed, SweepEntry{Path: dir, Size: size, Reason: reason})
}

// tombstonePrefix names a dead run dir moved aside for deletion; a sweep
// removes any it finds (one left by a sweep that crashed mid-delete).
const tombstonePrefix = ".gone-"

func (s *sweeper) rodProfiles(root string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	list := s.opts.procArgs
	if list == nil {
		list = processArgs
	}
	var args []string
	listed, asked := true, false
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if !fsage.IdleFor(dir, rodProfileMaxAge) {
			continue
		}
		if !asked { // list processes only when a profile is old enough to matter
			args, listed = list()
			asked = true
		}
		maxAge := rodProfileMaxAge
		if !listed {
			maxAge = rodProfileMaxAgeNoPS
		}
		if !fsage.IdleFor(dir, maxAge) || namedInArgs(args, dir) {
			continue
		}
		s.remove(dir, fmt.Sprintf("Chrome profile idle for %v", maxAge))
	}
}

// namedInArgs reports whether a running command line uses dir as its
// --user-data-dir.
func namedInArgs(args []string, dir string) bool {
	for _, a := range args {
		if strings.Contains(a, "--user-data-dir="+dir) {
			return true
		}
	}
	return false
}

// daemonLive reports whether the daemon owning run dir dir is running: its
// lock is held, or its daemon.json names a live pid.
func daemonLive(dir string) bool {
	// The pid comes first: only a dir whose daemon.json names no live
	// process gets the (brief) lock probe, which a daemon starting at that
	// moment would see as "already running or starting".
	if pidLive(dir) {
		return true
	}
	lockPath := filepath.Join(dir, "daemon.lock")
	if _, err := os.Stat(lockPath); err != nil {
		return false // never locked, and probing must not create one
	}
	lock, err := lockRunDir(lockPath)
	if errors.Is(err, errRunLocked) {
		return true
	}
	if err == nil {
		unlockRunDir(lock)
	}
	return false
}

func pidLive(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, infoFile))
	if err != nil {
		return false
	}
	var info Info
	if json.Unmarshal(data, &info) != nil || info.PID <= 0 {
		return false
	}
	return PIDAlive(info.PID)
}

// recordsBootedDevices reports whether a run dir's devices.json lists a
// simulator or emulator the daemon booted. A crashed daemon leaves those
// running, and `ps` and `device stop --orphans` find them only through this
// record, so such a run dir is kept until --orphans has shut them down.
func recordsBootedDevices(dir string) bool {
	for _, d := range readDevicesAt(dir) {
		if d.BootedBy == BootedByDaemon {
			return true
		}
	}
	return false
}

func readDevicesAt(dir string) []DeviceInfo {
	data, err := os.ReadFile(filepath.Join(dir, devicesFile))
	if err != nil {
		return nil
	}
	var devices []DeviceInfo
	_ = json.Unmarshal(data, &devices)
	return devices
}

// sweepInterval is how often a running daemon sweeps.
func sweepInterval() time.Duration {
	if v := os.Getenv(EnvHousekeepingInterval); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultSweepInterval
}

// logSweep reports a sweep's deletions in the daemon log.
func logSweep(res SweepResult) {
	for _, e := range res.Removed {
		logger.Info("housekeeping: deleted %s (%d bytes): %s", e.Path, e.Size, e.Reason)
	}
	if n := len(res.Removed); n > 0 {
		logger.Info("housekeeping: freed %d bytes in %d path(s)", res.Bytes(), n)
	}
}

// firstSweepDelay keeps the start-up sweep out of a daemon's busy first
// seconds; a daemon that lives less than that never sweeps.
const firstSweepDelay = 30 * time.Second

// housekeep sweeps Home() shortly after start, then every sweepInterval,
// until ctx ends.
func (s *Server) housekeep(ctx context.Context) {
	interval := sweepInterval()
	select {
	case <-ctx.Done():
		return
	case <-time.After(min(firstSweepDelay, interval)):
	}
	opts := SweepOptions{KeepRunDir: s.cfg.KeepRunDir, BlobsDir: s.cfg.BlobsDir}
	logSweep(Sweep(Home(), opts))
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			logSweep(Sweep(Home(), opts))
		}
	}
}
