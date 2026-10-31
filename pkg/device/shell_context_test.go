package device

import (
	"context"
	"os/exec"
	"reflect"
	"testing"
	"time"
)

// ShellContext runs `adb -s <serial> shell <cmd>` and kills it when its
// context ends, so a hung adb cannot outlive the caller's deadline.
func TestShellContext(t *testing.T) {
	var gotName string
	var gotArgs []string
	prev := execCommandContext
	t.Cleanup(func() { execCommandContext = prev })
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		gotName, gotArgs = name, args
		return exec.CommandContext(ctx, "printf", "%s", "resumed")
	}
	d := &AndroidDevice{serial: "SERIAL1", adbPath: "/fake/adb"}
	out, err := d.ShellContext(context.Background(), "dumpsys activity")
	if err != nil || out != "resumed" {
		t.Fatalf("got %q, %v", out, err)
	}
	if gotName != "/fake/adb" || !reflect.DeepEqual(gotArgs, []string{"-s", "SERIAL1", "shell", "dumpsys activity"}) {
		t.Errorf("ran %s %v", gotName, gotArgs)
	}

	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sleep", "30")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := d.ShellContext(ctx, "dumpsys activity"); err == nil {
		t.Error("a killed shell should fail")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("the shell outlived its context: %v", elapsed)
	}
}
