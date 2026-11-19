package xcodecache

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// cacheConfig makes a cache directory with a Build/ file, last used ago, and
// optionally built here builtAgo ago (0 = never).
func cacheConfig(t *testing.T, root, name string, ago, builtAgo time.Duration) string {
	t.Helper()
	dir := filepath.Join(root, name)
	mkdir(t, filepath.Join(dir, "Build"))
	if err := os.WriteFile(filepath.Join(dir, "Build", "x"), []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	age(t, dir, ago)
	stamp := filepath.Join(dir, LastUsedFile)
	touch(stamp)
	old := time.Now().Add(-ago)
	_ = os.Chtimes(stamp, old, old)
	if builtAgo > 0 {
		b := filepath.Join(dir, BuiltHereFile)
		touch(b)
		bt := time.Now().Add(-builtAgo)
		_ = os.Chtimes(b, bt, bt)
	}
	return dir
}

const day = 24 * time.Hour

func TestPruneBuildCacheAgeKeepAndInUse(t *testing.T) {
	fakeLiveness(t, 777)
	root := t.TempDir()
	old := cacheConfig(t, root, "sim-ios25.0-iphone", 31*day, 0)
	recent := cacheConfig(t, root, "sim-ios26.0-iphone", 29*day, 0)
	keep := cacheConfig(t, root, "sim-ios24.0-iphone", 60*day, 0)
	held := cacheConfig(t, root, "sim-ios23.0-iphone", 60*day, 0)
	touch(filepath.Join(held, ".in-use.777-1"))
	deadHolder := cacheConfig(t, root, "sim-ios22.0-iphone", 60*day, 0)
	touch(filepath.Join(deadHolder, ".in-use.888-1"))
	noStamp := filepath.Join(root, "sim-ios21.0-iphone")
	mkdir(t, noStamp)
	age(t, noStamp, 40*day)
	if err := os.WriteFile(filepath.Join(root, "stray-file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	dry := PruneBuildCache(root, keep, DefaultMaxAge, true)
	if len(dry) != 3 || !exists(old) {
		t.Fatalf("dry run = %+v, want 3 listed and nothing deleted", dry)
	}
	got := PruneBuildCache(root, keep, DefaultMaxAge, false)
	if len(got) != 3 {
		t.Fatalf("pruned %+v, want 3", got)
	}
	for _, p := range []string{old, deadHolder, noStamp} {
		if exists(p) {
			t.Errorf("%s survived", filepath.Base(p))
		}
	}
	for _, p := range []string{recent, keep, held} {
		if !exists(p) {
			t.Errorf("%s was pruned", filepath.Base(p))
		}
	}
	for _, g := range got {
		if g.Reason == "" || (g.Path == old && g.Size != 5) {
			t.Errorf("pruned entry lacks its size or reason: %+v", g)
		}
	}
	if PruneBuildCache(filepath.Join(root, "missing"), "", DefaultMaxAge, false) != nil {
		t.Error("missing root should prune nothing")
	}
}

func TestPruneBuildCacheDeviceBuildsNeedSigningHere(t *testing.T) {
	root := t.TempDir()
	onlyA := cacheConfig(t, root, "device-ios18.0-teamA", 60*day, 0)
	unsignedB := cacheConfig(t, root, "device-ios25.0-teamB", 60*day, 0)
	_ = cacheConfig(t, root, "device-ios26.0-teamB", 10*day, 0)
	PruneBuildCache(root, "", DefaultMaxAge, false)
	if !exists(onlyA) || !exists(unsignedB) {
		t.Fatal("a device build was pruned on a host that never compiled for its team")
	}

	_ = cacheConfig(t, root, "device-ios26.3-teamA", 2*day, 2*day)
	tv := cacheConfig(t, root, "device-tvosunknown-teamA-bundlecom.x.tv", 40*day, 0)
	noTeam := cacheConfig(t, root, "device-iosunknown", 40*day, 0)
	stale := cacheConfig(t, root, "device-ios17.0-teamA", 60*day, 70*day)
	PruneBuildCache(root, "", DefaultMaxAge, false)
	for _, p := range []string{onlyA, tv, stale} {
		if exists(p) {
			t.Errorf("%s survived after team A was rebuilt here", filepath.Base(p))
		}
	}
	if !exists(unsignedB) || !exists(noTeam) {
		t.Error("team B's or a team-less device build was pruned")
	}
}

func TestStampUseAndTeamOf(t *testing.T) {
	fakeLiveness(t, os.Getpid())
	dir := t.TempDir()
	release := StampUse(dir, 5)
	if !exists(filepath.Join(dir, LastUsedFile)) || !HeldByLiveRunner(dir) {
		t.Fatal("StampUse wrote no .last-used or in-use stamp")
	}
	release()
	if HeldByLiveRunner(dir) {
		t.Error("in-use stamp survived release")
	}
	MarkBuiltHere(dir)
	Touch(dir)
	if !exists(filepath.Join(dir, BuiltHereFile)) {
		t.Error("MarkBuiltHere wrote nothing")
	}
	for name, want := range map[string]string{
		"device-ios26.6.1-teamABCDE12345":                             "ABCDE12345",
		"device-iosunknown-teamABCDE12345-bundlecom.example.soak.wda": "ABCDE12345",
		"device-tvosunknown-teamFGHIJ67890":                           "FGHIJ67890",
		"sim-ios27.2-iphone":                                          "",
	} {
		if got := teamOf(name); got != want {
			t.Errorf("teamOf(%q) = %q, want %q", name, got, want)
		}
	}
	if !LastUsed(filepath.Join(dir, "missing")).IsZero() {
		t.Error("LastUsed of a missing dir should be zero")
	}
}

func TestStampUseClearsDeadStamps(t *testing.T) {
	fakeLiveness(t, os.Getpid())
	dir := t.TempDir()
	touch(filepath.Join(dir, ".in-use.999999-1"))
	release := StampUse(dir, 3)
	defer release()
	if exists(filepath.Join(dir, ".in-use.999999-1")) {
		t.Error("a dead runner's in-use stamp survived StampUse")
	}
}
