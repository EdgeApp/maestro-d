package daemon

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

// fakeProc points the /proc seams at a fake stat reader for the test.
func fakeProc(t *testing.T, goos string, stat func(pid int) ([]byte, error)) {
	t.Helper()
	oldOS, oldStat := procOS, procStat
	procOS, procStat = goos, stat
	t.Cleanup(func() { procOS, procStat = oldOS, oldStat })
}

// PIDAlive treats a Linux process in state Z or X as exited, though it
// still answers signal 0; any other state, or an unreadable or odd stat
// file, leaves signal 0's answer.
func TestPIDAlive_ProcState(t *testing.T) {
	self := os.Getpid()
	for _, tc := range []struct {
		name string
		stat string
		err  error
		want bool
	}{
		{"zombie", "4242 (maestro-d) Z 1 4242 4242 0 -1", nil, false},
		{"dead", "4242 (maestro-d) X 1", nil, false},
		{"running", "4242 (maestro-d) R 1 4242", nil, true},
		{"sleeping, comm with ') Z ('", "4242 (a) Z (b) S 1", nil, true},
		{"unreadable", "", errors.New("gone"), true},
		{"no comm", "4242 maestro-d Z", nil, true},
		{"nothing after comm", "4242 (maestro-d)", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var asked int
			fakeProc(t, "linux", func(pid int) ([]byte, error) {
				asked = pid
				return []byte(tc.stat), tc.err
			})
			if got := PIDAlive(self); got != tc.want {
				t.Fatalf("PIDAlive = %v, want %v", got, tc.want)
			}
			if asked != self {
				t.Fatalf("read stat of pid %d, want %d", asked, self)
			}
		})
	}
}

// Elsewhere (macOS) /proc is never read and signal 0 decides.
func TestPIDAlive_NotLinux(t *testing.T) {
	fakeProc(t, "darwin", func(int) ([]byte, error) {
		t.Fatal("read /proc off Linux")
		return nil, nil
	})
	if !PIDAlive(os.Getpid()) {
		t.Fatal("this process is not alive")
	}
	if PIDAlive(0) || PIDAlive(-1) {
		t.Fatal("pid <= 0 is alive")
	}
}

// A process that exited but is not reaped yet is gone for PIDAlive and
// WaitForExit, as in a container whose PID 1 reaps nothing.
func TestPIDAlive_RealZombie(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("zombie states come from Linux's /proc")
	}
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	defer func() { _ = cmd.Wait() }()
	if !WaitForExit(&Info{PID: pid}, 5*time.Second) {
		t.Fatalf("pid %d (exited, not reaped) still alive", pid)
	}
	if PIDAlive(pid) {
		t.Fatalf("PIDAlive(%d) for a zombie", pid)
	}
}
