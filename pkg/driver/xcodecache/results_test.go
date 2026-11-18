package xcodecache

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func fakeLiveness(t *testing.T, live ...int) {
	t.Helper()
	prev := pidAlive
	pidAlive = func(pid int) bool {
		for _, l := range live {
			if l == pid {
				return true
			}
		}
		return false
	}
	t.Cleanup(func() { pidAlive = prev })
}

func TestResultBundlePathAndSeq(t *testing.T) {
	a, b := NextRunnerSeq(), NextRunnerSeq()
	if a == b {
		t.Fatalf("NextRunnerSeq repeated %d", a)
	}
	got := ResultBundlePath("/cache/sim", a, 2)
	want := filepath.Join("/cache/sim", "results", fmt.Sprintf("%d-%d-2.xcresult", os.Getpid(), a))
	if got != want {
		t.Errorf("ResultBundlePath = %q, want %q", got, want)
	}
}

func TestRemoveOrphanResults(t *testing.T) {
	fakeLiveness(t, 100)
	build := t.TempDir()
	dead := filepath.Join(build, "results", "200-1-1.xcresult")
	live := filepath.Join(build, "results", "100-1-1.xcresult")
	odd := filepath.Join(build, "results", "notes")
	for _, p := range []string{dead, live, odd} {
		mkdir(t, p)
	}
	mkdir(t, filepath.Join(build, "DerivedData", "Build"))

	RemoveOrphanResults(build)

	if exists(dead) {
		t.Error("a dead pid's bundle survived")
	}
	if !exists(live) || !exists(odd) || !exists(filepath.Join(build, "DerivedData", "Build")) {
		t.Error("a live bundle, an unparseable name or Build/ was removed")
	}
	RemoveOrphanResults(filepath.Join(build, "missing")) // no results dir: no-op
}

func TestRemoveResultBundles(t *testing.T) {
	dir := t.TempDir()
	b := filepath.Join(dir, "1-1-1.xcresult")
	mkdir(t, filepath.Join(b, "Staging"))
	RemoveResultBundles([]string{b, filepath.Join(dir, "absent")})
	if exists(b) {
		t.Error("bundle survived RemoveResultBundles")
	}
}

func writePlist(t *testing.T, dir, workspace string) {
	t.Helper()
	mkdir(t, dir)
	body := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict><key>WorkspacePath</key><string>` + workspace + `</string></dict></plist>`
	if err := os.WriteFile(filepath.Join(dir, "info.plist"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func age(t *testing.T, dir string, d time.Duration) {
	t.Helper()
	old := time.Now().Add(-d)
	_ = filepath.Walk(dir, func(p string, _ os.FileInfo, _ error) error {
		return os.Chtimes(p, old, old)
	})
}

func TestRemoveStaleDerivedData(t *testing.T) {
	root := t.TempDir()
	prev := DerivedDataRoot
	DerivedDataRoot = func() string { return root }
	t.Cleanup(func() { DerivedDataRoot = prev })

	workspace := t.TempDir()
	gone := filepath.Join(root, "DevicelabIOSRunner-gone")
	writePlist(t, gone, filepath.Join(workspace, "deleted", "Runner.xcodeproj"))
	stale := filepath.Join(root, "DevicelabIOSRunner-stale") // an old test run's: no info.plist
	mkdir(t, filepath.Join(stale, "Logs", "Test"))
	age(t, stale, 48*time.Hour)
	extracted := filepath.Join(root, "DevicelabIOSRunner-extracted")
	srcSlot := filepath.Join(workspace, "embedded-runner-src", "h", "DevicelabIOSRunner.xcodeproj")
	mkdir(t, srcSlot)
	writePlist(t, extracted, srcSlot)
	age(t, extracted, 48*time.Hour)
	contributor := filepath.Join(root, "DevicelabIOSRunner-contributor") // opened in Xcode
	writePlist(t, contributor, workspace)
	age(t, contributor, 48*time.Hour)
	fresh := filepath.Join(root, "DevicelabIOSRunner-fresh")
	writePlist(t, fresh, workspace)
	other := filepath.Join(root, "SomeApp-abc")
	writePlist(t, other, filepath.Join(workspace, "deleted"))
	age(t, other, 48*time.Hour)
	noPlist := filepath.Join(root, "DevicelabIOSRunner-noplist") // fresh: a run in progress
	mkdir(t, noPlist)

	RemoveStaleDerivedData("DevicelabIOSRunner-", 24*time.Hour)

	for _, p := range []string{gone, stale, extracted} {
		if exists(p) {
			t.Errorf("%s survived", filepath.Base(p))
		}
	}
	for _, p := range []string{fresh, other, noPlist, contributor} {
		if !exists(p) {
			t.Errorf("%s was removed", filepath.Base(p))
		}
	}

	DerivedDataRoot = func() string { return "" }
	RemoveStaleDerivedData("DevicelabIOSRunner-", time.Hour) // no root: no-op
	DerivedDataRoot = func() string { return filepath.Join(root, "missing") }
	RemoveStaleDerivedData("DevicelabIOSRunner-", time.Hour)
}

func TestNoIndexStoreSetting(t *testing.T) {
	if NoIndexStoreSetting != "COMPILER_INDEX_STORE_ENABLE=NO" {
		t.Error("NoIndexStoreSetting changed")
	}
}

func TestDefaultsAndPIDParsing(t *testing.T) {
	if !pidAlive(os.Getpid()) {
		t.Error("this process reads as dead")
	}
	if pidAlive(1 << 30) {
		t.Error("an impossible pid reads as alive")
	}
	if root := DerivedDataRoot(); !strings.HasSuffix(root, filepath.Join("Xcode", "DerivedData")) {
		t.Errorf("DerivedDataRoot = %q", root)
	}
	for _, name := range []string{"nodash", "x-1-1.xcresult", "0-1-1.xcresult", "-1-1.xcresult"} {
		if _, ok := ownerPID(name); ok {
			t.Errorf("ownerPID(%q) parsed", name)
		}
	}
	if pid, ok := ownerPID("42-7-1.xcresult"); !ok || pid != 42 {
		t.Errorf("ownerPID = %d, %v", pid, ok)
	}
}
