package execbound

import (
	"bytes"
	"errors"
	"os/exec"
	"testing"
	"time"
)

func TestRunReturnsTheCommandsResult(t *testing.T) {
	var out bytes.Buffer
	cmd := exec.Command("printf", "%s", "ok")
	cmd.Stdout = &out
	if err := Run(cmd, time.Second); err != nil || out.String() != "ok" {
		t.Fatalf("Run = %v, output %q", err, out.String())
	}
	var exit *exec.ExitError
	if err := Run(exec.Command("false"), time.Second); !errors.As(err, &exit) {
		t.Errorf("a failing command: %v", err)
	}
	if err := Run(exec.Command("/nonexistent/tool"), time.Second); err == nil || errors.Is(err, ErrTimeout) {
		t.Errorf("a command that cannot start: %v", err)
	}
}

func TestRunKillsAtTheBound(t *testing.T) {
	start := time.Now()
	if err := Run(exec.Command("sleep", "5"), 50*time.Millisecond); !errors.Is(err, ErrTimeout) {
		t.Errorf("err = %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %v", d)
	}
}

// A wrapper that does not exec its tool leaves a child holding stdout when
// it is killed; Run still ends near the bound.
func TestRunDoesNotWaitForAKilledWrappersChild(t *testing.T) {
	var out bytes.Buffer
	cmd := exec.Command("sh", "-c", "sleep 5; true")
	cmd.Stdout = &out
	start := time.Now()
	if err := Run(cmd, 50*time.Millisecond); !errors.Is(err, ErrTimeout) {
		t.Errorf("err = %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("took %v; it waited for the child", d)
	}
}

// A command that exits 0 and leaves a child holding stdout succeeds, with
// its output, about waitDelay after it exits.
func TestRunSucceedsWhenAChildOutlivesTheCommand(t *testing.T) {
	var out bytes.Buffer
	cmd := exec.Command("sh", "-c", "printf ok; sleep 2 &")
	cmd.Stdout = &out
	start := time.Now()
	if err := Run(cmd, 5*time.Second); err != nil || out.String() != "ok" {
		t.Fatalf("Run = %v, output %q", err, out.String())
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("took %v; it waited for the child", d)
	}
}
