package hubd

import (
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/internal/daemontest"
	"github.com/devicelab-dev/maestro-runner/pkg/netshape"
)

func TestConfigCacheIdentity(t *testing.T) {
	path := daemontest.DeviceConfig(t, "One")
	loads, changes := 0, 0
	c := newConfigCache(path, func(old, current *netshape.Config) { changes++ })
	c.load = func(path string) (*netshape.Config, error) { loads++; return netshape.LoadConfig(path) }
	now := time.Now()
	check := func(id string, n int) {
		t.Helper()
		cfg, err := c.snapshot(now, false)
		if err != nil || loads != n || !slices.Equal(cfg.Shared, []string{id}) {
			t.Fatalf("snapshot %v, %v, loads %d", cfg, err, loads)
		}
	}
	check("One", 1)
	now = now.Add(time.Second)
	check("One", 1)
	// Atomic replacement with the same length and timestamp changes only the inode.
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	temp := path + ".new"
	daemontest.WriteDevices(t, temp, "Two")
	if err := os.Chtimes(temp, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(temp, path); err != nil {
		t.Fatal(err)
	}
	check("One", 1)
	now = now.Add(time.Second)
	if runtime.GOOS == "windows" {
		check("One", 1)
		if err := os.Chtimes(path, now, now); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Second)
	}
	check("Two", 2)
	now = now.Add(time.Second)
	check("Two", 2)
	if changes != 1 {
		t.Fatal(changes)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	cfg, err := c.snapshot(now, true)
	if err != nil || len(cfg.Shared) != 0 {
		t.Fatalf("removed file: %v %v", cfg, err)
	}
}

func TestConfigCacheLastGood(t *testing.T) {
	path := daemontest.DeviceConfig(t, "mock-1")
	c := newConfigCache(path, func(*netshape.Config, *netshape.Config) {})
	var warnings []string
	c.warn = func(f string, a ...any) { warnings = append(warnings, fmt.Sprintf(f, a...)) }
	now := time.Now()
	first, err := c.snapshot(now, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"{", "{", "{x", "{x", "{", `{"hubDevices":[{"id":"mock-2","description":"` + "\xff" + `"}]}`} {
		if err := os.WriteFile(path, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := c.snapshot(now, true)
		if cfg != first || err == nil || c.configError != err {
			t.Fatalf("invalid edit: %v %v", cfg, err)
		}
	}
	if len(warnings) != 3 || !strings.Contains(warnings[0], "still sharing [mock-1]") {
		t.Fatal(warnings)
	}
	daemontest.WriteDevices(t, path, "mock-2")
	cfg, err := c.snapshot(now, true)
	if err != nil || c.configError != nil || cfg.Shared[0] != "mock-2" {
		t.Fatalf("repaired edit: %v %v", cfg, err)
	}
}
