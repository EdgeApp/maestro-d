package hubd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/driver/mock"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

// shaperServer records every call a daemon's traffic shaper makes.
type shaperServer struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []string // "METHOD path body"
}

func newShaperServer(t *testing.T) *shaperServer {
	t.Helper()
	ss := &shaperServer{}
	ss.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ss.mu.Lock()
		ss.reqs = append(ss.reqs, strings.TrimSpace(r.Method+" "+r.URL.Path+" "+string(body)))
		ss.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ss.Close)
	return ss
}

func (ss *shaperServer) calls() []string {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return append([]string(nil), ss.reqs...)
}

// writeNetworkConfig configures mock-1 (only) against the shaper server.
func writeNetworkConfig(t *testing.T, path, base string) {
	t.Helper()
	cfg := map[string]any{"devices": map[string]any{"mock-1": map[string]any{
		"setTrafficShape": map[string]any{"method": "POST", "url": base + "/shape/${deviceId}",
			"body":            map[string]any{"mbit": "${mbit}", "lossPct": "${lossPct}", "latencyMs": "${latencyMs}", "jitterMs": "${jitterMs}", "direction": "${direction}"},
			"directionValues": map[string]string{"download": "down", "upload": "up"}},
		"resetTrafficShape": map[string]any{"method": "DELETE", "url": base + "/shape/${deviceId}"},
	}}}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

const resetCall = "DELETE /shape/mock-1"

func countCalls(ss *shaperServer, call string) int {
	n := 0
	for _, c := range ss.calls() {
		if c == call {
			n++
		}
	}
	return n
}

// Every way a session ends sends the shaped device's reset exactly once.
func TestServer_SessionEndResetsTheShape(t *testing.T) {
	ctx := context.Background()
	shape := func(t *testing.T, c *daemon.Client) {
		t.Helper()
		if _, err := c.Command(ctx, "mock-1", "setTrafficShape", map[string]any{"lossPct": 100}, true); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("detach", func(t *testing.T) {
		env := newTestEnv(t, 0)
		ss := newShaperServer(t)
		writeNetworkConfig(t, filepath.Join(daemon.Home(), "network.json"), ss.URL)
		env.attach("mock-1")
		shape(t, env.c)
		if err := env.c.Detach(ctx, "mock-1"); err != nil {
			t.Fatal(err)
		}
		if n := countCalls(ss, resetCall); n != 1 {
			t.Fatalf("resets = %d (%v)", n, ss.calls())
		}
	})
	t.Run("a session that reset last sends nothing more", func(t *testing.T) {
		env := newTestEnv(t, 0)
		ss := newShaperServer(t)
		writeNetworkConfig(t, filepath.Join(daemon.Home(), "network.json"), ss.URL)
		env.attach("mock-1")
		shape(t, env.c)
		if _, err := env.c.Command(ctx, "mock-1", "resetTrafficShape", nil, true); err != nil {
			t.Fatal(err)
		}
		if err := env.c.Detach(ctx, "mock-1"); err != nil {
			t.Fatal(err)
		}
		if n := countCalls(ss, resetCall); n != 1 {
			t.Fatalf("resets = %d", n)
		}
	})
	t.Run("an unshaped session sends nothing", func(t *testing.T) {
		env := newTestEnv(t, 0)
		ss := newShaperServer(t)
		writeNetworkConfig(t, filepath.Join(daemon.Home(), "network.json"), ss.URL)
		env.attach("mock-1")
		if err := env.c.Detach(ctx, "mock-1"); err != nil {
			t.Fatal(err)
		}
		if len(ss.calls()) != 0 {
			t.Fatalf("calls: %v", ss.calls())
		}
	})
	t.Run("daemon stop", func(t *testing.T) {
		env := newTestEnv(t, 0)
		ss := newShaperServer(t)
		writeNetworkConfig(t, filepath.Join(daemon.Home(), "network.json"), ss.URL)
		env.attach("mock-1")
		shape(t, env.c)
		env.s.close()
		if n := countCalls(ss, resetCall); n != 1 {
			t.Fatalf("resets = %d", n)
		}
	})
	hub := func(t *testing.T) (*hubEnv, *shaperServer) {
		env := newHubEnv(t, daemon.Config{})
		ss := newShaperServer(t)
		writeNetworkConfig(t, filepath.Join(daemon.Home(), "network.json"), ss.URL)
		return env, ss
	}
	t.Run("lease release", func(t *testing.T) {
		env, ss := hub(t)
		res := env.lease(env.remote, daemon.LeaseRequest{Device: "mock-1", Attach: &daemon.AttachConfig{Platform: "mock"}})
		shape(t, env.remote)
		if _, err := env.remote.ReleaseLease(ctx, res.Lease.ID); err != nil {
			t.Fatal(err)
		}
		if n := countCalls(ss, resetCall); n != 1 {
			t.Fatalf("resets = %d", n)
		}
	})
	t.Run("lease expiry", func(t *testing.T) {
		env, ss := hub(t)
		env.lease(env.remote, daemon.LeaseRequest{Device: "mock-1", TTLSec: 30, Attach: &daemon.AttachConfig{Platform: "mock"}})
		shape(t, env.remote)
		env.clock.advance(31 * time.Second)
		env.sweep()
		if n := countCalls(ss, resetCall); n != 1 {
			t.Fatalf("resets = %d", n)
		}
	})
	t.Run("hub shutdown", func(t *testing.T) {
		env, ss := hub(t)
		env.lease(env.remote, daemon.LeaseRequest{Device: "mock-1", Attach: &daemon.AttachConfig{Platform: "mock"}})
		shape(t, env.remote)
		env.s.Server.Server.Shutdown("test")
		waitFor(t, func() bool { return countCalls(ss, resetCall) == 1 })
	})
	// A lease that ends mid-step: the step unwinds (it honours its
	// context), then the teardown resets the shape and closes the driver.
	t.Run("lease expiry during a step", func(t *testing.T) {
		env, ss := hub(t)
		env.mu.Lock()
		env.stepDelay = 30 * time.Second
		env.mu.Unlock()
		env.lease(env.remote, daemon.LeaseRequest{Device: "mock-1", TTLSec: 5, Attach: &daemon.AttachConfig{Platform: "mock"}})
		shape(t, env.remote)
		done := make(chan error, 1)
		go func() {
			_, err := env.remote.Command(ctx, "mock-1", "tapOn", "slow", true)
			done <- err
		}()
		waitFor(t, func() bool {
			devs, _ := env.local.Devices(ctx, "mock")
			return len(devs) > 0 && devs[0].Busy
		})
		env.clock.advance(6 * time.Second)
		env.s.sweepLeases()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the step outlived its lease")
		}
		waitFor(t, func() bool { return countCalls(ss, resetCall) == 1 && atomic.LoadInt32(&env.cleanups) == 1 })
	})
}

// airplaneDriver records the airplane-mode values a device was set to.
type airplaneDriver struct {
	*mock.Driver
	mu   sync.Mutex
	seen []bool
}

func (d *airplaneDriver) Execute(step flow.Step) *core.CommandResult {
	if s, ok := step.(*flow.SetAirplaneModeStep); ok {
		d.mu.Lock()
		d.seen = append(d.seen, s.Enabled)
		d.mu.Unlock()
	}
	return d.Driver.Execute(step)
}

func (d *airplaneDriver) got() []bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]bool(nil), d.seen...)
}

// The session that turned airplane mode on turns it off as it ends, and
// sends the reset, through the daemon's own teardown: on detach, and on a
// hub shutdown within its lease-shutdown wait.
func TestServer_SessionEndTurnsAirplaneModeOff(t *testing.T) {
	ctx := context.Background()
	record := func(s *fixture) *airplaneDriver {
		d := &airplaneDriver{Driver: mock.New(mock.Config{Platform: "android", DeviceID: "mock-1"})}
		base := s.deps.NewDriver
		s.deps.NewDriver = func(ctx context.Context, id string, cfg daemon.AttachConfig) (core.Driver, daemon.DriverInfo, func(), error) {
			_, info, cleanup, err := base(ctx, id, cfg)
			return d, info, cleanup, err
		}
		return d
	}
	t.Run("detach", func(t *testing.T) {
		env := newTestEnv(t, 0)
		ss := newShaperServer(t)
		writeNetworkConfig(t, filepath.Join(daemon.Home(), "network.json"), ss.URL)
		d := record(env.s)
		env.attach("mock-1")
		if _, err := env.c.Command(ctx, "mock-1", "setAirplaneMode", "enabled", true); err != nil {
			t.Fatal(err)
		}
		if err := env.c.Detach(ctx, "mock-1"); err != nil {
			t.Fatal(err)
		}
		if got := d.got(); len(got) != 2 || !got[0] || got[1] {
			t.Fatalf("airplane values: %v", got)
		}
		if len(ss.calls()) != 0 {
			t.Fatalf("an unshaped session reset: %v", ss.calls())
		}
	})
	t.Run("untouched", func(t *testing.T) {
		env := newTestEnv(t, 0)
		d := record(env.s)
		env.attach("mock-1")
		if err := env.c.Detach(ctx, "mock-1"); err != nil {
			t.Fatal(err)
		}
		if got := d.got(); len(got) != 0 {
			t.Fatalf("airplane values: %v", got)
		}
	})
	t.Run("hub shutdown", func(t *testing.T) {
		env := newHubEnv(t, daemon.Config{})
		ss := newShaperServer(t)
		writeNetworkConfig(t, filepath.Join(daemon.Home(), "network.json"), ss.URL)
		d := record(env.s)
		env.lease(env.remote, daemon.LeaseRequest{Device: "mock-1", Attach: &daemon.AttachConfig{Platform: "mock"}})
		if _, err := env.remote.Command(ctx, "mock-1", "setAirplaneMode", "enabled", true); err != nil {
			t.Fatal(err)
		}
		if _, err := env.remote.Command(ctx, "mock-1", "setTrafficShape", map[string]any{"lossPct": 100}, true); err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		env.s.Server.Server.Shutdown("test")
		// The process exits when done closes: after the lease teardowns
		// finish, or leaseShutdownWait runs out beside them. Both undos must
		// be done by then.
		select {
		case <-env.s.Done():
		case <-time.After(2 * leaseShutdownWait):
			t.Fatal("the hub did not stop")
		}
		if took := time.Since(started); took > leaseShutdownWait {
			t.Fatalf("shutdown took %s", took)
		}
		if got := d.got(); len(got) != 2 || got[1] {
			t.Fatalf("airplane values: %v", got)
		}
		if n := countCalls(ss, resetCall); n != 1 {
			t.Fatalf("resets = %d", n)
		}
	})
}

// hubDevicesJSON is config.json for devices given by id: each entry's
// setTrafficShape and resetTrafficShape go under trafficShaping.
func hubDevicesJSON(t *testing.T, devices map[string]any) []byte {
	t.Helper()
	ids := make([]string, 0, len(devices))
	for id := range devices {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	list := []map[string]any{}
	for _, id := range ids {
		e := map[string]any{"id": id}
		shaping := map[string]any{}
		for k, v := range devices[id].(map[string]any) {
			if k == "setTrafficShape" || k == "resetTrafficShape" {
				shaping[k] = v
			} else {
				e[k] = v
			}
		}
		if len(shaping) > 0 {
			e["trafficShaping"] = shaping
		}
		list = append(list, e)
	}
	data, err := json.Marshal(map[string]any{"hubDevices": list})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// writeDescriptions writes a config.json in this daemon's home that only
// describes devices.
func writeDescriptions(t *testing.T, descriptions map[string]string) {
	t.Helper()
	devices := map[string]any{}
	for id, d := range descriptions {
		devices[id] = map[string]any{"description": d}
	}
	if err := os.WriteFile(filepath.Join(daemon.Home(), "config.json"), hubDevicesJSON(t, devices), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A lease's answer, attached or not, carries the device's description.
func TestLeases_AnswerCarriesTheDescription(t *testing.T) {
	env := newHubEnv(t, daemon.Config{})
	writeDescriptions(t, map[string]string{"mock-1": "Lab phone A", "mock-2": "Lab phone B"})
	res := env.lease(env.remote, daemon.LeaseRequest{Device: "mock-1", TTLSec: 30, Attach: &daemon.AttachConfig{Platform: "mock"}})
	if res.Device == nil || res.Device.Description != "Lab phone A" || !res.Device.Attached {
		t.Fatalf("attached lease answer: %+v", res.Device)
	}
	res = env.lease(env.remote, daemon.LeaseRequest{Device: "mock-2", TTLSec: 30})
	if res.Device == nil || res.Device.Description != "Lab phone B" {
		t.Fatalf("lease answer: %+v", res.Device)
	}
}
