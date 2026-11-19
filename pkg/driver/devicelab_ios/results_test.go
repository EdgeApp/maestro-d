package devicelab_ios

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestTestArgsKeepDerivedDataAndResultsInCache(t *testing.T) {
	got := testArgs("/c/Build/Products/r.xctestrun", "platform=iOS Simulator,id=U", "/c", "/c/results/1-2-1.xcresult")
	want := []string{
		"test-without-building",
		"-xctestrun", "/c/Build/Products/r.xctestrun",
		"-destination", "platform=iOS Simulator,id=U",
		"-derivedDataPath", "/c",
		"-resultBundlePath", "/c/results/1-2-1.xcresult",
	}
	if !slices.Equal(got, want) {
		t.Errorf("testArgs = %q, want %q", got, want)
	}
}

func TestRunnerHandleStopRemovesResultBundle(t *testing.T) {
	bundle := filepath.Join(t.TempDir(), "results", "1-1-1.xcresult")
	if err := os.MkdirAll(filepath.Join(bundle, "Staging"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skip("no sleep binary")
	}
	h := &RunnerHandle{cmd: cmd, resultBundle: bundle}
	if err := h.Stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bundle); !os.IsNotExist(err) {
		t.Errorf("result bundle survived Stop: %v", err)
	}
}

func TestPrepareRunnerCacheStampsAndPrunes(t *testing.T) {
	root := filepath.Join(t.TempDir(), runnerBuildsDirName)
	dir := filepath.Join(root, "sim-ios27.2-abc")
	stale := filepath.Join(root, "sim-ios26.0-old")
	for _, d := range []string{filepath.Join(dir, "Build"), filepath.Join(stale, "Build")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-40 * 24 * time.Hour)
	_ = filepath.Walk(stale, func(p string, _ os.FileInfo, _ error) error { return os.Chtimes(p, old, old) })

	seq, release := prepareRunnerCache(dir)
	if seq == 0 {
		t.Error("seq 0")
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("an unused sibling build survived")
	}
	if stamps, _ := filepath.Glob(filepath.Join(dir, ".in-use.*")); len(stamps) != 1 {
		t.Fatalf("in-use stamps = %v", stamps)
	}
	release()
	if stamps, _ := filepath.Glob(filepath.Join(dir, ".in-use.*")); len(stamps) != 0 {
		t.Error("release left the in-use stamp")
	}
}

const fakeXctestrun = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>TestConfigurations</key><array><dict><key>TestTargets</key><array><dict><key>EnvironmentVariables</key><dict/></dict></array></dict></array></dict></plist>
`

// TestStartOnceRemovesBundleWhenXcodebuildExits runs startOnce against an
// xcodebuild that exits at once: the run fails, and its result bundle,
// created by the fake, is gone.
func TestStartOnceRemovesBundleWhenXcodebuildExits(t *testing.T) {
	if _, err := os.Stat("/usr/libexec/PlistBuddy"); err != nil {
		t.Skip("PlistBuddy (macOS) edits the xctestrun")
	}
	dir := t.TempDir()
	xctestrun := filepath.Join(dir, "r.xctestrun")
	if err := os.WriteFile(xctestrun, []byte(fakeXctestrun), 0o644); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(dir, "results", "1-1-1.xcresult")
	var gotArgs []string
	execCommandContext = func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		gotArgs = args
		return exec.CommandContext(ctx, "sh", "-c", "mkdir -p \"$0\"; echo 'xcodebuild: error: fake'; exit 65", bundle)
	}
	t.Cleanup(func() { execCommandContext = exec.CommandContext })

	_, _, err := startOnce(context.Background(), SetupOptions{ArtifactsDir: dir, SimulatorUDID: "U", ReadyTimeout: 5 * time.Second, Stdout: io.Discard, Stderr: io.Discard},
		xctestrun, "", bundle)
	if err == nil {
		t.Fatal("startOnce succeeded with an exiting xcodebuild")
	}
	if !slices.Contains(gotArgs, "-resultBundlePath") || !slices.Contains(gotArgs, "-derivedDataPath") {
		t.Errorf("xcodebuild args %q lack the cache paths", gotArgs)
	}
	if _, err := os.Stat(bundle); !os.IsNotExist(err) {
		t.Error("the failed run's result bundle survived")
	}
}

func TestPrepareRunnerCacheClearsItsLogsTest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), runnerBuildsDirName, "sim-ios27.2-x")
	logsTest := filepath.Join(dir, "Logs", "Test", "LogStoreManifest.plist")
	if err := os.MkdirAll(filepath.Dir(logsTest), 0o755); err != nil {
		t.Fatal(err)
	}
	_, release := prepareRunnerCache(dir)
	if _, err := os.Stat(filepath.Dir(logsTest)); !os.IsNotExist(err) {
		t.Error("an unheld build kept its Logs/Test")
	}
	// A second runner on the same build keeps the first one's log store.
	if err := os.MkdirAll(filepath.Dir(logsTest), 0o755); err != nil {
		t.Fatal(err)
	}
	_, release2 := prepareRunnerCache(dir)
	if _, err := os.Stat(filepath.Dir(logsTest)); err != nil {
		t.Error("a held build lost its Logs/Test")
	}
	release2()
	release()
}
