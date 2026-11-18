package devicelab_ios

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
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
