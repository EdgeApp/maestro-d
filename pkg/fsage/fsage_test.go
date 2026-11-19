package fsage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func write(t *testing.T, p string, ago time.Duration) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-ago)
	_ = os.Chtimes(p, old, old)
}

func TestNewestIdleForAndSize(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a", "old"), 10*24*time.Hour)
	write(t, filepath.Join(dir, "daemon.lock"), 0) // a probed lock is not use
	if d := time.Since(Newest(dir)); d < 9*24*time.Hour {
		t.Errorf("Newest = %v ago, want ~10 days (dirs and locks ignored)", d)
	}
	if !IdleFor(dir, 7*24*time.Hour) || IdleFor(dir, 11*24*time.Hour) {
		t.Error("IdleFor wrong around a 10-day-old file")
	}
	write(t, filepath.Join(dir, "b", "new"), 0)
	if IdleFor(dir, time.Hour) {
		t.Error("a fresh file did not make the tree busy")
	}
	if Size(dir) != 15 {
		t.Errorf("Size = %d, want 15", Size(dir))
	}

	empty := t.TempDir()
	old := time.Now().Add(-48 * time.Hour)
	_ = os.Chtimes(empty, old, old)
	if !IdleFor(empty, 24*time.Hour) || Newest(empty).IsZero() {
		t.Error("an empty dir should fall back to its own time")
	}
	missing := filepath.Join(dir, "missing")
	if !Newest(missing).IsZero() || IdleFor(missing, time.Hour) || Size(missing) != 0 {
		t.Error("a missing path should be zero, not idle, size 0")
	}
}

func TestOnAFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "f")
	write(t, f, 48*time.Hour)
	if !IdleFor(f, 24*time.Hour) || IdleFor(f, 72*time.Hour) || Size(f) != 5 {
		t.Error("a single file's age or size is wrong")
	}
	if d := time.Since(Newest(f)); d < 47*time.Hour {
		t.Errorf("Newest of a file = %v ago", d)
	}
}
