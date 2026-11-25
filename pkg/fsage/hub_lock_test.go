package fsage

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// This subprocess test runs in the Windows lock job as well as the Unix suite.
func TestHubSingletonAcrossProcesses(t *testing.T) {
	const key = "MAESTRO_TEST_SINGLETON_HOME"
	if home := os.Getenv(key); home != "" {
		lock, err := TryHubLock(home)
		if err != nil {
			t.Fatal(err)
		}
		defer UnlockFile(lock)
		fmt.Println("ready")
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}
	home := t.TempDir()
	for _, crash := range []bool{false, true} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHubSingletonAcrossProcesses$")
		cmd.Env = append(os.Environ(), key+"="+home)
		input, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err = cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if cmd.ProcessState == nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
		})
		if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || strings.TrimSpace(line) != "ready" {
			t.Fatalf("child: %q %v", line, err)
		}
		if f, err := TryHubLock(home); !errors.Is(err, ErrLocked) {
			if f != nil {
				UnlockFile(f)
			}
			t.Fatalf("second hub: %v", err)
		}
		other, err := TryHubLock(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		UnlockFile(other)
		if crash {
			if err = cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
		}
		_ = input.Close()
		err = cmd.Wait()
		if (err != nil) != crash {
			t.Fatal(err)
		}
	}
	f, err := TryHubLock(home)
	if err != nil {
		t.Fatal(err)
	}
	UnlockFile(f)
}
