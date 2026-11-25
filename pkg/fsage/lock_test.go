package fsage

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLockFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "leases.lock")
	f, err := LockFile(path)
	if err != nil {
		t.Fatal(err)
	}
	UnlockFile(f)
	if _, err := f.Stat(); err == nil {
		t.Error("unlock left the file open")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if _, err := LockFile(filepath.Join(dir, "missing", "leases.lock")); err == nil {
		t.Fatal("locked a file in a missing directory")
	}
}

func TestLockContendedAcrossProcesses(t *testing.T) {
	const helper = "FSAGE_TEST_CHILD_LOCK"
	if path := os.Getenv(helper); path != "" {
		f, err := LockFile(path)
		if err != nil {
			t.Fatal(err)
		}
		defer UnlockFile(f)
		fmt.Fprintln(os.Stdout, "locked")
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}
	path := filepath.Join(t.TempDir(), "hub.lock")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLockContendedAcrossProcesses$")
	cmd.Env = append(os.Environ(), helper+"="+path)
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || strings.TrimSpace(line) != "locked" {
		t.Fatalf("child lock: %q, %v", line, err)
	}
	if f, err := TryLockFile(path); !errors.Is(err, ErrLocked) {
		if f != nil {
			UnlockFile(f)
		}
		t.Fatalf("contended lock: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("killed child exited successfully")
	}
	f, err := TryLockFile(path)
	if err != nil {
		t.Fatalf("lock after child exited: %v", err)
	}
	UnlockFile(f)
}
