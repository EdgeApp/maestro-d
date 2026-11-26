package hubd

import (
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/internal/daemontest"
	"github.com/devicelab-dev/maestro-runner/pkg/netshape"
)

func TestConfigCacheIdentity(t *testing.T) {
	path := daemontest.DeviceConfig(t, "One")
	loads, changes := 0, 0
	c := newConfigCache(path, nil, func(old, current *netshape.Config) { changes++ })
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
	c := newConfigCache(path, nil, func(*netshape.Config, *netshape.Config) {})
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

// A forced refresh of an unchanged file keeps its snapshot without a
// transition. A change resets derived state under the config lock, then runs
// its callback with that lock released, and transitions publish in order.
func TestConfigCacheTransitions(t *testing.T) {
	path := daemontest.DeviceConfig(t, "mock-1")
	var resets int
	var seen [][]string
	var c *configCache
	c = newConfigCache(path, func() {
		if c.mu.TryLock() {
			t.Error("reset ran without the config lock")
		}
		resets++
	}, func(old, current *netshape.Config) {
		if !c.mu.TryLock() {
			t.Error("callback ran under the config lock")
		} else {
			c.mu.Unlock()
		}
		// A reader is never blocked by the callback's device discovery.
		if _, err := c.snapshot(time.Now(), false); err != nil {
			t.Error(err)
		}
		seen = append(seen, []string{strings.Join(old.Shared, ","), strings.Join(current.Shared, ",")})
	})
	first, err := c.snapshot(time.Now(), true)
	if err != nil {
		t.Fatal(err)
	}
	daemontest.WriteDevices(t, path, "mock-1")
	if cfg, err := c.snapshot(time.Now(), true); err != nil || cfg != first || resets != 0 || len(seen) != 0 {
		t.Fatalf("unchanged refresh: %v %v resets %d %v", cfg, err, resets, seen)
	}
	for _, ids := range []string{"mock-2", "mock-3"} {
		if _, err := c.edit(func() error { daemontest.WriteDevices(t, path, ids); return nil }, c.changed); err != nil {
			t.Fatal(err)
		}
	}
	if resets != 2 || !slices.Equal(seen[0], []string{"mock-1", "mock-2"}) || !slices.Equal(seen[1], []string{"mock-2", "mock-3"}) {
		t.Fatalf("resets %d, transitions %v", resets, seen)
	}
	if _, err := c.edit(func() error { return os.ErrPermission }, c.changed); err != os.ErrPermission || !c.mu.TryLock() {
		t.Fatalf("failed edit: %v", err)
	}
	c.mu.Unlock()
}

// Overlapping transitions queue for their callbacks without holding the
// config lock: while the first callback is still discovering devices and a
// second waits its turn, a reader gets the config at once, and the callbacks
// still run in transition order.
func TestConfigCacheOverlappingTransitionsNeverBlockReaders(t *testing.T) {
	path := daemontest.DeviceConfig(t, "mock-1")
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	started := make(chan string, 2)
	var mu sync.Mutex
	var order []string
	c := newConfigCache(path, nil, func(old, current *netshape.Config) {
		id := current.Shared[0]
		started <- id
		if id == "mock-2" {
			<-release // the first transition's discovery is slow
		}
		mu.Lock()
		order = append(order, id)
		mu.Unlock()
	})
	if _, err := c.snapshot(time.Now(), true); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{}, 2)
	edit := func(id string) {
		if _, err := c.edit(func() error { daemontest.WriteDevices(t, path, id); return nil }, c.changed); err != nil {
			t.Error(err)
		}
		done <- struct{}{}
	}
	go edit("mock-2")
	if got := <-started; got != "mock-2" {
		t.Fatal(got)
	}
	go edit("mock-3")
	// The second transition has taken its ticket once the snapshot shows
	// mock-3. The config lock must stay free while it waits for its turn.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if c.mu.TryLock() {
			shared := c.config.Shared
			c.mu.Unlock()
			if slices.Equal(shared, []string{"mock-3"}) {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the config lock stayed held behind an earlier callback")
		}
	}
	read := make(chan []string)
	go func() {
		cfg, _ := c.snapshot(time.Now(), true)
		read <- cfg.Shared
	}()
	select {
	case got := <-read:
		if !slices.Equal(got, []string{"mock-3"}) {
			t.Fatal(got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a reader waited behind a callback's discovery")
	}
	select {
	case id := <-started:
		t.Fatalf("%s's callback ran before the earlier transition's finished", id)
	case <-time.After(50 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	<-done
	<-done
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(order, []string{"mock-2", "mock-3"}) {
		t.Fatal(order)
	}
}
