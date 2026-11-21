package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
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

func TestServer_TrafficShaping(t *testing.T) {
	env := newTestEnv(t, 0)
	ss := newShaperServer(t)
	cfgPath := filepath.Join(Home(), "network.json")
	writeNetworkConfig(t, cfgPath, ss.URL)
	ctx := context.Background()

	info := env.attach("mock-1")
	if !info.TrafficShaping {
		t.Fatalf("attach answer: %+v", info)
	}
	env.attach("mock-2")

	res, err := env.c.Command(ctx, "mock-1", "setTrafficShape", map[string]any{"mbit": 1.5, "latencyMs": 100, "direction": "download"}, true)
	if err != nil || !res.OK {
		t.Fatalf("%v %+v", err, res)
	}
	if _, err := env.c.Command(ctx, "mock-1", "resetTrafficShape", nil, true); err != nil {
		t.Fatal(err)
	}
	want := []string{
		`POST /shape/mock-1 {"direction":"down","jitterMs":0,"latencyMs":100,"lossPct":0,"mbit":1.5}`,
		"DELETE /shape/mock-1",
	}
	if got := ss.calls(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// An unconfigured device: UNSUPPORTED, and no call.
	res, err = env.c.Command(ctx, "mock-2", "setTrafficShape", map[string]any{"lossPct": 100}, true)
	if codeOf(t, err) != CodeUnsupported || res.ExitCode != 2 || res.Error.Message != "no traffic shaper is configured for mock-2 on this host" {
		t.Fatalf("%v %+v", err, res)
	}
	if len(ss.calls()) != 2 {
		t.Fatalf("an unconfigured device made a call: %v", ss.calls())
	}

	// Listings: mock-1 only, in the list and on its own.
	devs, err := env.c.Devices(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	shaping := map[string]bool{}
	for _, d := range devs {
		shaping[d.ID] = d.TrafficShaping
	}
	if !shaping["mock-1"] || shaping["mock-2"] || len(shaping) < 2 {
		t.Fatalf("listing: %v", shaping)
	}
	if d, err := env.c.Device(ctx, "mock-1"); err != nil || !d.TrafficShaping {
		t.Fatalf("device: %v %+v", err, d)
	}

	// Bad values never reach the shaper.
	if _, err := env.c.Command(ctx, "mock-1", "setTrafficShape", map[string]any{"lossPct": 101}, true); codeOf(t, err) != CodeUsage {
		t.Fatalf("lossPct 101: %v", err)
	}

	// The file is read on every call: an edit takes effect, and a broken
	// file is USAGE naming it.
	if err := os.WriteFile(cfgPath, []byte(`{"devices":`), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err = env.c.Command(ctx, "mock-1", "setTrafficShape", nil, true)
	if codeOf(t, err) != CodeUsage || !strings.Contains(res.Error.Message, "network config "+cfgPath) {
		t.Fatalf("%v %+v", err, res)
	}
	if d, err := env.c.Device(ctx, "mock-1"); err != nil || d.TrafficShaping {
		t.Fatalf("a broken file still lists shaping: %v %+v", err, d)
	}
	if err := os.Remove(cfgPath); err != nil {
		t.Fatal(err)
	}
	if _, err := env.c.Command(ctx, "mock-1", "resetTrafficShape", nil, true); codeOf(t, err) != CodeUnsupported {
		t.Fatalf("no file: %v", err)
	}
	if len(ss.calls()) != 2 {
		t.Fatalf("calls: %v", ss.calls())
	}
}

func TestServer_NetworkConfigFlag(t *testing.T) {
	env := newTestEnv(t, 0)
	ss := newShaperServer(t)
	path := filepath.Join(t.TempDir(), "shapes.json")
	writeNetworkConfig(t, path, ss.URL)
	env.s.cfg.NetworkConfig = path
	if got := env.s.networkConfigPath(); got != path {
		t.Fatalf("path = %s", got)
	}
	env.attach("mock-1")
	if _, err := env.c.Command(context.Background(), "mock-1", "resetTrafficShape", nil, true); err != nil {
		t.Fatal(err)
	}
	if len(ss.calls()) != 1 {
		t.Fatalf("calls: %v", ss.calls())
	}
	env.s.markTrafficShaping(nil) // nothing to mark is not an error
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
	shape := func(t *testing.T, c *Client) {
		t.Helper()
		if _, err := c.Command(ctx, "mock-1", "setTrafficShape", map[string]any{"lossPct": 100}, true); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("detach", func(t *testing.T) {
		env := newTestEnv(t, 0)
		ss := newShaperServer(t)
		writeNetworkConfig(t, filepath.Join(Home(), "network.json"), ss.URL)
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
		writeNetworkConfig(t, filepath.Join(Home(), "network.json"), ss.URL)
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
		writeNetworkConfig(t, filepath.Join(Home(), "network.json"), ss.URL)
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
		writeNetworkConfig(t, filepath.Join(Home(), "network.json"), ss.URL)
		env.attach("mock-1")
		shape(t, env.c)
		env.s.detachAll()
		if n := countCalls(ss, resetCall); n != 1 {
			t.Fatalf("resets = %d", n)
		}
	})
	hub := func(t *testing.T) (*hubEnv, *shaperServer) {
		env := newHubEnv(t, Config{})
		ss := newShaperServer(t)
		writeNetworkConfig(t, filepath.Join(Home(), "network.json"), ss.URL)
		return env, ss
	}
	t.Run("lease release", func(t *testing.T) {
		env, ss := hub(t)
		res := env.lease(env.remote, LeaseRequest{Device: "mock-1", Attach: &AttachConfig{Platform: "mock"}})
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
		env.lease(env.remote, LeaseRequest{Device: "mock-1", TTLSec: 30, Attach: &AttachConfig{Platform: "mock"}})
		shape(t, env.remote)
		env.clock.advance(31 * time.Second)
		env.sweep()
		if n := countCalls(ss, resetCall); n != 1 {
			t.Fatalf("resets = %d", n)
		}
	})
	t.Run("hub shutdown", func(t *testing.T) {
		env, ss := hub(t)
		env.lease(env.remote, LeaseRequest{Device: "mock-1", Attach: &AttachConfig{Platform: "mock"}})
		shape(t, env.remote)
		env.s.Shutdown("test")
		waitFor(t, func() bool { return countCalls(ss, resetCall) == 1 })
	})
	// A lease that ends mid-step: the step unwinds (it honours its
	// context), then the teardown resets the shape and closes the driver.
	t.Run("lease expiry during a step", func(t *testing.T) {
		env, ss := hub(t)
		env.mu.Lock()
		env.stepDelay = 30 * time.Second
		env.mu.Unlock()
		env.lease(env.remote, LeaseRequest{Device: "mock-1", TTLSec: 5, Attach: &AttachConfig{Platform: "mock"}})
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

// A reset that never answers holds the teardown for its bound only.
func TestServer_SessionEndResetIsBounded(t *testing.T) {
	env := newTestEnv(t, 0)
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			select {
			case <-hang:
			case <-r.Context().Done():
			}
		}
	}))
	t.Cleanup(srv.Close)
	writeNetworkConfig(t, filepath.Join(Home(), "network.json"), srv.URL)
	env.attach("mock-1")
	if _, err := env.c.Command(context.Background(), "mock-1", "setTrafficShape", nil, true); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := env.c.Detach(context.Background(), "mock-1"); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(started); took < 3*time.Second || took > 8*time.Second {
		t.Fatalf("detach took %s; the reset's bound is 4 s", took)
	}
	if atomic.LoadInt32(&env.cleanups) != 1 {
		t.Fatalf("the driver was not closed: %d", env.cleanups)
	}
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
	record := func(s *Server) *airplaneDriver {
		d := &airplaneDriver{Driver: mock.New(mock.Config{Platform: "android", DeviceID: "mock-1"})}
		base := s.deps.NewDriver
		s.deps.NewDriver = func(ctx context.Context, id string, cfg AttachConfig) (core.Driver, DriverInfo, func(), error) {
			_, info, cleanup, err := base(ctx, id, cfg)
			return d, info, cleanup, err
		}
		return d
	}
	t.Run("detach", func(t *testing.T) {
		env := newTestEnv(t, 0)
		ss := newShaperServer(t)
		writeNetworkConfig(t, filepath.Join(Home(), "network.json"), ss.URL)
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
		env := newHubEnv(t, Config{})
		ss := newShaperServer(t)
		writeNetworkConfig(t, filepath.Join(Home(), "network.json"), ss.URL)
		d := record(env.s)
		env.lease(env.remote, LeaseRequest{Device: "mock-1", Attach: &AttachConfig{Platform: "mock"}})
		if _, err := env.remote.Command(ctx, "mock-1", "setAirplaneMode", "enabled", true); err != nil {
			t.Fatal(err)
		}
		if _, err := env.remote.Command(ctx, "mock-1", "setTrafficShape", map[string]any{"lossPct": 100}, true); err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		env.s.Shutdown("test")
		// The process exits when done closes: after the lease teardowns
		// finish, or leaseShutdownWait runs out beside them. Both undos must
		// be done by then.
		select {
		case <-env.s.done:
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

// A listing logs an unusable config once per problem, not once per poll.
func TestWarnShapingOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "network.json")
	if !warnShapingOnce(path, "mode 0644") || warnShapingOnce(path, "mode 0644") {
		t.Fatal("the same problem logged twice")
	}
	if !warnShapingOnce(path, "invalid JSON") {
		t.Fatal("a new problem was not logged")
	}
	shapingWarned.Delete(path) // the file loaded again
	if !warnShapingOnce(path, "invalid JSON") {
		t.Fatal("a problem after a good load was not logged")
	}
	shapingWarned.Delete(path)
}

// A new attach of a device waits for the previous session's teardown, so
// that session's reset never lands after the new session's shape.
func TestServer_AttachWaitsForTheDetachCleanup(t *testing.T) {
	env := newTestEnv(t, 0)
	var mu sync.Mutex
	var events []string
	resetStarted := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			resetStarted <- struct{}{}
			time.Sleep(500 * time.Millisecond)
		}
		mu.Lock()
		events = append(events, r.Method+" done")
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	writeNetworkConfig(t, filepath.Join(Home(), "network.json"), srv.URL)
	ctx := context.Background()
	env.attach("mock-1")
	if _, err := env.c.Command(ctx, "mock-1", "setTrafficShape", map[string]any{"lossPct": 100}, true); err != nil {
		t.Fatal(err)
	}
	detached := make(chan error, 1)
	go func() { detached <- env.c.Detach(ctx, "mock-1") }()
	<-resetStarted
	// The old session's reset is in flight: attach again and shape.
	env.attach("mock-1")
	if _, err := env.c.Command(ctx, "mock-1", "setTrafficShape", map[string]any{"latencyMs": 50}, true); err != nil {
		t.Fatal(err)
	}
	if err := <-detached; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := strings.Join(events, ", ")
	mu.Unlock()
	if got != "POST done, DELETE done, POST done" {
		t.Fatalf("shaper saw %s; the new shape must follow the old reset", got)
	}
}

// A teardown that panics still releases the device's reservation, so it
// can be attached again.
func TestServer_DetachPanicReleasesTheDevice(t *testing.T) {
	env := newTestEnv(t, 0)
	env.attach("mock-1")
	env.s.mu.Lock()
	a := env.s.attached["mock-1"]
	env.s.mu.Unlock()
	a.release = func() { panic("release") }
	func() {
		defer func() { _ = recover() }()
		env.s.detachForStop("mock-1")
	}()
	env.s.mu.Lock()
	_, reserved := env.s.attaching["mock-1"]
	env.s.mu.Unlock()
	if reserved {
		t.Fatal("the device stayed reserved after a panicking teardown")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := env.c.Attach(ctx, "mock-1", AttachConfig{Platform: "mock"}); err != nil {
		t.Fatalf("re-attach: %v", err)
	}
}
