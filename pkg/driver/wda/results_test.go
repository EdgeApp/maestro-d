package wda

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestHelperXcodebuild stands in for `xcodebuild test-without-building`
// when a test swaps execCommandContext: it creates the -resultBundlePath
// it was given, prints WDA's ready marker and waits to be killed.
func TestHelperXcodebuild(t *testing.T) {
	if os.Getenv("WDA_FAKE_XCODEBUILD") != "1" {
		return
	}
	args := os.Args
	for i, a := range args {
		if a == "-resultBundlePath" && i+1 < len(args) {
			_ = os.MkdirAll(args[i+1], 0o755)
			_ = os.WriteFile(filepath.Join(args[i+1], "Session.log"), []byte("session"), 0o644)
		}
	}
	fmt.Println("ServerURLHere->http://127.0.0.1:8100<-ServerURLHere")
	time.Sleep(time.Minute)
	os.Exit(0)
}

func fakeXcodebuild(t *testing.T) {
	t.Helper()
	execCommandContext = func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=^TestHelperXcodebuild$", "--"}, args...)...)
		cmd.Env = append(os.Environ(), "WDA_FAKE_XCODEBUILD=1")
		return cmd
	}
	t.Cleanup(func() { execCommandContext = exec.CommandContext })
}

const minimalXctestrun = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>__xctestrun_metadata__</key><dict><key>FormatVersion</key><integer>1</integer></dict></dict></plist>
`

// startedRunner returns a runner on a fresh build dir whose fake
// xcodebuild has reported ready.
func startedRunner(t *testing.T, buildDir string) *Runner {
	t.Helper()
	if _, err := exec.LookPath("plutil"); err != nil {
		t.Skip("plutil (macOS) edits the xctestrun")
	}
	products := filepath.Join(buildDir, "DerivedData", "Build", "Products")
	if err := os.MkdirAll(products, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(buildDir, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	xctestrun := filepath.Join(products, "WebDriverAgentRunner.xctestrun")
	if err := os.WriteFile(xctestrun, []byte(minimalXctestrun), 0o644); err != nil {
		t.Fatal(err)
	}
	r := NewRunner("SIM-UDID-000000000001", "", "")
	r.buildDir = buildDir
	r.runtimeChecked = true
	if err := r.startOnce(context.Background(), xctestrun, filepath.Join(buildDir, "logs", "runner.log"), 1); err != nil {
		t.Fatalf("startOnce: %v", err)
	}
	return r
}

func bundlesIn(t *testing.T, buildDir string) []string {
	t.Helper()
	entries, _ := os.ReadDir(filepath.Join(buildDir, "results"))
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestWDATestArgs(t *testing.T) {
	got := wdaTestArgs("/x.xctestrun", "id=U", "/b/DerivedData", "/b/results/1-2-3.xcresult")
	want := []string{
		"test-without-building", "-xctestrun", "/x.xctestrun", "-destination", "id=U",
		"-derivedDataPath", "/b/DerivedData", "-resultBundlePath", "/b/results/1-2-3.xcresult",
	}
	if !slices.Equal(got, want) {
		t.Errorf("wdaTestArgs = %q, want %q", got, want)
	}
	if a, b := NewRunner("U1", "", ""), NewRunner("U2", "", ""); a.seq == b.seq {
		t.Errorf("two runners share seq %d", a.seq)
	}
}

func TestRunnerStopRemovesItsResultBundle(t *testing.T) {
	fakeXcodebuild(t)
	buildDir := t.TempDir()

	first := startedRunner(t, buildDir)
	second := startedRunner(t, buildDir)
	if got := bundlesIn(t, buildDir); len(got) != 2 {
		t.Fatalf("results while both run = %q, want 2 bundles", got)
	}
	for _, b := range bundlesIn(t, buildDir) {
		if !strings.HasPrefix(b, fmt.Sprintf("%d-", os.Getpid())) || !strings.HasSuffix(b, "-1.xcresult") {
			t.Errorf("bundle %q is not <pid>-<seq>-1.xcresult", b)
		}
	}

	first.Stop()
	got := bundlesIn(t, buildDir)
	want := fmt.Sprintf("%d-%d-1.xcresult", os.Getpid(), second.seq)
	if len(got) != 1 || got[0] != want {
		t.Fatalf("after the first Stop = %q, want only %q", got, want)
	}
	second.Stop()
	if got := bundlesIn(t, buildDir); len(got) != 0 {
		t.Errorf("after both Stops = %q, want none", got)
	}
	second.Stop() // a second Stop with nothing to remove is fine
}

func TestRunnerBuildArgumentsSkipIndexStore(t *testing.T) {
	for _, r := range []*Runner{
		{deviceUDID: "U", runtimeChecked: true, buildDir: "/b"},
		{deviceUDID: "U", runtimeChecked: true, runtimePlatform: "tvOS", buildDir: "/b"},
	} {
		if args := r.buildArguments("/p"); !slices.Contains(args, "COMPILER_INDEX_STORE_ENABLE=NO") {
			t.Errorf("buildArguments (platform %q) = %q, want COMPILER_INDEX_STORE_ENABLE=NO", r.runtimePlatform, args)
		}
	}
}
