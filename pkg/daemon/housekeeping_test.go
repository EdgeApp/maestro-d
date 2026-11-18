package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const hkDay = 24 * time.Hour

func writeFile(t *testing.T, path string, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ageTree sets every mtime under path to ago.
func ageTree(t *testing.T, path string, ago time.Duration) {
	t.Helper()
	old := time.Now().Add(-ago)
	_ = filepath.Walk(path, func(p string, _ os.FileInfo, _ error) error { return os.Chtimes(p, old, old) })
}

func removedPaths(r SweepResult) map[string]bool {
	m := map[string]bool{}
	for _, e := range r.Removed {
		m[e.Path] = true
	}
	return m
}

func noProcs() ([]string, bool) { return nil, true }

func TestSweepBlobs(t *testing.T) {
	home := t.TempDir()
	oldBlob := filepath.Join(home, "blobs", "sha256", "aa")
	oldPartial := filepath.Join(home, "blobs", "partial", "bb")
	fresh := filepath.Join(home, "blobs", "sha256", "cc")
	for _, p := range []string{oldBlob, oldPartial, fresh} {
		writeFile(t, p, "x")
	}
	ageTree(t, oldBlob, 25*time.Hour)
	ageTree(t, oldPartial, 25*time.Hour)

	r := Sweep(home, SweepOptions{procArgs: noProcs, TempDir: t.TempDir()})
	got := removedPaths(r)
	if !got[oldBlob] || !got[oldPartial] || got[fresh] || r.Bytes() != 2 {
		t.Errorf("removed %v (%d bytes)", r.Removed, r.Bytes())
	}
	if !present(fresh) || present(oldBlob) {
		t.Error("wrong blobs deleted")
	}
}

func TestSweepReportsAndRunDirs(t *testing.T) {
	home := t.TempDir()
	run := filepath.Join(home, "run")

	// A live daemon (lock held) listing one of its report dirs in use.
	live := filepath.Join(run, "live")
	inUse := filepath.Join(live, "reports", "mock-1", "20261101-000000")
	idle := filepath.Join(live, "reports", "mock-2", "20261101-000000")
	busy := filepath.Join(live, "reports", "mock-3", "20261101-000000")
	writeFile(t, filepath.Join(inUse, "report.json"), "{}")
	writeFile(t, filepath.Join(idle, "report.json"), "{}")
	writeFile(t, filepath.Join(busy, "report.json"), "{}")
	ageTree(t, live, 4*hkDay)
	writeFile(t, filepath.Join(busy, "assets", "new.png"), "x") // written an hour ago
	devices, _ := json.Marshal([]DeviceInfo{{ID: "mock-1", ReportDir: inUse}})
	writeFile(t, filepath.Join(live, devicesFile), string(devices))
	liveLock, err := lockRunDir(filepath.Join(live, "daemon.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlockRunDir(liveLock)

	// Dead daemons: one idle 8 days, one 6 days; one whose lock is free
	// but whose pid is alive.
	dead := filepath.Join(run, "dead")
	writeFile(t, filepath.Join(dead, "daemon.log"), "old")
	ageTree(t, dead, 8*hkDay)
	young := filepath.Join(run, "young")
	writeFile(t, filepath.Join(young, "daemon.log"), "old")
	ageTree(t, young, 6*hkDay)
	pidOnly := filepath.Join(run, "pid-only")
	info, _ := json.Marshal(Info{Name: "pid-only", PID: os.Getpid()})
	writeFile(t, filepath.Join(pidOnly, infoFile), string(info))
	ageTree(t, pidOnly, 8*hkDay)

	// Never touched, however old.
	for _, p := range []string{"bin/maestro-d", "drivers/x", "browsers/c/x", "profiles/p/x", "hub/secrets/token", "leases.json"} {
		writeFile(t, filepath.Join(home, p), "x")
	}
	for _, p := range []string{"bin", "drivers", "browsers", "profiles", "hub", "leases.json"} {
		ageTree(t, filepath.Join(home, p), 30*hkDay)
	}

	dry := Sweep(home, SweepOptions{DryRun: true, procArgs: noProcs, TempDir: t.TempDir()})
	if len(dry.Removed) != 2 || !present(idle) || !present(dead) {
		t.Fatalf("dry run = %+v, want 2 listed, nothing deleted", dry.Removed)
	}

	r := Sweep(home, SweepOptions{procArgs: noProcs, TempDir: t.TempDir()})
	got := removedPaths(r)
	if !got[idle] || !got[dead] || len(r.Removed) != 2 {
		t.Fatalf("removed %+v, want the idle report and the dead run dir", r.Removed)
	}
	for _, p := range []string{inUse, busy, young, pidOnly, filepath.Join(live, "daemon.lock")} {
		if !present(p) {
			t.Errorf("%s was swept", p)
		}
	}
	for _, p := range []string{"bin", "drivers", "browsers", "profiles", "hub", "leases.json"} {
		if !present(filepath.Join(home, p)) {
			t.Errorf("%s was swept", p)
		}
	}
	// The dead dir and its lock are gone; a daemon of that name locks a
	// fresh file.
	if present(dead) {
		t.Fatal("dead run dir survived")
	}
	if err := os.MkdirAll(dead, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := lockRunDir(filepath.Join(dead, "daemon.lock"))
	if err != nil {
		t.Fatalf("a fresh daemon could not lock: %v", err)
	}
	unlockRunDir(f)
}

func TestSweepSkipsRunDirsWhereLivenessIsUntestable(t *testing.T) {
	prev := sweepRunDirs
	sweepRunDirs = false
	t.Cleanup(func() { sweepRunDirs = prev })
	home := t.TempDir()
	dead := filepath.Join(home, "run", "dead", "daemon.log")
	blob := filepath.Join(home, "blobs", "sha256", "aa")
	writeFile(t, dead, "x")
	writeFile(t, blob, "x")
	ageTree(t, home, 10*hkDay)
	Sweep(home, SweepOptions{procArgs: noProcs, TempDir: t.TempDir()})
	if !present(dead) || present(blob) {
		t.Error("with sweepRunDirs false the run dir must stay and the old blob go")
	}
}

func TestSweepRodProfiles(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "rod", "user-data")
	old := filepath.Join(root, "old")
	running := filepath.Join(root, "running")
	fresh := filepath.Join(root, "fresh")
	for _, p := range []string{old, running, fresh} {
		writeFile(t, filepath.Join(p, "Default", "Preferences"), "{}")
	}
	ageTree(t, old, 2*hkDay)
	ageTree(t, running, 2*hkDay)
	procs := func() ([]string, bool) {
		return []string{"/x/Chromium --headless --user-data-dir=" + running + " --no-first-run"}, true
	}
	Sweep(t.TempDir(), SweepOptions{TempDir: tmp, procArgs: procs})
	if present(old) || !present(running) || !present(fresh) {
		t.Errorf("old gone=%v running kept=%v fresh kept=%v", !present(old), present(running), present(fresh))
	}

	// Without a process list only profiles a week old go.
	ageTree(t, running, 3*hkDay)
	Sweep(t.TempDir(), SweepOptions{TempDir: tmp, procArgs: func() ([]string, bool) { return nil, false }})
	if !present(running) {
		t.Error("without ps a 3-day-old profile was swept")
	}
	if _, listed := processArgs(); !listed {
		t.Log("ps unavailable here")
	}
}

func TestSweepSkipsWhileAnotherSweepRuns(t *testing.T) {
	home := t.TempDir()
	lock, err := lockRunDir(filepath.Join(home, housekeepingLockFile))
	if err != nil {
		t.Fatal(err)
	}
	defer unlockRunDir(lock)
	if r := Sweep(home, SweepOptions{procArgs: noProcs}); !r.Skipped {
		t.Error("a sweep ran while another held the housekeeping lock")
	}
}

func TestSweepInterval(t *testing.T) {
	t.Setenv(EnvHousekeepingInterval, "")
	if sweepInterval() != time.Hour {
		t.Error("default interval is not an hour")
	}
	t.Setenv(EnvHousekeepingInterval, "30s")
	if sweepInterval() != 30*time.Second {
		t.Error("interval override ignored")
	}
	t.Setenv(EnvHousekeepingInterval, "nonsense")
	if sweepInterval() != time.Hour {
		t.Error("a bad override should fall back to an hour")
	}
	logSweep(SweepResult{Removed: []SweepEntry{{Path: "/x", Size: 1, Reason: "r"}}})
}

func TestHousekeepLoopSweepsTheHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv(EnvHome, home)
	t.Setenv(EnvHousekeepingInterval, "20ms")
	blob := filepath.Join(home, "blobs", "sha256", "aa")
	writeFile(t, blob, "x")
	ageTree(t, blob, 2*hkDay)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { (&Server{}).housekeep(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for present(blob) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // let the ticker fire once more
	cancel()
	<-done
	if present(blob) {
		t.Error("the housekeeping loop never swept the old blob")
	}

	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	(&Server{}).housekeep(ctx) // cancelled before the first sweep: returns
}

func TestSweepEdgeCases(t *testing.T) {
	home := t.TempDir()
	tmp := t.TempDir()
	// Stray files where directories are expected are skipped.
	writeFile(t, filepath.Join(home, "run", "stray-file"), "x")
	writeFile(t, filepath.Join(tmp, "rod", "user-data", "stray-file"), "x")
	// A dead daemon that once held its lock: the lock file exists, free.
	locked := filepath.Join(home, "run", "was-locked")
	writeFile(t, filepath.Join(locked, "daemon.lock"), "")
	writeFile(t, filepath.Join(locked, infoFile), "not json")
	ageTree(t, locked, 8*hkDay)
	// A blob that cannot be removed (its directory is read-only).
	stuck := filepath.Join(home, "blobs", "sha256", "aa")
	writeFile(t, stuck, "x")
	ageTree(t, stuck, 2*hkDay)
	if err := os.Chmod(filepath.Dir(stuck), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(stuck), 0o755) })

	r := Sweep(home, SweepOptions{TempDir: tmp, procArgs: noProcs})
	got := removedPaths(r)
	if !got[locked] || present(locked) {
		t.Errorf("a dead daemon's once-locked run dir survived: %+v", r.Removed)
	}
	if got[stuck] || !present(stuck) {
		t.Error("an unremovable blob was reported as removed")
	}
	if !present(filepath.Join(home, "run", "stray-file")) || !present(filepath.Join(tmp, "rod", "user-data", "stray-file")) {
		t.Error("stray files were swept")
	}
	if pidLive(filepath.Join(home, "run", "missing")) {
		t.Error("a missing daemon.json reads as live")
	}
}

// A dead daemon's run dir that still holds an old report is removed whole,
// by a dry run's listing and by the real sweep alike.
func TestSweepDeadRunDirWithOldReport(t *testing.T) {
	home := t.TempDir()
	dead := filepath.Join(home, "run", "idle-01")
	writeFile(t, filepath.Join(dead, "daemon.log"), "x")
	writeFile(t, filepath.Join(dead, "reports", "web", "20261101-000000", "report.json"), "{}")
	ageTree(t, dead, 10*hkDay)

	dry := Sweep(home, SweepOptions{DryRun: true, procArgs: noProcs, TempDir: t.TempDir()})
	if len(dry.Removed) != 1 || dry.Removed[0].Path != dead {
		t.Fatalf("dry run = %+v, want only the run dir", dry.Removed)
	}
	r := Sweep(home, SweepOptions{procArgs: noProcs, TempDir: t.TempDir()})
	if present(dead) || len(r.Removed) != 1 || r.Removed[0].Path != dead {
		t.Errorf("sweep = %+v; run dir present: %v", r.Removed, present(dead))
	}
}

func TestSweepKeepsRunDirsOfInstalledServices(t *testing.T) {
	home := t.TempDir()
	svc := filepath.Join(home, "run", "hub")
	other := filepath.Join(home, "run", "gone")
	writeFile(t, filepath.Join(svc, "daemon.log"), "x")
	writeFile(t, filepath.Join(other, "daemon.log"), "x")
	ageTree(t, home, 10*hkDay)
	Sweep(home, SweepOptions{procArgs: noProcs, TempDir: t.TempDir(), KeepRunDir: func(name string) bool { return name == "hub" }})
	if !present(svc) {
		t.Error("an installed service's run dir was swept")
	}
	if present(other) {
		t.Error("a dead daemon's run dir without a service survived")
	}
}

func TestSweepRemovesLeftoverTombstones(t *testing.T) {
	home := t.TempDir()
	tomb := filepath.Join(home, "run", ".gone-old-123")
	writeFile(t, filepath.Join(tomb, "daemon.log"), "x")
	r := Sweep(home, SweepOptions{procArgs: noProcs, TempDir: t.TempDir()})
	if present(tomb) || !removedPaths(r)[tomb] {
		t.Errorf("a leftover tombstone survived: %+v", r.Removed)
	}
	// A dead run dir is moved aside before deletion: nothing named after
	// it, and no tombstone, remains.
	dead := filepath.Join(home, "run", "dead")
	writeFile(t, filepath.Join(dead, "daemon.log"), "x")
	ageTree(t, dead, 10*hkDay)
	Sweep(home, SweepOptions{procArgs: noProcs, TempDir: t.TempDir()})
	if entries, _ := os.ReadDir(filepath.Join(home, "run")); len(entries) != 0 {
		t.Errorf("run/ still holds %v", entries)
	}
}

func TestSweepExpiresTheConfiguredBlobsDir(t *testing.T) {
	home, store := t.TempDir(), t.TempDir()
	blob := filepath.Join(store, "sha256", "aa")
	writeFile(t, blob, "x")
	ageTree(t, blob, 2*hkDay)
	Sweep(home, SweepOptions{BlobsDir: store, procArgs: noProcs, TempDir: t.TempDir()})
	if present(blob) {
		t.Error("the server's own blob store was not expired")
	}
}

func TestSweepKeepsRunDirsThatRecordBootedDevices(t *testing.T) {
	home := t.TempDir()
	crashed := filepath.Join(home, "run", "crashed")
	devices, _ := json.Marshal([]DeviceInfo{{ID: "SIM-1", BootedBy: BootedByDaemon}})
	writeFile(t, filepath.Join(crashed, devicesFile), string(devices))
	external := filepath.Join(home, "run", "external")
	devices, _ = json.Marshal([]DeviceInfo{{ID: "SIM-2", BootedBy: BootedByExternal}})
	writeFile(t, filepath.Join(external, devicesFile), string(devices))
	ageTree(t, home, 10*hkDay)

	for _, dry := range []bool{true, false} {
		r := Sweep(home, SweepOptions{DryRun: dry, procArgs: noProcs, TempDir: t.TempDir()})
		if removedPaths(r)[crashed] {
			t.Errorf("dry=%v: a run dir recording a daemon-booted device was swept", dry)
		}
	}
	if !present(crashed) || present(external) {
		t.Errorf("crashed kept=%v, external gone=%v", present(crashed), !present(external))
	}
}
