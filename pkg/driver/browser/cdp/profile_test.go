package cdp

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/launcher/flags"
)

type fakeLauncher struct {
	calls   []string
	dir     string
	blockCh chan struct{} // Cleanup waits on it when set
}

func (f *fakeLauncher) Kill() { f.calls = append(f.calls, "kill") }
func (f *fakeLauncher) Cleanup() {
	f.calls = append(f.calls, "cleanup")
	if f.blockCh != nil {
		<-f.blockCh
	}
}
func (f *fakeLauncher) Get(name flags.Flag) string {
	if name == flags.UserDataDir {
		return f.dir
	}
	return ""
}

func profileDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "rod", "user-data", "abc")
	if err := os.MkdirAll(filepath.Join(dir, "Default"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCloseRemovesOnlyItsOwnProfile(t *testing.T) {
	owned := &fakeLauncher{dir: profileDir(t)}
	d := &Driver{stopCh: make(chan struct{}), launcher: owned, ownsProfile: true}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	_ = d.Close() // a second Close is a no-op
	if len(owned.calls) != 2 || owned.calls[0] != "kill" || owned.calls[1] != "cleanup" {
		t.Errorf("owned profile calls = %v, want [kill cleanup] once", owned.calls)
	}
	if _, err := os.Stat(owned.dir); !os.IsNotExist(err) {
		t.Error("the owned profile survived Close")
	}

	callers := &fakeLauncher{dir: profileDir(t)}
	d = &Driver{stopCh: make(chan struct{}), launcher: callers, ownsProfile: false}
	_ = d.Close()
	if len(callers.calls) != 0 {
		t.Errorf("a caller's UserDataDir launcher got %v", callers.calls)
	}
	if _, err := os.Stat(callers.dir); err != nil {
		t.Error("a caller's profile was removed")
	}

	removeProfile(nil, true) // no launcher: no-op
}

func TestRemoveProfileDoesNotHangOnCleanup(t *testing.T) {
	prev := cleanupWait
	cleanupWait = 50 * time.Millisecond
	t.Cleanup(func() { cleanupWait = prev })
	stuck := &fakeLauncher{dir: profileDir(t), blockCh: make(chan struct{})}
	defer close(stuck.blockCh)

	start := time.Now()
	removeProfile(stuck, true)
	if time.Since(start) > 2*time.Second {
		t.Fatal("removeProfile waited on a Cleanup that never returns")
	}
	if _, err := os.Stat(stuck.dir); !os.IsNotExist(err) {
		t.Error("the profile survived a stuck Cleanup")
	}
}

func TestNewWithBadBinaryRemovesTheProfileWithoutHanging(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	done := make(chan error, 1)
	go func() {
		_, err := New(Config{Headless: true, ChromeBin: filepath.Join(tmp, "no-such-chrome"), ViewportW: 800, ViewportH: 600})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("New succeeded with a missing binary")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("New hung after a failed launch")
	}
	if entries, _ := os.ReadDir(filepath.Join(tmp, "rod", "user-data")); len(entries) != 0 {
		t.Errorf("failed launch left %d profile(s)", len(entries))
	}
}
