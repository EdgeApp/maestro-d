package daemon

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/driver/mock"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
	"github.com/devicelab-dev/maestro-runner/pkg/report"
)

// fakeClock is a settable clock for the lease table.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// hubEnv is a hub daemon with listed devices: two ready mocks, a booted
// "simulator" and a shut-down one, plus clients for the unix socket and a
// remote TCP caller.
type hubEnv struct {
	t        *testing.T
	s        *Server
	local    *Client
	remote   *Client
	clock    *fakeClock
	cleanups int32
	// booted lists the shut-down simulators the boot seam has started.
	mu      sync.Mutex
	booted  map[string]bool
	stopped []string
	// emuAVD, when set, lists a running emulator-5554 running that AVD;
	// downAVD a shut-down AVD of that name.
	emuAVD, downAVD string
	// lists counts live discoveries; onList, when set, runs in each.
	lists  atomic.Int32
	onList func()
	// stepDelay slows each mock step; onAttach, when set, runs in each
	// driver start.
	stepDelay time.Duration
	onAttach  func()
	// onStep, when set as the driver starts, runs in each of its steps and
	// ignores the step's context (as a WebDriverAgent HTTP call does).
	onStep func()
}

func newHubEnv(t *testing.T, cfg Config) *hubEnv {
	t.Helper()
	t.Setenv(EnvHome, t.TempDir())
	env := &hubEnv{t: t, clock: &fakeClock{t: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}, booted: map[string]bool{}}
	deps := testDeps(&env.cleanups, 0)
	deps.NewDriver = func(ctx context.Context, id string, cfg AttachConfig) (core.Driver, DriverInfo, func(), error) {
		env.mu.Lock()
		f, delay, onStep := env.onAttach, env.stepDelay, env.onStep
		env.mu.Unlock()
		if f != nil {
			f()
		}
		var d core.Driver = mock.New(mock.Config{Platform: "mock", DeviceID: id})
		if delay > 0 {
			d = &slowDriver{Driver: d.(*mock.Driver), delay: delay}
		}
		if onStep != nil {
			d = &stubbornDriver{Driver: d, onStep: onStep}
		}
		return d, DriverInfo{DriverName: "mock", Device: report.Device{ID: id}, Platform: "mock", Kind: "device"},
			func() { atomic.AddInt32(&env.cleanups, 1) }, nil
	}
	deps.ListDevices = func(platform string) []DeviceInfo {
		env.lists.Add(1)
		env.mu.Lock()
		if f := env.onList; f != nil {
			env.mu.Unlock()
			f()
			env.mu.Lock()
		}
		defer env.mu.Unlock()
		all := []DeviceInfo{
			{ID: "mock-1", Platform: "mock", Kind: "device", Name: "Mock One", State: "device", Ready: true},
			{ID: "mock-2", Platform: "mock", Kind: "device", Name: "Mock Two", State: "device", Ready: true},
			{ID: "SIM-UP", Platform: "ios", Kind: "simulator", Name: "Shared Phone", OSVersion: "26.5", State: "Booted", Ready: true},
		}
		down := DeviceInfo{ID: "SIM-DOWN", Platform: "ios", Kind: "simulator", Name: "Spare Phone", OSVersion: "26.5", State: "Shutdown"}
		if env.booted["SIM-DOWN"] {
			down.State, down.Ready = "Booted", true
		}
		all = append(all, down)
		if env.downAVD != "" {
			all = append(all, DeviceInfo{ID: AVDPrefix + env.downAVD, Platform: "android", Kind: "emulator", Name: env.downAVD, State: "Shutdown"})
		}
		if env.emuAVD != "" {
			all = append(all, DeviceInfo{ID: "emulator-5554", Platform: "android", Kind: "emulator", Name: env.emuAVD, State: "device", Ready: true})
		}
		var out []DeviceInfo
		for _, d := range all {
			if platform == "" || d.Platform == platform {
				out = append(out, d)
			}
		}
		return out
	}
	if cfg.Name == "" {
		cfg.Name = "hubtest"
	}
	cfg.Hub, cfg.Token, cfg.HubName = true, "t", "hubtest"
	if cfg.HTTPAddr == "" {
		cfg.HTTPAddr = ":7433"
	}
	cfg.ReportsDir = filepath.Join(t.TempDir(), "reports")
	env.s = New(cfg, deps)
	env.s.info = Info{Name: cfg.Name, PID: os.Getpid(), APIVersion: APIVersion, Version: "test", Hub: "hubtest"}
	env.s.leases.now = env.clock.now
	env.s.registry.boot = func(platform, name string, timeout time.Duration) (string, error) {
		env.mu.Lock()
		env.booted[name] = true
		env.mu.Unlock()
		return name, nil
	}
	env.s.registry.shutdown = func(d DeviceInfo) error {
		env.mu.Lock()
		env.stopped = append(env.stopped, d.ID)
		delete(env.booted, d.ID)
		env.mu.Unlock()
		return nil
	}
	t.Cleanup(env.s.detachAll)
	mk := func(h http.Handler) *Client {
		hs := httptest.NewServer(h)
		t.Cleanup(hs.Close)
		return NewHTTPClient(hs.URL, "t", nil)
	}
	env.local = mk(env.s.Handler())
	env.remote = mk(fromAddr("192.0.2.20:40000", env.s.TCPHandler()))
	return env
}

// slowDriver is the mock driver with steps that take delay and stop
// early, failing, when their context ends (as a real driver's polling
// does).
type slowDriver struct {
	*mock.Driver
	mu    sync.Mutex
	ctx   context.Context
	delay time.Duration
}

func (d *slowDriver) SetContext(ctx context.Context) {
	d.mu.Lock()
	d.ctx = ctx
	d.mu.Unlock()
}

func (d *slowDriver) Execute(step flow.Step) *core.CommandResult {
	d.mu.Lock()
	ctx := d.ctx
	d.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-time.After(d.delay):
		return d.Driver.Execute(step)
	case <-ctx.Done():
		return &core.CommandResult{Success: false, Error: ctx.Err(), Message: "cancelled"}
	}
}

// stubbornDriver runs onStep in each step and never looks at the step's
// context: the step returns only when onStep does.
type stubbornDriver struct {
	core.Driver
	onStep func()
}

func (d *stubbornDriver) Execute(step flow.Step) *core.CommandResult {
	d.onStep()
	return d.Driver.Execute(step)
}

// waitFor polls cond until it holds, failing the test after 5 s.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// sweep runs the lease sweeper once and waits for the teardowns it
// started (the sweeper itself does not wait for them).
func (e *hubEnv) sweep() {
	e.t.Helper()
	e.s.sweepLeases()
	waitFor(e.t, func() bool {
		e.s.leases.mu.Lock()
		defer e.s.leases.mu.Unlock()
		return len(e.s.leases.ending) == 0
	})
}

// leaseEvents lists the lease.* events the hub published, as type:device.
func (e *hubEnv) leaseEvents() []string {
	var out []string
	for _, ev := range e.s.events.Recent("", 0) {
		if strings.HasPrefix(ev.Type, "lease.") {
			out = append(out, ev.Type+":"+ev.Device)
		}
	}
	return out
}

func (e *hubEnv) lease(c *Client, req LeaseRequest) *LeaseResult {
	e.t.Helper()
	res, err := c.AcquireLease(context.Background(), req)
	if err != nil {
		e.t.Fatalf("lease %+v: %v", req, err)
	}
	c.SetLease(res.Lease.Device, res.Lease.ID)
	return res
}

func TestLeases_AcquireByIDAndInUse(t *testing.T) {
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	res := env.lease(env.remote, LeaseRequest{Device: "mock-1", Holder: "alice", TTLSec: 30})
	l := res.Lease
	if l.Device != "mock-1" || l.Holder != "alice" || l.TTLSec != 30 || l.ID == "" || res.Device == nil || res.Device.Lease == nil {
		t.Fatalf("lease: %+v %+v", l, res.Device)
	}
	if want := env.clock.now().Add(30 * time.Second).Format(time.RFC3339); l.ExpiresAt != want {
		t.Fatalf("expiresAt %s, want %s", l.ExpiresAt, want)
	}
	// A second client: DEVICE_IN_USE naming the holder.
	_, err := env.local.AcquireLease(ctx, LeaseRequest{Device: "mock-1"})
	if de := wantErrCode(t, err, CodeDeviceInUse); de.Details["holder"] != "alice" {
		t.Fatalf("details: %+v", de.Details)
	}
	// Attached by another daemon on this machine: DEVICE_IN_USE naming it.
	other := &Info{Name: "other", PID: os.Getpid(), APIVersion: APIVersion}
	if err := WriteInfo(other); err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteJSON(DevicesPath("other"), []DeviceInfo{{ID: "mock-2", Platform: "mock", Attached: true}}); err != nil {
		t.Fatal(err)
	}
	_, err = env.remote.AcquireLease(ctx, LeaseRequest{Device: "mock-2"})
	if de := wantErrCode(t, err, CodeDeviceInUse); de.Details["owner"] != "other" {
		t.Fatalf("details: %+v", de.Details)
	}
	devs, _ := env.remote.Devices(ctx, "mock")
	if len(devs) != 2 || devs[1].Owner != "other" || devs[1].Available {
		t.Fatalf("owner in listing: %+v", devs)
	}
	// Bad requests.
	for _, req := range []LeaseRequest{{}, {Device: "mock-1", Select: &LeaseSelector{}}, {Device: "mock-1", TTLSec: -1}} {
		_, err := env.remote.AcquireLease(ctx, req)
		wantErrCode(t, err, CodeUsage)
	}
	// An id the hub does not list is not found, and nothing is granted.
	_, err = env.remote.AcquireLease(ctx, LeaseRequest{Device: "mock-9"})
	if de := wantErrCode(t, err, CodeDeviceNotFound); !strings.Contains(de.Message, "not listed on hub hubtest") {
		t.Fatalf("unlisted: %v", de)
	}
	if ls, _ := env.remote.Leases(ctx); len(ls) != 1 {
		t.Fatalf("an unlisted id was granted: %+v", ls)
	}
	// The default holder names the caller; the default window is 8 hours,
	// the default maximum too.
	res = env.lease(env.remote, LeaseRequest{Device: "SIM-DOWN"})
	if res.Lease.TTLSec != 28800 || res.Lease.MaxSec != 28800 || !strings.Contains(res.Lease.Holder, "192.0.2.20") || res.Device == nil || res.Device.ID != "SIM-DOWN" {
		t.Fatalf("defaults: %+v %+v", res.Lease, res.Device)
	}
	if want := env.clock.now().Add(8 * time.Hour).Format(time.RFC3339); res.Lease.ExpiresAt != want {
		t.Fatalf("default window ends %s, want %s", res.Lease.ExpiresAt, want)
	}
	// A window over the maximum is refused, naming it, and nothing is granted.
	_, err = env.remote.AcquireLease(ctx, LeaseRequest{Device: "SIM-UP", TTLSec: 8*3600 + 1})
	if de := wantErrCode(t, err, CodeUsage); !strings.Contains(de.Message, "8h0m0s") || de.Details["maxSec"] != float64(28800) {
		t.Fatalf("over the maximum: %v %+v", de, de.Details)
	}
	if ls, _ := env.remote.Leases(ctx); len(ls) != 2 {
		t.Fatalf("a refused lease was granted: %+v", ls)
	}
	// Exactly the maximum is granted.
	if res = env.lease(env.remote, LeaseRequest{Device: "SIM-UP", TTLSec: 28800}); res.Lease.TTLSec != 28800 {
		t.Fatalf("at the maximum: %+v", res.Lease)
	}
}

// --lease-max sets the hub's maximum, and DEVICE=DURATION one device's,
// by id or by name; the default window is capped at the maximum; listings
// and leases carry maxSec; a renew over it is refused too.
func TestLeases_LeaseMax(t *testing.T) {
	env := newHubEnv(t, Config{LeaseMax: time.Hour, LeaseMaxDevices: []LeaseMax{
		{Device: "mock-2", Max: 2 * time.Minute}, {Device: "shared phone", Max: 10 * time.Hour}}})
	ctx := context.Background()
	devs, err := env.remote.Devices(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"mock-1": 3600, "mock-2": 120, "SIM-UP": 36000, "SIM-DOWN": 3600}
	for _, d := range devs {
		if d.MaxSec != want[d.ID] {
			t.Errorf("%s maxSec %d, want %d", d.ID, d.MaxSec, want[d.ID])
		}
	}
	// The default window is the device's maximum when that is shorter
	// than 8 hours; a longer maximum still defaults to 8 hours.
	for dev, ttl := range map[string]int{"mock-1": 3600, "mock-2": 120, "SIM-UP": 28800} {
		l := env.lease(env.local, LeaseRequest{Device: dev}).Lease
		if l.TTLSec != ttl || l.MaxSec != want[dev] {
			t.Errorf("%s: %+v", dev, l)
		}
		if _, err := env.local.ReleaseLease(ctx, l.ID); err != nil {
			t.Fatal(err)
		}
	}
	_, err = env.remote.AcquireLease(ctx, LeaseRequest{Device: "mock-2", TTLSec: 180})
	if de := wantErrCode(t, err, CodeUsage); !strings.Contains(de.Message, "2m0s") {
		t.Fatalf("per-device maximum: %v", de)
	}
	_, err = env.remote.AcquireLease(ctx, LeaseRequest{Device: "mock-1", TTLSec: 3601})
	wantErrCode(t, err, CodeUsage)
	l := env.lease(env.remote, LeaseRequest{Device: "SIM-UP", TTLSec: 9 * 3600}).Lease
	if l.MaxSec != 36000 {
		t.Fatalf("by name: %+v", l)
	}
	_, err = env.remote.RenewLease(ctx, l.ID, 36001)
	if de := wantErrCode(t, err, CodeUsage); de.Details["maxSec"] != float64(36000) {
		t.Fatalf("renew over the maximum: %+v", de.Details)
	}
	if got, _ := env.remote.Lease(ctx, l.ID); got.Lease.ExpiresAt != l.ExpiresAt {
		t.Fatalf("a refused renew moved the window: %+v", got.Lease)
	}
	// A selector skips a candidate whose maximum is too short; with none
	// left, it is USAGE naming the maximum.
	_, err = env.remote.AcquireLease(ctx, LeaseRequest{Select: &LeaseSelector{Platform: "mock"}, TTLSec: 600})
	if err != nil {
		t.Fatal(err)
	}
	_, err = env.remote.AcquireLease(ctx, LeaseRequest{Select: &LeaseSelector{Platform: "mock"}, TTLSec: 600})
	if de := wantErrCode(t, err, CodeUsage); !strings.Contains(de.Message, "mock-2") {
		t.Fatalf("selector over the maximum: %v", de)
	}
	_, err = env.remote.AcquireLease(ctx, LeaseRequest{Select: &LeaseSelector{Name: "SIM-DOWN", Boot: true}, TTLSec: 7200})
	wantErrCode(t, err, CodeUsage)
	if len(env.stopped) != 0 || env.booted["SIM-DOWN"] {
		t.Fatalf("a refused boot lease booted: %+v %+v", env.stopped, env.booted)
	}
	if got := leaseMaxNote([]LeaseMax{{Device: "a", Max: time.Minute}}); got != ", a 1m0s" {
		t.Fatalf("note: %q", got)
	}
}

func TestLeases_RequiredAndExpiredOnEveryMutatingRoute(t *testing.T) {
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	res := env.lease(env.remote, LeaseRequest{Device: "mock-1", TTLSec: 60})
	if _, err := env.remote.Attach(ctx, "mock-1", AttachConfig{Platform: "mock"}); err != nil {
		t.Fatal(err)
	}
	lid := res.Lease.ID
	routes := []struct{ method, path, body string }{
		{"POST", "/v1/devices/mock-1/attach", `{"platform":"mock"}`},
		{"POST", "/v1/devices/mock-1/commands/tapOn", `"Login"`},
		{"POST", "/v1/devices/mock-1/steps", `{"steps":[{"tapOn":"x"}]}`},
		{"POST", "/v1/devices/mock-1/flows", `{"yaml":"appId: x\n---\n- tapOn: y\n"}`},
		{"GET", "/v1/devices/mock-1/screenshot", ""},
		{"GET", "/v1/devices/mock-1/hierarchy", ""},
		{"GET", "/v1/devices/mock-1/state", ""},
		{"GET", "/v1/devices/mock-1/vars", ""},
		{"PUT", "/v1/devices/mock-1/vars", `{"A":"1"}`},
		{"POST", "/v1/devices/mock-1/eval", `{"script":"1"}`},
		{"HEAD", "/v1/devices/mock-1/blobs/" + strings.Repeat("a", 64), ""},
		{"PUT", "/v1/devices/mock-1/blobs/" + strings.Repeat("a", 64), "x"},
		{"POST", "/v1/devices/mock-1/apps", `{"app":"blob:sha256:` + strings.Repeat("a", 64) + `/x.apk"}`},
		{"DELETE", "/v1/devices/mock-1/apps/com.example", ""},
		{"GET", "/v1/devices/mock-1/artifacts", ""},
		{"GET", "/v1/devices/mock-1/artifacts/client.log", ""},
		{"DELETE", "/v1/devices/mock-1", ""},
		{"POST", "/v1/devices/mock-1/detach", ""},
	}
	send := func(method, path, body, lease string) (int, Code) {
		req, _ := http.NewRequest(method, env.remote.BaseURL()+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer t")
		if lease != "" {
			req.Header.Set(HeaderLease, lease)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, errorFromResponse(resp.StatusCode, resp.Header, data).Code
	}
	for _, r := range routes {
		if st, code := send(r.method, r.path, r.body, ""); st != http.StatusConflict || (r.method != "HEAD" && code != CodeLeaseRequired) {
			t.Errorf("%s %s without a lease: %d %s", r.method, r.path, st, code)
		}
		if st, code := send(r.method, r.path, r.body, "stale-lease"); st != http.StatusConflict || (r.method != "HEAD" && code != CodeLeaseExpired) {
			t.Errorf("%s %s with a stale lease: %d %s", r.method, r.path, st, code)
		}
	}
	// GET /v1/devices/{id} and …/info stay open.
	for _, p := range []string{"/v1/devices/mock-1", "/v1/devices/mock-1/info"} {
		if st, _ := send("GET", p, "", ""); st != http.StatusOK {
			t.Errorf("GET %s: %d", p, st)
		}
	}
	// With the lease every route is let through to its handler.
	for _, r := range routes[:3] {
		if st, code := send(r.method, r.path, r.body, lid); st >= 300 {
			t.Errorf("%s %s with the lease: %d %s", r.method, r.path, st, code)
		}
	}
	// Another device's lease is not this one's.
	other := env.lease(env.remote, LeaseRequest{Device: "mock-2"})
	if _, code := send("POST", "/v1/devices/mock-1/commands/tapOn", `"x"`, other.Lease.ID); code != CodeLeaseRequired {
		t.Fatalf("another device's lease: %s", code)
	}
	// Stopping a device nobody leases needs no lease.
	if st, code := send("DELETE", "/v1/devices/SIM-UP", `{"force":true}`, ""); st != http.StatusOK {
		t.Fatalf("stop an unleased device: %d %s", st, code)
	}
}

// A lease is a fixed window: using it never moves its expiry; only an
// explicit renew does, by the lease's own window or by ttlSec.
func TestLeases_FixedWindowExpiryAndRelease(t *testing.T) {
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	res := env.lease(env.remote, LeaseRequest{Device: "mock-1", TTLSec: 30, Attach: &AttachConfig{Platform: "mock"}})
	if res.Device == nil || !res.Device.Attached {
		t.Fatalf("attach in the lease call: %+v", res.Device)
	}
	lid, end := res.Lease.ID, res.Lease.ExpiresAt
	// Requests carrying the lease leave its expiry alone.
	for i := 0; i < 2; i++ {
		env.clock.advance(10 * time.Second)
		if _, err := env.remote.Command(ctx, "mock-1", "tapOn", "x", true); err != nil {
			t.Fatalf("tap %d: %v", i, err)
		}
		env.sweep()
		if got, err := env.remote.Lease(ctx, lid); err != nil || got.Lease.ExpiresAt != end {
			t.Fatalf("use moved the expiry: %+v %v", got, err)
		}
	}
	// The window ends 30 s after the grant, used or not.
	env.clock.advance(10 * time.Second)
	env.sweep()
	if atomic.LoadInt32(&env.cleanups) != 1 {
		t.Fatalf("expiry should detach, cleanups=%d", env.cleanups)
	}
	_, err := env.remote.Command(ctx, "mock-1", "tapOn", "x", true)
	wantErrCode(t, err, CodeLeaseExpired)
	for _, f := range []func() error{
		func() error { _, err := env.remote.Lease(ctx, lid); return err },
		func() error { _, err := env.remote.RenewLease(ctx, lid, 0); return err },
		func() error { _, err := env.remote.ReleaseLease(ctx, lid); return err },
	} {
		wantErrCode(t, f(), CodeLeaseExpired)
	}
	devs, _ := env.remote.Devices(ctx, "mock")
	if devs[0].Lease != nil || !devs[0].Available || devs[0].Attached {
		t.Fatalf("free after expiry: %+v", devs[0])
	}
	// An explicit renew moves the end: by the lease's own window, then by
	// the ttlSec it names, which becomes the lease's window.
	res = env.lease(env.local, LeaseRequest{Device: "mock-1", TTLSec: 30, Attach: &AttachConfig{Platform: "mock"}})
	lid = res.Lease.ID
	env.clock.advance(20 * time.Second)
	if r, err := env.local.RenewLease(ctx, lid, 0); err != nil || r.Lease.ExpiresAt != env.clock.now().Add(30*time.Second).Format(time.RFC3339) {
		t.Fatalf("renew: %+v %v", r, err)
	}
	env.clock.advance(20 * time.Second)
	env.sweep()
	r, err := env.local.RenewLease(ctx, lid, 90)
	if err != nil || r.Lease.TTLSec != 90 || r.Lease.ExpiresAt != env.clock.now().Add(90*time.Second).Format(time.RFC3339) {
		t.Fatalf("renew with ttlSec: %+v %v", r, err)
	}
	if _, err := env.local.RenewLease(ctx, lid, -1); err == nil {
		t.Fatal("a negative renew")
	}
	env.clock.advance(80 * time.Second)
	env.sweep()
	if ls, err := env.remote.Leases(ctx); err != nil || len(ls) != 1 || ls[0].ID != lid {
		t.Fatalf("list: %+v %v", ls, err)
	}
	// Release detaches and frees it at once.
	if _, err := env.local.ReleaseLease(ctx, lid); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&env.cleanups) != 2 {
		t.Fatalf("release should detach, cleanups=%d", env.cleanups)
	}
	if ls, _ := env.local.Leases(ctx); len(ls) != 0 {
		t.Fatalf("leases after release: %+v", ls)
	}
}

// At the end of the window the owner ends the lease even while a step
// runs under it: the step is cancelled (INTERRUPTED), the device is
// detached, and the next request with the lease is LEASE_EXPIRED. An
// expired lease the sweeper has not reached yet ends when the device is
// leased again.
func TestLeases_ExpiryInterruptsARunningStep(t *testing.T) {
	env := newHubEnv(t, Config{})
	env.mu.Lock()
	env.stepDelay = 5 * time.Second
	env.mu.Unlock()
	ctx := context.Background()
	res := env.lease(env.remote, LeaseRequest{Device: "mock-1", TTLSec: 5, Attach: &AttachConfig{Platform: "mock"}})
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
	case err := <-done:
		wantErrCode(t, err, CodeInterrupted)
	case <-time.After(3 * time.Second):
		t.Fatal("the step outlived its lease")
	}
	_, err := env.remote.Command(ctx, "mock-1", "tapOn", "x", true)
	wantErrCode(t, err, CodeLeaseExpired)
	// Detached once the step has unwound (the teardown waits for it).
	waitFor(t, func() bool {
		devs, _ := env.local.Devices(ctx, "mock")
		return !devs[0].Attached && devs[0].Available
	})

	old := env.lease(env.remote, LeaseRequest{Device: "mock-2", TTLSec: 5}).Lease
	env.clock.advance(6 * time.Second)
	res2 := env.lease(env.local, LeaseRequest{Device: "mock-2"})
	if res2.Lease.ID == old.ID || res2.Lease.ID == res.Lease.ID {
		t.Fatal("the same lease")
	}
}

// A lease whose window ends while its attach runs ends that attach: the
// device is not left attached without a lease.
func TestLeases_WindowEndsDuringTheLeaseAttach(t *testing.T) {
	env := newHubEnv(t, Config{})
	attaching := make(chan struct{})
	release := make(chan struct{})
	env.mu.Lock()
	env.onAttach = func() {
		close(attaching)
		<-release
	}
	env.mu.Unlock()
	ctx := context.Background()
	done := make(chan error, 1)
	go func() {
		_, err := env.remote.AcquireLease(ctx, LeaseRequest{Device: "mock-1", TTLSec: 5, Attach: &AttachConfig{Platform: "mock"}})
		done <- err
	}()
	<-attaching
	env.clock.advance(6 * time.Second)
	env.s.sweepLeases()
	close(release)
	err := <-done
	if err == nil {
		t.Fatal("an attach outlived its lease")
	}
	if devs, _ := env.local.Devices(ctx, "mock"); devs[0].Attached || devs[0].Lease != nil {
		t.Fatalf("left attached: %+v", devs[0])
	}
}

// An attach whose driver start ignores cancellation (WebDriverAgent's) and
// outlives its lease: the request answers LEASE_EXPIRED as the lease ends,
// without waiting for it; until the attach has unwound the device is not
// available and refuses new leases; the late driver is discarded; and a
// later holder's session is never touched by it. Both the lease request's
// attach and POST …/attach under a lease (released this time).
func TestLeases_AttachOutlivingItsLease(t *testing.T) {
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	var block atomic.Bool
	attaching := make(chan struct{}, 1)
	release, stopped := make(chan struct{}), make(chan struct{})
	// A failing test still lets a blocked driver start return, so the
	// hub's servers can close.
	t.Cleanup(func() { close(stopped) })
	env.mu.Lock()
	env.onAttach = func() {
		if !block.Load() {
			return
		}
		attaching <- struct{}{}
		select {
		case <-release:
		case <-stopped:
		}
	}
	env.mu.Unlock()
	mock1 := func() DeviceInfo {
		devs, err := env.local.Devices(ctx, "mock")
		if err != nil {
			t.Fatal(err)
		}
		return devs[0]
	}
	reserved := func(when string) {
		t.Helper()
		if d := mock1(); d.Available || d.Attached || d.Lease != nil {
			t.Fatalf("%s: listed %+v", when, d)
		}
		_, err := env.local.AcquireLease(ctx, LeaseRequest{Device: "mock-1"})
		if de := wantErrCode(t, err, CodeDeviceInUse); !strings.Contains(de.Message, "being released") {
			t.Fatalf("%s: %s", when, de.Message)
		}
		// A selector passes it over.
		other := env.lease(env.local, LeaseRequest{Select: &LeaseSelector{Platform: "mock"}})
		if other.Lease.Device != "mock-2" {
			t.Fatalf("%s: the selector took %s", when, other.Lease.Device)
		}
		if _, err := env.local.ReleaseLease(ctx, other.Lease.ID); err != nil {
			t.Fatal(err)
		}
	}
	unwind := func() {
		t.Helper()
		before := atomic.LoadInt32(&env.cleanups)
		release <- struct{}{}
		// The late driver is discarded, and the device freed after it.
		waitFor(t, func() bool { return atomic.LoadInt32(&env.cleanups) == before+1 })
		waitFor(t, func() bool { return mock1().Available })
		if d := mock1(); d.Attached {
			t.Fatalf("left attached: %+v", d)
		}
	}

	// 1. The lease request's attach; the window ends (the sweeper).
	block.Store(true)
	done := make(chan error, 1)
	go func() {
		_, err := env.remote.AcquireLease(ctx, LeaseRequest{Device: "mock-1", TTLSec: 5, Attach: &AttachConfig{Platform: "mock"}})
		done <- err
	}()
	<-attaching
	env.clock.advance(6 * time.Second)
	// Expired, not yet ended by the sweeper: not available either.
	if d := mock1(); d.Available || d.Lease != nil {
		t.Fatalf("expired, before the sweep: listed %+v", d)
	}
	swept := make(chan struct{})
	go func() {
		env.s.sweepLeases()
		close(swept)
	}()
	select {
	case <-swept:
	case <-time.After(3 * time.Second):
		t.Fatal("the sweeper waited for the attach")
	}
	select {
	case err := <-done:
		wantErrCode(t, err, CodeLeaseExpired)
	case <-time.After(3 * time.Second):
		t.Fatal("the request waited for the attach")
	}
	reserved("after the lease expired")
	unwind()

	// 2. The next holder is granted only after the late attach unwound, so
	// its teardown is done: the new session attaches, runs and stays.
	block.Store(false)
	b := env.lease(env.remote, LeaseRequest{Device: "mock-1", TTLSec: 60, Attach: &AttachConfig{Platform: "mock"}})
	cleanups := atomic.LoadInt32(&env.cleanups)
	if r, err := env.remote.Command(ctx, "mock-1", "back", nil, true); err != nil || !r.OK {
		t.Fatalf("the new holder's step: %+v %v", r, err)
	}
	if atomic.LoadInt32(&env.cleanups) != cleanups || !mock1().Attached {
		t.Fatal("the new holder's session was torn down")
	}
	if _, err := env.remote.ReleaseLease(ctx, b.Lease.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return mock1().Available })

	// 3. POST …/attach under a lease that is released mid-attach.
	block.Store(true)
	c := env.lease(env.remote, LeaseRequest{Device: "mock-1", TTLSec: 60})
	go func() {
		_, err := env.remote.Attach(ctx, "mock-1", AttachConfig{Platform: "mock"})
		done <- err
	}()
	<-attaching
	start := time.Now()
	if _, err := env.local.ReleaseLease(ctx, c.Lease.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		wantErrCode(t, err, CodeLeaseExpired)
	case <-time.After(3 * time.Second):
		t.Fatal("the attach request waited for the attach")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("release waited for the attach")
	}
	reserved("after the lease was released")
	unwind()
}

// A step whose driver call ignores cancellation (a WebDriverAgent HTTP
// call) and outlives its lease: the request answers soon after the lease
// ends, without waiting for it; the device stays attached, unavailable and
// refuses new leases until the step has returned; only then is it detached
// and freed, and a new lease on it is granted.
func TestLeases_StepOutlivingItsLease(t *testing.T) {
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	stepping := make(chan struct{}, 1)
	release, stopped := make(chan struct{}), make(chan struct{})
	// A failing test still lets the blocked step return, so the hub's
	// servers can close.
	t.Cleanup(func() { close(stopped) })
	env.mu.Lock()
	env.onStep = func() {
		stepping <- struct{}{}
		select {
		case <-release:
		case <-stopped:
		}
	}
	env.mu.Unlock()
	mock1 := func() DeviceInfo {
		devs, err := env.local.Devices(ctx, "mock")
		if err != nil {
			t.Fatal(err)
		}
		return devs[0]
	}
	env.lease(env.remote, LeaseRequest{Device: "mock-1", TTLSec: 5, Attach: &AttachConfig{Platform: "mock"}})
	done := make(chan error, 1)
	go func() {
		_, err := env.remote.Command(ctx, "mock-1", "tapOn", "stuck", true)
		done <- err
	}()
	<-stepping
	env.clock.advance(6 * time.Second)
	swept := make(chan struct{})
	go func() {
		env.s.sweepLeases()
		close(swept)
	}()
	select {
	case <-swept:
	case <-time.After(3 * time.Second):
		t.Fatal("the sweeper waited for the step")
	}
	ended := time.Now()
	select {
	case err := <-done:
		wantErrCode(t, err, CodeLeaseExpired)
	case <-time.After(leaseUnwindGrace + 3*time.Second):
		t.Fatal("the request waited for the step")
	}
	if took := time.Since(ended); took > leaseUnwindGrace+2*time.Second {
		t.Fatalf("answered %v after the lease ended", took)
	}
	// The step still talks to the driver: the device is not detached,
	// listed available, nor granted.
	if d := mock1(); d.Available || !d.Attached || d.Lease != nil || atomic.LoadInt32(&env.cleanups) != 0 {
		t.Fatalf("while the step runs: listed %+v, cleanups %d", d, atomic.LoadInt32(&env.cleanups))
	}
	_, err := env.local.AcquireLease(ctx, LeaseRequest{Device: "mock-1"})
	if de := wantErrCode(t, err, CodeDeviceInUse); !strings.Contains(de.Message, "being released") {
		t.Fatalf("while the step runs: %s", de.Message)
	}
	if other := env.lease(env.local, LeaseRequest{Select: &LeaseSelector{Platform: "mock"}}); other.Lease.Device != "mock-2" {
		t.Fatalf("the selector took %s", other.Lease.Device)
	}

	// The step returns: only now is the device detached and freed.
	close(release)
	waitFor(t, func() bool { return atomic.LoadInt32(&env.cleanups) == 1 })
	waitFor(t, func() bool { return mock1().Available })
	if d := mock1(); d.Attached {
		t.Fatalf("left attached: %+v", d)
	}
	if next := env.lease(env.remote, LeaseRequest{Device: "mock-1"}); next.Lease.Device != "mock-1" {
		t.Fatalf("leased %s", next.Lease.Device)
	}
}

func TestLeases_Selector(t *testing.T) {
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	// Match order: the listing's (platform, id) order, ready devices only.
	a := env.lease(env.remote, LeaseRequest{Select: &LeaseSelector{Platform: "mock"}})
	b := env.lease(env.remote, LeaseRequest{Select: &LeaseSelector{Platform: "MOCK"}})
	if a.Lease.Device != "mock-1" || b.Lease.Device != "mock-2" {
		t.Fatalf("order: %s, %s", a.Lease.Device, b.Lease.Device)
	}
	_, err := env.remote.AcquireLease(ctx, LeaseRequest{Select: &LeaseSelector{Platform: "mock"}})
	if de := wantErrCode(t, err, CodeDeviceInUse); len(de.Details["candidates"].([]any)) != 2 {
		t.Fatalf("candidates: %+v", de.Details)
	}
	// No match: DEVICE_NOT_FOUND naming the selector.
	for _, sel := range []LeaseSelector{{Platform: "android"}, {Name: "nope"}, {Platform: "ios", OSVersion: "27"}, {Hub: "elsewhere"}, {Kind: "emulator"}} {
		_, err := env.remote.AcquireLease(ctx, LeaseRequest{Select: &sel})
		if de := wantErrCode(t, err, CodeDeviceNotFound); !strings.Contains(de.Message, sel.String()) {
			t.Fatalf("message %q lacks %s", de.Message, sel.String())
		}
	}
	// Name matches the name (any case) or the id; osVersion a prefix; the
	// hub's own name matches.
	c := env.lease(env.remote, LeaseRequest{Select: &LeaseSelector{Name: "shared phone", OSVersion: "26", Kind: "simulator", Hub: "hubtest"}})
	if c.Lease.Device != "SIM-UP" || c.Lease.Booted {
		t.Fatalf("by name: %+v", c.Lease)
	}
	// A shut-down simulator is a candidate only with boot: true.
	_, err = env.remote.AcquireLease(ctx, LeaseRequest{Select: &LeaseSelector{Platform: "ios"}})
	wantErrCode(t, err, CodeDeviceInUse)
	d := env.lease(env.remote, LeaseRequest{Select: &LeaseSelector{Platform: "ios", Boot: true}})
	if d.Lease.Device != "SIM-DOWN" || !d.Lease.Booted || !env.booted["SIM-DOWN"] {
		t.Fatalf("boot: %+v %v", d.Lease, env.booted)
	}
	// Ending a lease-booted lease shuts the device down.
	if _, err := env.remote.ReleaseLease(ctx, d.Lease.ID); err != nil {
		t.Fatal(err)
	}
	if len(env.stopped) != 1 || env.stopped[0] != "SIM-DOWN" {
		t.Fatalf("stopped: %v", env.stopped)
	}
	// ...and so does expiry.
	d = env.lease(env.remote, LeaseRequest{Select: &LeaseSelector{Name: "SIM-DOWN", Boot: true}, TTLSec: 1})
	env.clock.advance(2 * time.Second)
	env.sweep()
	if len(env.stopped) != 2 {
		t.Fatalf("expiry of a lease-booted device: %v", env.stopped)
	}
	// A failed boot is DEVICE_ERROR.
	env.s.registry.boot = func(string, string, time.Duration) (string, error) { return "", io.ErrUnexpectedEOF }
	_, err = env.remote.AcquireLease(ctx, LeaseRequest{Select: &LeaseSelector{Platform: "ios", Boot: true}})
	wantErrCode(t, err, CodeDeviceError)
}

// Two selector leases at once: while one boots a shut-down simulator, the
// other must not take that simulator the moment it is listed ready, and the
// booting caller gets it, as lease-booted. A client that leases a booted
// device's new id by name in that window keeps it, and its lease shuts the
// device down when it ends.
func TestLeases_ConcurrentBootSelectors(t *testing.T) {
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	gate := make(chan struct{})
	booting := make(chan struct{})
	// open releases the boot; a failed check releases it too, so the
	// blocked request does not hold the test server open.
	var once *sync.Once
	open := func() { once.Do(func() { close(gate) }) }
	once = &sync.Once{}
	t.Cleanup(func() { open() })
	env.s.registry.boot = func(platform, name string, timeout time.Duration) (string, error) {
		env.mu.Lock()
		env.booted[name] = true
		env.mu.Unlock()
		close(booting)
		<-gate
		return name, nil
	}
	type result struct {
		res *LeaseResult
		err error
	}
	first := make(chan result, 1)
	go func() {
		res, err := env.remote.AcquireLease(ctx, LeaseRequest{Select: &LeaseSelector{Name: "Spare Phone", Boot: true}})
		first <- result{res, err}
	}()
	<-booting
	// SIM-DOWN is listed ready now, but it is the first caller's.
	_, err := env.remote.AcquireLease(ctx, LeaseRequest{Select: &LeaseSelector{Name: "Spare Phone", Boot: true}})
	wantErrCode(t, err, CodeDeviceInUse)
	_, err = env.remote.AcquireLease(ctx, LeaseRequest{Device: "SIM-DOWN"})
	wantErrCode(t, err, CodeDeviceInUse)
	open()
	r := <-first
	if r.err != nil || r.res.Lease.Device != "SIM-DOWN" || !r.res.Lease.Booted {
		t.Fatalf("the booting caller: %+v %v", r.res, r.err)
	}
	if _, err := env.remote.ReleaseLease(ctx, r.res.Lease.ID); err != nil {
		t.Fatal(err)
	}

	// The boot returns another id (as an AVD becomes a serial) that a
	// client leased by id meanwhile: that lease adopts the boot.
	gate, booting, once = make(chan struct{}), make(chan struct{}), &sync.Once{}
	env.s.registry.boot = func(platform, name string, timeout time.Duration) (string, error) {
		close(booting)
		<-gate
		return "SIM-UP", nil
	}
	go func() {
		res, err := env.remote.AcquireLease(ctx, LeaseRequest{Select: &LeaseSelector{Name: "Spare Phone", Boot: true}})
		first <- result{res, err}
	}()
	<-booting
	other := env.lease(env.remote, LeaseRequest{Device: "SIM-UP"})
	open()
	r = <-first
	wantErrCode(t, r.err, CodeDeviceInUse)
	if l, ok := env.s.leases.get(other.Lease.ID); !ok || !l.Booted {
		t.Fatalf("the other lease did not adopt the boot: %+v", l)
	}
	env.mu.Lock()
	before := len(env.stopped)
	env.mu.Unlock()
	if _, err := env.remote.ReleaseLease(ctx, other.Lease.ID); err != nil {
		t.Fatal(err)
	}
	env.mu.Lock()
	defer env.mu.Unlock()
	if len(env.stopped) != before+1 || env.stopped[before] != "SIM-UP" {
		t.Fatalf("stopped: %v", env.stopped)
	}
}

// A booting AVD is reserved under its name, which the emulator it becomes
// is listed with; a simulator under its UDID.
func TestLeases_BootReservationKeys(t *testing.T) {
	tb := newLeaseTable()
	avd := DeviceInfo{ID: AVDPrefix + "Pixel_9", Platform: "android", Kind: "emulator", Name: "Pixel_9"}
	sim := DeviceInfo{ID: "UDID-1", Platform: "ios", Kind: "simulator", Name: "iPhone"}
	tb.booting[bootKey(avd)], tb.booting[bootKey(sim)] = true, true
	for _, c := range []struct {
		d    DeviceInfo
		want bool
	}{
		{DeviceInfo{ID: "emulator-5554", Platform: "android", Name: "pixel_9"}, true},
		{DeviceInfo{ID: "emulator-5556", Platform: "android", Name: "Other"}, false},
		{DeviceInfo{ID: "emulator-5558", Platform: "android"}, false},
		{DeviceInfo{ID: "UDID-1", Platform: "ios", Name: "iPhone"}, true},
		{DeviceInfo{ID: "UDID-2", Platform: "ios", Name: "iPhone"}, false},
	} {
		if got := tb.reservedLocked(c.d); got != c.want {
			t.Errorf("%+v: reserved %v", c.d, got)
		}
	}
	if tb.markBooted("nothing") {
		t.Fatal("marked a device no lease holds")
	}
}

func TestLeases_ListingsCarryLeaseState(t *testing.T) {
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	env.lease(env.remote, LeaseRequest{Device: "mock-1", Holder: "bob"})
	devs, err := env.remote.Devices(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]DeviceInfo{}
	for _, d := range devs {
		byID[d.ID] = d
	}
	if l := byID["mock-1"].Lease; l == nil || l.Holder != "bob" || byID["mock-1"].Available {
		t.Fatalf("leased: %+v", byID["mock-1"])
	}
	if !byID["mock-2"].Available || byID["mock-2"].Lease != nil || byID["SIM-DOWN"].Available {
		t.Fatalf("available: %+v %+v", byID["mock-2"], byID["SIM-DOWN"])
	}
	one, err := env.remote.Device(ctx, "mock-1")
	if err != nil || one.Lease == nil {
		t.Fatalf("GET device: %+v %v", one, err)
	}
	st, err := env.remote.Status(ctx)
	if err != nil || st.Daemon.Hub != "hubtest" {
		t.Fatalf("status: %+v %v", st, err)
	}
	// A plain daemon's listing has neither field, and no lease routes.
	plain := newTestEnv(t, 0)
	pdevs, _ := plain.c.Devices(ctx, "")
	if pdevs[0].Lease != nil || pdevs[0].Available {
		t.Fatalf("plain daemon: %+v", pdevs[0])
	}
	for _, f := range []func() error{
		func() error { _, err := plain.c.AcquireLease(ctx, LeaseRequest{Device: "mock-1"}); return err },
		func() error { _, err := plain.c.Leases(ctx); return err },
		func() error { _, err := plain.c.Lease(ctx, "x"); return err },
		func() error { _, err := plain.c.RenewLease(ctx, "x", 0); return err },
		func() error { _, err := plain.c.ReleaseLease(ctx, "x"); return err },
	} {
		wantErrCode(t, f(), CodeUsage)
	}
	// And attaches without one, as before.
	plain.attach("mock-1")
}

func TestLeases_ShareDevice(t *testing.T) {
	env := newHubEnv(t, Config{ShareDevices: []string{"shared phone", "mock-2", "mock-9"}})
	ctx := context.Background()
	devs, _ := env.remote.Devices(ctx, "")
	var ids []string
	for _, d := range devs {
		ids = append(ids, d.ID)
	}
	if strings.Join(ids, ",") != "SIM-UP,mock-2" {
		t.Fatalf("remote listing: %v", ids)
	}
	st, _ := env.remote.Status(ctx)
	if len(st.Devices) != 0 {
		t.Fatalf("status: %+v", st.Devices)
	}
	// The unix socket sees every device.
	if all, _ := env.local.Devices(ctx, ""); len(all) != 4 {
		t.Fatalf("local listing: %d", len(all))
	}
	// An unshared device is not found for a remote caller, on every route.
	_, err := env.remote.Device(ctx, "mock-1")
	wantErrCode(t, err, CodeDeviceNotFound)
	_, err = env.remote.AcquireLease(ctx, LeaseRequest{Device: "mock-1"})
	wantErrCode(t, err, CodeDeviceNotFound)
	_, err = env.remote.Command(ctx, "mock-1", "tapOn", "x", true)
	wantErrCode(t, err, CodeDeviceNotFound)
	_, err = env.remote.AcquireLease(ctx, LeaseRequest{Select: &LeaseSelector{Name: "Mock One"}})
	wantErrCode(t, err, CodeDeviceNotFound)
	// Shared ones by id and by name; an unlisted id is not found even when
	// --share-device names it.
	env.lease(env.remote, LeaseRequest{Device: "mock-2"})
	env.lease(env.remote, LeaseRequest{Device: "SIM-UP"})
	_, err = env.remote.AcquireLease(ctx, LeaseRequest{Device: "mock-9"})
	wantErrCode(t, err, CodeDeviceNotFound)
	if _, err := env.remote.Device(ctx, "SIM-UP"); err != nil {
		t.Fatal(err)
	}
	// Booting or creating an unshared device remotely is refused.
	_, err = env.remote.StartDevice(ctx, BootRequest{Platform: "ios", Name: "Spare Phone"})
	wantErrCode(t, err, CodeDeviceNotFound)
	_, err = env.remote.CreateDevice(ctx, BootRequest{Platform: "ios", Name: "Other"})
	wantErrCode(t, err, CodeForbidden)
	_, err = env.remote.StartDevice(ctx, BootRequest{Platform: "ios", Name: "SIM-UP"})
	if err != nil {
		t.Fatalf("boot a shared device: %v", err)
	}
	if !shareMatches("Edge_Pixel", DeviceInfo{ID: "avd:Edge_Pixel"}) || shareMatches("x", DeviceInfo{ID: "y"}) {
		t.Fatal("shareMatches")
	}
}

// A remote caller does not see the leases on unshared devices: the list
// leaves them out, and reading, renewing or releasing one is LEASE_EXPIRED,
// as for an unknown lease, and leaves it alone. The unix socket sees them.
func TestLeases_ShareDeviceHidesUnsharedLeases(t *testing.T) {
	env := newHubEnv(t, Config{ShareDevices: []string{"mock-2"}})
	ctx := context.Background()
	private := env.lease(env.local, LeaseRequest{Device: "mock-1"}).Lease
	shared := env.lease(env.local, LeaseRequest{Device: "mock-2"}).Lease
	got, err := env.remote.Leases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != shared.ID {
		t.Fatalf("remote leases: %+v", got)
	}
	// So that a renew would move the expiry on.
	env.clock.advance(time.Minute)
	_, err = env.remote.Lease(ctx, private.ID)
	wantErrCode(t, err, CodeLeaseExpired)
	_, err = env.remote.RenewLease(ctx, private.ID, 0)
	wantErrCode(t, err, CodeLeaseExpired)
	_, err = env.remote.ReleaseLease(ctx, private.ID)
	wantErrCode(t, err, CodeLeaseExpired)
	if l, err := env.local.Lease(ctx, private.ID); err != nil || l.Lease.ExpiresAt != private.ExpiresAt {
		t.Fatalf("the private lease after remote calls: %+v %v", l, err)
	}
	if all, _ := env.local.Leases(ctx); len(all) != 2 {
		t.Fatalf("local leases: %+v", all)
	}
	// The shared device's lease works remotely.
	if _, err := env.remote.Lease(ctx, shared.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.remote.RenewLease(ctx, shared.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := env.remote.ReleaseLease(ctx, shared.ID); err != nil {
		t.Fatal(err)
	}
	_, err = env.local.Lease(ctx, shared.ID)
	wantErrCode(t, err, CodeLeaseExpired)
}

// Ending a lease detaches the device and shuts down one the lease booted,
// which takes seconds; that runs outside the hub's lease lock, so leases
// on other devices are granted meanwhile, while the device itself stays
// reserved until its shutdown is done.
func TestLeases_SlowShutdownDoesNotBlockOtherLeases(t *testing.T) {
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	stopping := make(chan string, 4)
	unblock := make(chan struct{})
	env.s.registry.shutdown = func(d DeviceInfo) error {
		stopping <- d.ID
		<-unblock
		env.mu.Lock()
		delete(env.booted, d.ID)
		env.mu.Unlock()
		return nil
	}
	defer func() {
		select {
		case <-unblock:
		default:
			close(unblock)
		}
	}()
	res := env.lease(env.remote, LeaseRequest{Select: &LeaseSelector{Name: "Spare Phone", Boot: true}, TTLSec: 30})
	if !res.Lease.Booted || res.Lease.Device != "SIM-DOWN" {
		t.Fatalf("boot lease: %+v", res.Lease)
	}
	// It expires; the sweeper starts shutting SIM-DOWN down and blocks.
	env.clock.advance(31 * time.Second)
	swept := make(chan struct{})
	go func() {
		env.s.sweepLeases()
		close(swept)
	}()
	select {
	case id := <-stopping:
		if id != "SIM-DOWN" {
			t.Fatalf("stopping %s", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the sweeper never shut the booted device down")
	}
	// The sweep does not wait for the shutdown.
	select {
	case <-swept:
	case <-time.After(5 * time.Second):
		t.Fatal("the sweep waited for a teardown")
	}
	// A lease on another device, a listing and a selector do not wait.
	done := make(chan error, 1)
	go func() {
		_, err := env.remote.AcquireLease(ctx, LeaseRequest{Device: "mock-1"})
		if err == nil {
			_, err = env.remote.AcquireLease(ctx, LeaseRequest{Select: &LeaseSelector{Platform: "mock"}})
		}
		if err == nil {
			_, err = env.remote.Leases(ctx)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lease calls waited on another device's shutdown")
	}
	// The device being shut down is not leased by id or by selector.
	_, err := env.remote.AcquireLease(ctx, LeaseRequest{Device: "SIM-DOWN"})
	wantErrCode(t, err, CodeDeviceInUse)
	_, err = env.remote.AcquireLease(ctx, LeaseRequest{Select: &LeaseSelector{Name: "Spare Phone"}})
	wantErrCode(t, err, CodeDeviceInUse)
	close(unblock)
	waitFor(t, func() bool { return !env.s.leases.releasing(DeviceInfo{ID: "SIM-DOWN"}) })
	env.lease(env.remote, LeaseRequest{Device: "SIM-DOWN"})
}

// A slow teardown never holds up the sweeper: a lease that expires while
// another lease's device is still shutting down is ended on the next
// sweep, and the requests under it are cancelled.
func TestLeases_SweeperEndsLeasesBesideASlowTeardown(t *testing.T) {
	env := newHubEnv(t, Config{})
	stopping := make(chan string, 4)
	unblock := make(chan struct{})
	env.s.registry.shutdown = func(d DeviceInfo) error {
		stopping <- d.ID
		<-unblock
		return nil
	}
	var once sync.Once
	defer once.Do(func() { close(unblock) })
	env.lease(env.remote, LeaseRequest{Select: &LeaseSelector{Name: "Spare Phone", Boot: true}, TTLSec: 10})
	b := env.lease(env.remote, LeaseRequest{Device: "mock-1", TTLSec: 20}).Lease
	entry := env.s.leases.lookup(b.ID)
	env.clock.advance(11 * time.Second)
	swept := make(chan struct{})
	go func() {
		env.s.sweepLeases()
		close(swept)
	}()
	select {
	case <-stopping:
	case <-time.After(5 * time.Second):
		t.Fatal("the sweeper never shut the booted device down")
	}
	select {
	case <-swept:
	case <-time.After(3 * time.Second):
		t.Fatal("the sweep waited for a slow teardown")
	}
	// mock-1's lease expires while SIM-DOWN is still shutting down.
	env.clock.advance(10 * time.Second)
	done := make(chan struct{})
	go func() {
		env.s.sweepLeases()
		close(done)
	}()
	select {
	case <-entry.ended:
	case <-time.After(3 * time.Second):
		t.Fatal("a lease that expired beside a slow teardown was not ended")
	}
	<-done
	if env.s.leases.lookup(b.ID) != nil {
		t.Fatal("the expired lease is still in the table")
	}
	once.Do(func() { close(unblock) })
	waitFor(t, func() bool { return !env.s.leases.releasing(DeviceInfo{ID: "SIM-DOWN"}) })
}

// An expired lease a selector meets before the sweeper is ended in the
// background, and the selector takes another device.
func TestLeases_SelectorSkipsAnUnsweptExpiredLease(t *testing.T) {
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	old := env.lease(env.remote, LeaseRequest{Device: "mock-1", TTLSec: 5})
	env.clock.advance(6 * time.Second)
	res, err := env.remote.AcquireLease(ctx, LeaseRequest{Select: &LeaseSelector{Platform: "mock"}})
	if err != nil || res.Lease.Device != "mock-2" {
		t.Fatalf("selector: %+v %v", res, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	ending := func() bool {
		env.s.leases.mu.Lock()
		defer env.s.leases.mu.Unlock()
		return env.s.leases.reservedLocked(DeviceInfo{ID: "mock-1"})
	}
	for env.s.leases.lookup(old.Lease.ID) != nil || ending() {
		if time.Now().After(deadline) {
			t.Fatal("the expired lease was not ended")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Leasing by id a device whose expired, unswept lease booted it runs that
// lease's teardown first, which shuts the device down: an emulator whose
// serial is then gone is DEVICE_NOT_FOUND, never a lease on a device the
// hub no longer lists.
func TestLeases_ByIDAfterAShutdownTeardownIsNotFound(t *testing.T) {
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	env.mu.Lock()
	env.emuAVD = "Pixel"
	env.mu.Unlock()
	env.s.registry.shutdown = func(d DeviceInfo) error {
		env.mu.Lock()
		env.emuAVD, env.downAVD = "", "Pixel"
		env.stopped = append(env.stopped, d.ID)
		env.mu.Unlock()
		return nil
	}
	env.lease(env.remote, LeaseRequest{Device: "emulator-5554", TTLSec: 5})
	if !env.s.leases.markBooted("emulator-5554") {
		t.Fatal("no live lease on emulator-5554")
	}
	env.clock.advance(6 * time.Second)
	_, err := env.remote.AcquireLease(ctx, LeaseRequest{Device: "emulator-5554"})
	wantErrCode(t, err, CodeDeviceNotFound)
	env.mu.Lock()
	stopped := strings.Join(env.stopped, " ")
	env.mu.Unlock()
	if stopped != "emulator-5554" {
		t.Fatalf("stopped = %q", stopped)
	}
	env.s.leases.mu.Lock()
	held := env.s.leases.byDevice["emulator-5554"]
	env.s.leases.mu.Unlock()
	if held != nil {
		t.Fatalf("leased a device its old lease shut down: %+v", held)
	}
}

// An emulator serial is reused by whichever AVD boots on its port next, so
// a shared AVD's serial stays shared only while that AVD runs on it.
func TestLeases_ShareDeviceEmulatorSerialReused(t *testing.T) {
	env := newHubEnv(t, Config{ShareDevices: []string{"Pixel_Shared"}})
	ctx := context.Background()
	env.mu.Lock()
	env.emuAVD = "Pixel_Shared"
	env.mu.Unlock()
	res := env.lease(env.remote, LeaseRequest{Device: "emulator-5554"})
	if _, err := env.remote.Device(ctx, "emulator-5554"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.remote.ReleaseLease(ctx, res.Lease.ID); err != nil {
		t.Fatal(err)
	}
	// Pixel_Shared stops, and an unshared AVD boots on the same port.
	env.mu.Lock()
	env.emuAVD = "Pixel_Private"
	env.mu.Unlock()
	_, err := env.remote.AcquireLease(ctx, LeaseRequest{Device: "emulator-5554"})
	wantErrCode(t, err, CodeDeviceNotFound)
	_, err = env.remote.Device(ctx, "emulator-5554")
	wantErrCode(t, err, CodeDeviceNotFound)
	_, err = env.remote.Command(ctx, "emulator-5554", "tapOn", "x", true)
	wantErrCode(t, err, CodeDeviceNotFound)
	if len(env.s.sharedIDs) != 0 {
		t.Fatalf("an emulator serial must not be remembered: %v", env.s.sharedIDs)
	}
}

// A run of steps on a shared emulator, followed on a remote event stream,
// looks the serial up once: not per request and per event. Its stop
// forgets the answer, and so does the time.
func TestLeases_ShareDeviceLooksASerialUpOnce(t *testing.T) {
	env := newHubEnv(t, Config{ShareDevices: []string{"Pixel_Shared", "SIM-UP"}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	env.mu.Lock()
	env.emuAVD = "Pixel_Shared"
	env.mu.Unlock()
	env.lease(env.remote, LeaseRequest{Device: "emulator-5554", Attach: &AttachConfig{Platform: "mock"}})
	before := env.lists.Load()
	steps := make(chan struct{})
	go func() {
		n := 0
		_ = env.remote.Events(ctx, "emulator-5554", 0, func(ev Event) bool {
			if ev.Type == "step" {
				n++
			}
			if n == 5 {
				close(steps)
				return false
			}
			return true
		})
	}()
	time.Sleep(100 * time.Millisecond)
	for range 5 {
		if _, err := env.remote.Command(ctx, "emulator-5554", "tapOn", "x", true); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-steps:
	case <-ctx.Done():
		t.Fatal("the remote stream did not see the five steps")
	}
	if n := env.lists.Load() - before; n > 1 {
		t.Fatalf("five steps and their events ran %d discoveries", n)
	}
	// Remembered for shareCacheFor; a device event forgets it.
	before = env.lists.Load()
	env.s.shared("emulator-5554")
	if env.lists.Load() != before {
		t.Fatal("not remembered")
	}
	env.clock.advance(shareCacheFor)
	env.s.shared("emulator-5554")
	if env.lists.Load() != before+1 {
		t.Fatal("remembered past shareCacheFor")
	}
	env.s.events.Publish(Event{Type: "device.stopped", Device: "emulator-5554"})
	env.mu.Lock()
	env.emuAVD = "Pixel_Private"
	env.mu.Unlock()
	if env.s.shared("emulator-5554") {
		t.Fatal("a stop kept the serial's answer")
	}
	// A negative answer is remembered too, and forgotten the same way.
	before = env.lists.Load()
	if env.s.shared("emulator-5554") || env.lists.Load() != before {
		t.Fatal("negative answer not remembered")
	}
	// An answer looked up while a device changed is not kept.
	env.mu.Lock()
	env.onList = func() { env.s.events.Publish(Event{Type: "device.booted", Device: "emulator-5554"}) }
	env.mu.Unlock()
	env.clock.advance(shareCacheFor)
	env.s.shared("emulator-5554")
	env.s.shareMu.Lock()
	_, kept := env.s.shareSeen["emulator-5554"]
	env.s.shareMu.Unlock()
	if kept {
		t.Fatal("kept an answer a device change overtook")
	}
}

// A remote caller's event stream carries only the shared pool's events,
// without the host's report directory; the unix socket's carries all.
func TestLeases_ShareDeviceEvents(t *testing.T) {
	env := newHubEnv(t, Config{ShareDevices: []string{"mock-2"}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := env.remote.Events(ctx, "mock-1", 0, func(Event) bool { return false })
	wantErrCode(t, err, CodeDeviceNotFound)
	for _, ev := range []Event{
		{Type: "step", Device: "mock-1", Step: "unshared"},
		{Type: "device.attached", Device: "mock-2", Message: "/host/reports/mock-2"},
		{Type: "device.attached", Device: "mock-1", Message: "/host/reports/mock-1"},
		{Type: "note", Message: "/host/path"},
		{Type: "step", Device: "mock-2", Step: "shared"},
		{Type: "daemon.shutdown", Message: "test"},
	} {
		env.s.events.Publish(ev)
	}
	collect := func(c *Client, device string) []Event {
		var got []Event
		if err := c.Events(ctx, device, 0, func(ev Event) bool {
			got = append(got, ev)
			return ev.Type != "daemon.shutdown" && !(device != "" && ev.Step == "shared")
		}); err != nil {
			t.Fatal(err)
		}
		return got
	}
	got := collect(env.remote, "")
	if len(got) != 3 || got[0].Device != "mock-2" || got[0].Message != "" || got[1].Step != "shared" || got[2].Type != "daemon.shutdown" {
		t.Fatalf("remote stream: %+v", got)
	}
	if got := collect(env.remote, "mock-2"); len(got) != 2 || got[0].Message != "" {
		t.Fatalf("remote stream for a shared device: %+v", got)
	}
	if got := collect(env.local, ""); len(got) != 6 || got[1].Message != "/host/reports/mock-2" {
		t.Fatalf("local stream: %+v", got)
	}
}

// A remote caller cannot stop a hub; its unix socket can.
func TestLeases_RemoteShutdownForbidden(t *testing.T) {
	env := newHubEnv(t, Config{})
	err := env.remote.Shutdown(context.Background())
	wantErrCode(t, err, CodeForbidden)
	if env.s.shuttingDown.Load() {
		t.Fatal("a remote shutdown request stopped the hub")
	}
}

func TestLeases_RemoteAttachRefusedAndAttachFailureEndsLease(t *testing.T) {
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	_, err := env.remote.AcquireLease(ctx, LeaseRequest{Device: "mock-1", Attach: &AttachConfig{Platform: "web"}})
	wantErrCode(t, err, CodeForbidden)
	env.s.deps.NewDriver = func(context.Context, string, AttachConfig) (core.Driver, DriverInfo, func(), error) {
		return nil, DriverInfo{}, nil, io.ErrUnexpectedEOF
	}
	_, err = env.remote.AcquireLease(ctx, LeaseRequest{Device: "mock-1", Attach: &AttachConfig{Platform: "mock"}})
	wantErrCode(t, err, CodeDeviceError)
	if ls, _ := env.remote.Leases(ctx); len(ls) != 0 {
		t.Fatalf("a failed attach must end its lease: %+v", ls)
	}
}

func TestLeases_DeleteDeviceEndsLease(t *testing.T) {
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	if err := recordCreated(CreatedRecord{Platform: "ios", ID: "SIM-UP", Name: "Shared Phone"}); err != nil {
		t.Fatal(err)
	}
	var deleted []string
	env.s.registry.deps.DeleteSimulator = func(udid string) error { deleted = append(deleted, udid); return nil }
	env.lease(env.remote, LeaseRequest{Device: "SIM-UP"})
	if _, err := env.remote.DeleteDevice(ctx, "SIM-UP", false); err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 {
		t.Fatalf("deleted: %v", deleted)
	}
	if ls, _ := env.remote.Leases(ctx); len(ls) != 0 {
		t.Fatalf("lease after delete: %+v", ls)
	}
}

// `device delete NAME` is checked as the device the name resolves to: a
// client without the lease cannot delete another client's leased
// simulator by its name, and the holder still can.
func TestLeases_DeleteByNameNeedsTheLease(t *testing.T) {
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	if err := recordCreated(CreatedRecord{Platform: "ios", ID: "SIM-UP", Name: "Shared Phone"}); err != nil {
		t.Fatal(err)
	}
	var deleted []string
	env.s.registry.deps.DeleteSimulator = func(udid string) error { deleted = append(deleted, udid); return nil }
	res := env.lease(env.remote, LeaseRequest{Device: "SIM-UP", Holder: "alice"})
	for _, c := range []*Client{env.local, env.remote} {
		_, err := c.DeleteDevice(ctx, "Shared Phone", false)
		if de := wantErrCode(t, err, CodeLeaseRequired); de.Details["holder"] != "alice" {
			t.Fatalf("details: %+v", de.Details)
		}
	}
	if len(deleted) != 0 {
		t.Fatalf("deleted without the lease: %v", deleted)
	}
	// The holder deletes it by name with its lease.
	env.local.SetLease("Shared Phone", res.Lease.ID)
	if _, err := env.local.DeleteDevice(ctx, "Shared Phone", false); err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || deleted[0] != "SIM-UP" {
		t.Fatalf("deleted: %v", deleted)
	}
	if ls, _ := env.remote.Leases(ctx); len(ls) != 0 {
		t.Fatalf("lease after delete: %+v", ls)
	}
}

// A remote caller cannot delete an unshared device by its name either.
func TestLeases_DeleteByNameOfAnUnsharedDevice(t *testing.T) {
	env := newHubEnv(t, Config{ShareDevices: []string{"mock-1"}})
	if err := recordCreated(CreatedRecord{Platform: "ios", ID: "SIM-UP", Name: "Shared Phone"}); err != nil {
		t.Fatal(err)
	}
	env.s.registry.deps.DeleteSimulator = func(string) error { t.Fatal("deleted an unshared device"); return nil }
	_, err := env.remote.DeleteDevice(context.Background(), "Shared Phone", false)
	wantErrCode(t, err, CodeDeviceNotFound)
}

func TestLeases_ClientForgetsExpiredLease(t *testing.T) {
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	var told []string
	env.remote.OnLeaseExpired = func(device, lease string) { told = append(told, device+"="+lease) }
	env.remote.SetLease("mock-1", "gone")
	_, err := env.remote.Command(ctx, "mock-1", "tapOn", "x", true)
	wantErrCode(t, err, CodeLeaseExpired)
	if env.remote.LeaseOf("mock-1") != "" || len(told) != 1 || told[0] != "mock-1=gone" {
		t.Fatalf("told %v, lease %q", told, env.remote.LeaseOf("mock-1"))
	}
	// Blob uploads carry the lease too.
	res := env.lease(env.remote, LeaseRequest{Device: "mock-1"})
	f := filepath.Join(t.TempDir(), "b")
	_ = os.WriteFile(f, []byte("hello"), 0o644)
	hex := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if _, err := env.remote.UploadBlob(ctx, "mock-1", hex, f, 5); err != nil {
		t.Fatalf("upload with the lease %s: %v", res.Lease.ID, err)
	}
	for in, want := range map[string]string{"/v1/devices/a%40b/x": "a@b", "/v1/devices/a?x": "a", "/v1/status": "", "/v1/devices/%zz": ""} {
		if got := pathDevice(in); got != want {
			t.Errorf("pathDevice(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLeases_HubRunSweepsAndListensEverywhere(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	var n int32
	pr, pw := io.Pipe()
	s := New(Config{Name: "hubrun", Hub: true, HubName: "named", HTTPAddr: "127.0.0.1:0", Token: "t", Ready: pw,
		ReportsDir: filepath.Join(t.TempDir(), "r")}, testDeps(&n, 0))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	line, _ := bufio.NewReader(pr).ReadString('\n')
	go func() { _, _ = io.Copy(io.Discard, pr) }()
	addr := ""
	for _, f := range strings.Fields(line) {
		if v, ok := strings.CutPrefix(f, "http="); ok {
			addr = v
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	c := NewHTTPClient("http://"+addr, "t", nil)
	if st, err := c.Status(ctx); err != nil || st.Daemon.Hub != "named" {
		t.Fatalf("status: %+v %v", st, err)
	}
	res, err := c.AcquireLease(ctx, LeaseRequest{Device: "mock-1", TTLSec: 1})
	if err != nil {
		t.Fatal(err)
	}
	for time.Now().Before(deadline) {
		if _, err := c.Lease(ctx, res.Lease.ID); err != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := c.Lease(ctx, res.Lease.ID); err == nil {
		t.Fatal("the running hub's sweeper did not end an expired lease")
	}
	cancel()
	<-done
	if DefaultHubName() == "" || len(DefaultHubName()) > 32 || strings.ContainsAny(DefaultHubName(), "._ ") {
		t.Fatalf("DefaultHubName %q", DefaultHubName())
	}
	s2 := New(Config{Hub: true}, Deps{})
	if s2.hubName() != DefaultHubName() {
		t.Fatal("hubName defaults to the host name")
	}
}

// An install over TCP runs detached from its client (the outcome cache
// lets a retry join it), but never from its lease: expiry or a release
// cancels it at once, not after the client grace or at its end. Detaching
// the device does not stop an install, so only the lease can.
func TestLeases_EndCancelsADetachedInstallAtOnce(t *testing.T) {
	for _, end := range []string{"expiry", "release"} {
		t.Run(end, func(t *testing.T) {
			env := newHubEnv(t, Config{})
			env.s.idemGrace = time.Hour
			started := make(chan struct{}, 1)
			cancelled := make(chan struct{}, 1)
			env.s.deps.InstallApp = func(ctx context.Context, id, platform, path string) error {
				started <- struct{}{}
				select {
				case <-ctx.Done():
					cancelled <- struct{}{}
					return ctx.Err()
				case <-time.After(30 * time.Second):
					return nil
				}
			}
			ctx := context.Background()
			lease := env.lease(env.remote, LeaseRequest{Device: "mock-1", TTLSec: 5}).Lease
			apk := []byte("PK\x03\x04 an apk")
			h := sha(apk)
			if _, err := env.remote.UploadBlob(ctx, "mock-1", h, writeTemp(t, "a.apk", apk), int64(len(apk))); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				_, err := env.remote.InstallApp(ctx, "mock-1", InstallRequest{App: "blob:sha256:" + h + "/a.apk"})
				done <- err
			}()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("the install never started")
			}
			if end == "expiry" {
				env.clock.advance(6 * time.Second)
				env.s.sweepLeases()
			} else if _, err := env.local.ReleaseLease(ctx, lease.ID); err != nil {
				t.Fatal(err)
			}
			select {
			case <-cancelled:
			case <-time.After(3 * time.Second):
				t.Fatalf("the install outlived its lease (%s)", end)
			}
			if err := <-done; err == nil {
				t.Fatal("the cancelled install succeeded")
			}
		})
	}
}

// A selector boot whose candidate someone else starts between the
// selection and the boot: Registry.Boot returns the running device as it
// is, and the lease must not count it as lease-booted, so its end leaves
// the device running.
func TestLeases_SelectorBootOfADeviceStartedMeanwhile(t *testing.T) {
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	var calls atomic.Int32
	env.mu.Lock()
	env.onList = func() {
		// The selection lists once and sees SIM-DOWN shut down; by the
		// next listing (Registry.Boot's) another tool has booted it.
		if calls.Add(1) >= 2 {
			env.mu.Lock()
			env.booted["SIM-DOWN"] = true
			env.mu.Unlock()
		}
	}
	env.mu.Unlock()
	env.s.registry.boot = func(platform, name string, timeout time.Duration) (string, error) {
		t.Errorf("booted %s, which was already running", name)
		return name, nil
	}
	d := env.lease(env.remote, LeaseRequest{Select: &LeaseSelector{Name: "SIM-DOWN", Boot: true}})
	if d.Lease.Device != "SIM-DOWN" || d.Lease.Booted {
		t.Fatalf("a device the hub did not start is not lease-booted: %+v", d.Lease)
	}
	if _, err := env.remote.ReleaseLease(ctx, d.Lease.ID); err != nil {
		t.Fatal(err)
	}
	env.mu.Lock()
	defer env.mu.Unlock()
	if len(env.stopped) != 0 {
		t.Fatalf("the lease's end shut down a device someone else started: %v", env.stopped)
	}
}

// A lease that expires between leaseDevice's look for an unswept expired
// lease and grant's own check is not dropped silently: grant refuses the
// device while any lease, live or not, holds it, and the expired lease is
// ended (its requests cancelled, the device detached) before anyone gets
// the device. calls is how many clock reads still see the lease live.
func TestLeases_GrantNeverDropsAnUnendedLease(t *testing.T) {
	for _, live := range []int{1, 2} {
		t.Run(fmt.Sprint(live), func(t *testing.T) {
			env := newHubEnv(t, Config{})
			old := env.lease(env.local, LeaseRequest{Device: "mock-1", TTLSec: 5}).Lease
			entry := env.s.leases.lookup(old.ID)
			before := env.clock.now()
			var reads atomic.Int32
			env.s.leases.now = func() time.Time {
				if int(reads.Add(1)) <= live {
					return before
				}
				return before.Add(6 * time.Second)
			}
			_, e := env.s.leaseDevice(DeviceInfo{ID: "mock-1"}, "bob", 0, claimNone)
			if e == nil || e.Code != CodeDeviceInUse {
				t.Fatalf("leased a device an unended lease holds: %v", e)
			}
			select {
			case <-entry.ended:
			case <-time.After(5 * time.Second):
				if env.s.leases.lookup(old.ID) == nil {
					t.Fatal("the expired lease was dropped without ending")
				}
				t.Fatal("the expired lease was not ended")
			}
		})
	}
}

// grant refuses a device whose expired lease has not ended.
func TestLeases_GrantRefusesAnExpiredUnendedLease(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	tb := newLeaseTable()
	tb.now = clock.now
	old, e := tb.grant("d", "", "alice", time.Second, time.Hour, false)
	if e != nil {
		t.Fatal(e)
	}
	clock.advance(2 * time.Second)
	if _, e := tb.grant("d", "", "bob", time.Second, time.Hour, false); e == nil || e.Code != CodeDeviceInUse ||
		!strings.Contains(e.Message, "being released") {
		t.Fatalf("grant over an unended lease: %v", e)
	}
	if tb.lookup(old.id) != old {
		t.Fatal("grant dropped the unended lease")
	}
}

// A client that leaves while a selector lease boots its simulator gets no
// lease, and the simulator, booted for nobody, is shut down again rather
// than left running unleased.
func TestLeases_SelectorBootCancelledMidBoot(t *testing.T) {
	env := newHubEnv(t, Config{})
	gate := make(chan struct{})
	booting := make(chan struct{})
	var once sync.Once
	open := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(open)
	env.s.registry.boot = func(platform, name string, timeout time.Duration) (string, error) {
		close(booting)
		<-gate
		env.mu.Lock()
		env.booted[name] = true
		env.mu.Unlock()
		return name, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := env.local.AcquireLease(ctx, LeaseRequest{Select: &LeaseSelector{Name: "Spare Phone", Boot: true}})
		done <- err
	}()
	<-booting
	cancel()
	if err := <-done; err == nil {
		t.Fatal("a cancelled lease request succeeded")
	}
	open()
	waitFor(t, func() bool {
		env.mu.Lock()
		defer env.mu.Unlock()
		return len(env.stopped) == 1 && env.stopped[0] == "SIM-DOWN" && !env.booted["SIM-DOWN"]
	})
	if ls := env.s.leases.list(); len(ls) != 0 {
		t.Fatalf("a lease outlived its cancelled request: %+v", ls)
	}
}

// A shut-down AVD is not leased by its avd: id, which no booted emulator
// answers to: the refusal points at a selector boot or device start.
func TestLeases_ByIDRefusesAShutDownAVD(t *testing.T) {
	env := newHubEnv(t, Config{})
	env.mu.Lock()
	env.downAVD = "Pixel_9"
	env.mu.Unlock()
	_, err := env.remote.AcquireLease(context.Background(), LeaseRequest{Device: AVDPrefix + "Pixel_9"})
	de := wantErrCode(t, err, CodeUsage)
	if !strings.Contains(de.Message, "--name Pixel_9 --boot") || !strings.Contains(de.Message, "device start avd:Pixel_9") {
		t.Fatalf("message: %q", de.Message)
	}
	if ls := env.s.leases.list(); len(ls) != 0 {
		t.Fatalf("leased: %+v", ls)
	}
}

// A selector boot skips a device whose lease is still being torn down,
// though the teardown already lists it shut down: booting it would fail
// as "being released" after a whole boot, and shut it down again.
func TestLeases_SelectorBootSkipsAnEndingDevice(t *testing.T) {
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	unblock := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(unblock) }) }
	t.Cleanup(release)
	// The first shutdown (the release's teardown) lists the device shut
	// down, then blocks until the test releases it.
	stopping := make(chan struct{})
	var first atomic.Bool
	env.s.registry.shutdown = func(d DeviceInfo) error {
		env.mu.Lock()
		delete(env.booted, d.ID)
		env.stopped = append(env.stopped, d.ID)
		env.mu.Unlock()
		if first.CompareAndSwap(false, true) {
			close(stopping)
			<-unblock
		}
		return nil
	}
	l := env.lease(env.remote, LeaseRequest{Select: &LeaseSelector{Name: "Spare Phone", Boot: true}}).Lease
	released := make(chan error, 1)
	go func() {
		_, err := env.remote.ReleaseLease(ctx, l.ID)
		released <- err
	}()
	<-stopping
	var boots atomic.Int32
	env.s.registry.boot = func(platform, name string, timeout time.Duration) (string, error) {
		boots.Add(1)
		return name, nil
	}
	_, err := env.remote.AcquireLease(ctx, LeaseRequest{Select: &LeaseSelector{Name: "Spare Phone", Boot: true}})
	wantErrCode(t, err, CodeDeviceInUse)
	if n := boots.Load(); n != 0 {
		t.Fatalf("booted a device whose lease is still ending (%d boots)", n)
	}
	release()
	if err := <-released; err != nil {
		t.Fatal(err)
	}
}

// An emulator whose lease is ending is listed as its shut-down AVD
// (avd:<name>) before the teardown's shutdown returns; a selector boot
// skips that AVD too until the teardown is done.
func TestLeases_SelectorBootSkipsAnEndingAVD(t *testing.T) {
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	env.mu.Lock()
	env.emuAVD = "Pixel"
	env.mu.Unlock()
	unblock := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(unblock) }) }
	t.Cleanup(release)
	stopping := make(chan struct{})
	var first atomic.Bool
	env.s.registry.shutdown = func(d DeviceInfo) error {
		env.mu.Lock()
		env.emuAVD, env.downAVD = "", "Pixel"
		env.stopped = append(env.stopped, d.ID)
		env.mu.Unlock()
		if first.CompareAndSwap(false, true) {
			close(stopping)
			<-unblock
		}
		return nil
	}
	l := env.lease(env.remote, LeaseRequest{Device: "emulator-5554"}).Lease
	if !env.s.leases.markBooted("emulator-5554") {
		t.Fatal("no live lease on emulator-5554")
	}
	released := make(chan error, 1)
	go func() {
		_, err := env.remote.ReleaseLease(ctx, l.ID)
		released <- err
	}()
	<-stopping
	var boots atomic.Int32
	env.s.registry.boot = func(platform, name string, timeout time.Duration) (string, error) {
		boots.Add(1)
		return name, nil
	}
	_, err := env.remote.AcquireLease(ctx, LeaseRequest{Select: &LeaseSelector{Name: "Pixel", Boot: true}})
	wantErrCode(t, err, CodeDeviceInUse)
	if n := boots.Load(); n != 0 {
		t.Fatalf("booted an AVD whose emulator's lease is still ending (%d boots)", n)
	}
	release()
	if err := <-released; err != nil {
		t.Fatal(err)
	}
	env.s.leases.mu.Lock()
	left := len(env.s.leases.ending)
	env.s.leases.mu.Unlock()
	if left != 0 {
		t.Fatalf("ending reservations left after the teardown: %d", left)
	}
}

// Ending a lease-booted emulator's lease shuts its serial down only while
// it still runs the leased AVD: once that emulator died, another AVD that
// took the same port belongs to nobody's lease and is left running.
func TestLeases_TeardownSkipsAnotherAVDOnTheSerial(t *testing.T) {
	for _, tc := range []struct {
		name, later string
		stopped     bool
	}{
		{"same AVD", "Pixel", true},
		{"another AVD", "Tablet", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newHubEnv(t, Config{})
			env.mu.Lock()
			env.emuAVD = "Pixel"
			env.mu.Unlock()
			l := env.lease(env.remote, LeaseRequest{Device: "emulator-5554"}).Lease
			if !env.s.leases.markBooted("emulator-5554") {
				t.Fatal("no live lease on emulator-5554")
			}
			env.mu.Lock()
			env.emuAVD = tc.later
			env.mu.Unlock()
			if _, err := env.remote.ReleaseLease(context.Background(), l.ID); err != nil {
				t.Fatal(err)
			}
			env.mu.Lock()
			stopped := append([]string(nil), env.stopped...)
			env.mu.Unlock()
			if got := len(stopped) == 1 && stopped[0] == "emulator-5554"; got != tc.stopped || (!tc.stopped && len(stopped) != 0) {
				t.Fatalf("emulator-5554 running %s at the lease's end: stopped %v, want stopped=%v", tc.later, stopped, tc.stopped)
			}
		})
	}
}

// With per-device maximums, granting a lease runs no device discovery
// while it holds the hub-wide leaseMu: a selector with several candidates
// would hold every other grant, release and the sweeper for seconds.
func TestLeases_NoDiscoveryUnderLeaseMu(t *testing.T) {
	env := newHubEnv(t, Config{LeaseMaxDevices: []LeaseMax{{Device: "mock-2", Max: time.Hour}, {Device: "Spare Phone", Max: time.Hour}}})
	var underLock atomic.Int32
	env.mu.Lock()
	env.onList = func() {
		if !env.s.leaseMu.TryLock() {
			underLock.Add(1)
			return
		}
		env.s.leaseMu.Unlock()
	}
	env.mu.Unlock()
	a := env.lease(env.remote, LeaseRequest{Device: "mock-2"}).Lease
	b := env.lease(env.remote, LeaseRequest{Select: &LeaseSelector{Platform: "mock"}}).Lease
	c := env.lease(env.remote, LeaseRequest{Select: &LeaseSelector{Name: "Spare Phone", Boot: true}}).Lease
	if n := underLock.Load(); n != 0 {
		t.Fatalf("%d device listings ran under leaseMu", n)
	}
	if a.MaxSec != 3600 || b.Device != "mock-1" || b.MaxSec != 8*3600 || c.Device != "SIM-DOWN" || c.MaxSec != 3600 {
		t.Fatalf("maximums: %+v %+v %+v", a, b, c)
	}
}

// A ttlSec too large for a Duration is USAGE over the maximum, on a lease
// and on a renew, never a window that wrapped into the past.
func TestLeases_HugeTTLSecIsUsage(t *testing.T) {
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	const huge = 9223372037 // time.Duration(huge) * time.Second wraps negative
	for _, req := range []LeaseRequest{{Device: "mock-1", TTLSec: huge}, {Select: &LeaseSelector{Platform: "mock"}, TTLSec: huge}} {
		_, err := env.remote.AcquireLease(ctx, req)
		if de := wantErrCode(t, err, CodeUsage); de.Details["ttlSec"] != float64(huge) {
			t.Fatalf("%+v: details %+v", req, de.Details)
		}
	}
	l := env.lease(env.remote, LeaseRequest{Device: "mock-1", TTLSec: 60}).Lease
	_, err := env.remote.RenewLease(ctx, l.ID, huge)
	wantErrCode(t, err, CodeUsage)
	if got, err := env.remote.Lease(ctx, l.ID); err != nil || got.Lease.ExpiresAt != l.ExpiresAt {
		t.Fatalf("a refused renew moved or ended the lease: %+v %v", got, err)
	}
}

// A retry joined to a running attempt runs under the lease too, so the
// lease ending cancels its context. Its client is still connected: it gets
// the running attempt's outcome, never an empty 200.
func TestLeases_JoinedRetryAnsweredWhenTheLeaseEnds(t *testing.T) {
	env := newHubEnv(t, Config{})
	env.mu.Lock()
	env.stepDelay = 5 * time.Second
	env.mu.Unlock()
	l := env.lease(env.remote, LeaseRequest{Device: "mock-1", Attach: &AttachConfig{Platform: "mock"}}).Lease
	hs := httptest.NewServer(fromAddr("192.0.2.20:40000", env.s.TCPHandler()))
	t.Cleanup(hs.Close)
	type answer struct {
		status   int
		replayed string
		body     string
		err      error
	}
	post := func() chan answer {
		out := make(chan answer, 1)
		go func() {
			req, _ := http.NewRequest(http.MethodPost, hs.URL+"/v1/devices/mock-1/commands/runFlow", strings.NewReader(slowCount("n")))
			req.Header.Set("Authorization", "Bearer t")
			req.Header.Set(HeaderRequestID, "lost-answer")
			req.Header.Set(HeaderLease, l.ID)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				out <- answer{err: err}
				return
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			out <- answer{status: resp.StatusCode, replayed: resp.Header.Get(HeaderReplayed), body: string(b)}
		}()
		return out
	}
	entry := func() *idemEntry {
		env.s.idem.mu.Lock()
		defer env.s.idem.mu.Unlock()
		if d := env.s.idem.devices["mock-1"]; d != nil {
			return d.entries["lost-answer"]
		}
		return nil
	}
	first := post()
	waitFor(t, func() bool { return entry() != nil })
	retry := post()
	waitFor(t, func() bool { return entry().joined.Load() })
	if _, err := env.remote.ReleaseLease(context.Background(), l.ID); err != nil {
		t.Fatal(err)
	}
	got := <-retry
	if got.err != nil {
		t.Fatal(got.err)
	}
	if got.body == "" || got.replayed != "true" {
		t.Fatalf("the joined retry: status %d, replayed %q, body %q", got.status, got.replayed, got.body)
	}
	if want := <-first; want.err != nil || want.status != got.status || want.body != got.body {
		t.Fatalf("the retry's answer differs from the attempt's: %+v vs %+v", got, want)
	}
	// The teardown writes the session's report as it detaches: wait for
	// it before the report directory is removed.
	waitFor(t, func() bool { return !env.s.leases.releasing(DeviceInfo{ID: "mock-1"}) })
}

// The outcome cache sits inside the lease guard: a retry of a leased
// request is checked for the lease like any attempt, so a caller without
// the lease is refused, never answered from the cache.
func TestLeases_CacheReplayNeedsTheLease(t *testing.T) {
	env := newHubEnv(t, Config{})
	l := env.lease(env.remote, LeaseRequest{Device: "mock-1", Attach: &AttachConfig{Platform: "mock"}}).Lease
	hs := httptest.NewServer(fromAddr("192.0.2.20:40000", env.s.TCPHandler()))
	t.Cleanup(hs.Close)
	post := func(lease string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, hs.URL+"/v1/devices/mock-1/commands/runFlow", strings.NewReader(slowCount("n")))
		req.Header.Set("Authorization", "Bearer t")
		req.Header.Set(HeaderRequestID, "one-request")
		if lease != "" {
			req.Header.Set(HeaderLease, lease)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp
	}
	if resp := post(l.ID); resp.StatusCode != http.StatusOK {
		t.Fatalf("with the lease: %d", resp.StatusCode)
	}
	if resp := post(""); resp.StatusCode == http.StatusOK || resp.Header.Get(HeaderReplayed) != "" {
		t.Fatalf("a retry without the lease was answered (%d, replayed %q)", resp.StatusCode, resp.Header.Get(HeaderReplayed))
	}
	if resp := post(l.ID); resp.StatusCode != http.StatusOK || resp.Header.Get(HeaderReplayed) != "true" {
		t.Fatalf("a retry with the lease: %d, replayed %q", resp.StatusCode, resp.Header.Get(HeaderReplayed))
	}
}

// Stopping a leased device without deleting it ends its lease too, once:
// the device is shut down by the stop, never again by the lease's
// teardown, and the lease's end is the documented lease.released.
func TestLeases_StopEndsTheLease(t *testing.T) {
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	env.lease(env.remote, LeaseRequest{Select: &LeaseSelector{Name: "Spare Phone", Boot: true}, Holder: "alice"})
	if err := env.remote.StopDevice(ctx, "SIM-DOWN", false); err != nil {
		t.Fatal(err)
	}
	if ls, _ := env.remote.Leases(ctx); len(ls) != 0 {
		t.Fatalf("lease after stop: %+v", ls)
	}
	waitFor(t, func() bool { return !env.s.leases.releasing(DeviceInfo{ID: "SIM-DOWN"}) })
	env.mu.Lock()
	stopped := strings.Join(env.stopped, " ")
	env.mu.Unlock()
	if stopped != "SIM-DOWN" {
		t.Fatalf("stopped = %q, want SIM-DOWN once", stopped)
	}
	// A device the hub did not boot, stopped with force.
	env.lease(env.remote, LeaseRequest{Device: "mock-1"})
	if err := env.remote.StopDevice(ctx, "mock-1", true); err != nil {
		t.Fatal(err)
	}
	if ls, _ := env.remote.Leases(ctx); len(ls) != 0 {
		t.Fatalf("lease after a forced stop: %+v", ls)
	}
	waitFor(t, func() bool { return !env.s.leases.releasing(DeviceInfo{ID: "mock-1"}) })
	got := strings.Join(env.leaseEvents(), " ")
	if want := "lease.granted:SIM-DOWN lease.released:SIM-DOWN lease.granted:mock-1 lease.released:mock-1"; got != want {
		t.Fatalf("lease events %q, want %q", got, want)
	}
	// A failed stop leaves the lease alone.
	env.lease(env.remote, LeaseRequest{Device: "mock-2"})
	err := env.remote.StopDevice(ctx, "mock-2", false)
	wantErrCode(t, err, CodeDeviceExternal)
	if ls, _ := env.remote.Leases(ctx); len(ls) != 1 {
		t.Fatalf("a failed stop ended the lease: %+v", ls)
	}
}

// Every way a lease ends publishes one of the two documented events.
func TestLeases_EndEventsAreDocumentedTypes(t *testing.T) {
	for reason, want := range map[string]string{
		endExpired: "lease.expired", endReleased: "lease.released", endAttachFailed: "lease.released",
		endStopped: "lease.released", endDeleted: "lease.released", endHubStopped: "lease.released",
	} {
		if got := leaseEndEvent(reason); got != want {
			t.Errorf("%s: %s, want %s", reason, got, want)
		}
	}
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	if err := recordCreated(CreatedRecord{Platform: "ios", ID: "SIM-UP", Name: "Shared Phone"}); err != nil {
		t.Fatal(err)
	}
	env.s.registry.deps.DeleteSimulator = func(string) error { return nil }
	env.lease(env.remote, LeaseRequest{Device: "SIM-UP"})
	if _, err := env.remote.DeleteDevice(ctx, "SIM-UP", false); err != nil {
		t.Fatal(err)
	}
	env.s.deps.NewDriver = func(context.Context, string, AttachConfig) (core.Driver, DriverInfo, func(), error) {
		return nil, DriverInfo{}, nil, io.ErrUnexpectedEOF
	}
	_, err := env.remote.AcquireLease(ctx, LeaseRequest{Device: "mock-1", Attach: &AttachConfig{Platform: "mock"}})
	wantErrCode(t, err, CodeDeviceError)
	waitFor(t, func() bool {
		return !env.s.leases.releasing(DeviceInfo{ID: "SIM-UP"}) && !env.s.leases.releasing(DeviceInfo{ID: "mock-1"})
	})
	got := strings.Join(env.leaseEvents(), " ")
	if want := "lease.granted:SIM-UP lease.released:SIM-UP lease.granted:mock-1 lease.released:mock-1"; got != want {
		t.Fatalf("lease events %q, want %q", got, want)
	}
}

// A by-id lease on a running emulator is refused while a selector boots
// its AVD, or a teardown shuts it down: both reserve it under the AVD's
// key, not the serial.
func TestLeases_ByIDRespectsTheAVDReservation(t *testing.T) {
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	env.mu.Lock()
	env.emuAVD = "Pixel"
	env.mu.Unlock()
	for _, m := range []map[string]bool{env.s.leases.booting, env.s.leases.ending} {
		env.s.leases.mu.Lock()
		m["android:pixel"] = true
		env.s.leases.mu.Unlock()
		_, err := env.remote.AcquireLease(ctx, LeaseRequest{Device: "emulator-5554"})
		wantErrCode(t, err, CodeDeviceInUse)
		env.s.leases.mu.Lock()
		delete(m, "android:pixel")
		env.s.leases.mu.Unlock()
	}
	env.lease(env.remote, LeaseRequest{Device: "emulator-5554"})
}

// The holder's stop and the teardown of its lease, which expired while
// the stop ran, never both shut the device down: the teardown waits for
// the stop and leaves the device alone, and the holder's stop answers for
// itself. Meanwhile a stop without the lease is refused.
func TestLeases_StopBesideTheLeaseTeardown(t *testing.T) {
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	stopping := make(chan string, 4)
	unblock := make(chan struct{})
	var stops atomic.Int32
	env.s.registry.shutdown = func(d DeviceInfo) error {
		stops.Add(1)
		stopping <- d.ID
		<-unblock
		env.mu.Lock()
		delete(env.booted, d.ID)
		env.mu.Unlock()
		return nil
	}
	var once sync.Once
	defer once.Do(func() { close(unblock) })
	env.lease(env.remote, LeaseRequest{Select: &LeaseSelector{Name: "Spare Phone", Boot: true}, TTLSec: 10})
	done := make(chan error, 1)
	go func() { done <- env.remote.StopDevice(ctx, "SIM-DOWN", false) }()
	select {
	case <-stopping:
	case <-time.After(5 * time.Second):
		t.Fatal("the holder's stop never started")
	}
	env.clock.advance(11 * time.Second)
	go env.s.sweepLeases()
	select {
	case id := <-stopping:
		t.Fatalf("the teardown stopped %s beside the holder's stop", id)
	case <-time.After(300 * time.Millisecond):
	}
	err := env.local.StopDevice(ctx, "SIM-DOWN", false)
	wantErrCode(t, err, CodeDeviceInUse)
	once.Do(func() { close(unblock) })
	if err := <-done; err != nil {
		t.Fatalf("the holder's stop: %v", err)
	}
	waitFor(t, func() bool { return !env.s.leases.releasing(DeviceInfo{ID: "SIM-DOWN"}) })
	if n := stops.Load(); n != 1 {
		t.Fatalf("the device was shut down %d times", n)
	}
	if got := strings.Join(env.leaseEvents(), " "); got != "lease.granted:SIM-DOWN lease.expired:SIM-DOWN" {
		t.Fatalf("lease events %q", got)
	}
}

// A request under a lease whose body upload stalls is answered
// LEASE_EXPIRED after the grace: the stalled body read holds no lock the
// answer needs.
func TestLeases_StalledUploadAnsweredWhenTheLeaseEnds(t *testing.T) {
	env := newHubEnv(t, Config{})
	l := env.lease(env.remote, LeaseRequest{Device: "mock-1"}).Lease
	u, err := url.Parse(env.remote.BaseURL())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "PUT /v1/devices/mock-1/blobs/%s HTTP/1.1\r\nHost: hub\r\nAuthorization: Bearer t\r\n%s: %s\r\nContent-Length: 100\r\n\r\nabc",
		strings.Repeat("a", 64), HeaderLease, l.ID)
	waitFor(t, func() bool { return env.s.leases.lookup(l.ID).inFlight.Load() == 1 })
	if _, err := env.local.ReleaseLease(context.Background(), l.ID); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(leaseUnwindGrace + 4*time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no answer while the upload stalls: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if code := errorFromResponse(resp.StatusCode, resp.Header, data).Code; code != CodeLeaseExpired {
		t.Fatalf("answer %d %s: %s", resp.StatusCode, code, data)
	}
	// The stalled read did not outlive the request: with the client
	// still connected and silent, the upload's handler unwound, and the
	// lease's teardown freed the device.
	waitFor(t, func() bool { return !env.s.leases.releasing(DeviceInfo{ID: "mock-1"}) })
}

// Stopping a hub ends its leases first: the requests under them are
// cancelled, and every lease publishes its end.
func TestLeases_HubShutdownEndsItsLeases(t *testing.T) {
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	env.mu.Lock()
	env.stepDelay = 30 * time.Second
	env.mu.Unlock()
	env.lease(env.remote, LeaseRequest{Device: "mock-1", Attach: &AttachConfig{Platform: "mock"}})
	env.lease(env.remote, LeaseRequest{Device: "mock-2"})
	done := make(chan error, 1)
	go func() {
		_, err := env.remote.Command(ctx, "mock-1", "tapOn", "slow", true)
		done <- err
	}()
	waitFor(t, func() bool {
		devs, _ := env.local.Devices(ctx, "mock")
		return len(devs) > 0 && devs[0].Busy
	})
	env.s.Shutdown("test")
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a step under a lease outlived the hub's stop")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the step under the lease was not cancelled")
	}
	select {
	case <-env.s.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the hub never finished stopping")
	}
	if ls := env.s.leases.list(); len(ls) != 0 {
		t.Fatalf("leases after the hub stopped: %+v", ls)
	}
	got := env.leaseEvents()
	sort.Strings(got)
	if strings.Join(got, " ") != "lease.granted:mock-1 lease.granted:mock-2 lease.released:mock-1 lease.released:mock-2" {
		t.Fatalf("lease events %v", got)
	}
}

// eventIndex is the position of the first event of type typ on device
// among the hub's recent events, or -1.
func (e *hubEnv) eventIndex(typ, device string) int {
	for i, ev := range e.s.events.Recent("", 0) {
		if ev.Type == typ && ev.Device == device {
			return i
		}
	}
	return -1
}

// A stopping hub waits for every lease teardown, one that waits for work
// that ignores its cancellation included, before it detaches and shuts
// down its devices: the device the lease booted is shut down once, by the
// teardown, and the lease's end goes out before daemon.shutdown.
func TestLeases_ShutdownWaitsForTeardownsOfStubbornWork(t *testing.T) {
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	var stops atomic.Int32
	env.s.registry.shutdown = func(d DeviceInfo) error {
		stops.Add(1)
		env.mu.Lock()
		delete(env.booted, d.ID)
		env.mu.Unlock()
		return nil
	}
	started := make(chan struct{}, 1)
	unblock := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(unblock) })
	env.mu.Lock()
	env.onStep = func() {
		select {
		case started <- struct{}{}:
		default:
		}
		<-unblock
	}
	env.mu.Unlock()
	env.lease(env.remote, LeaseRequest{Select: &LeaseSelector{Name: "Spare Phone", Boot: true}, Attach: &AttachConfig{Platform: "mock"}})
	go func() { _, _ = env.remote.Command(ctx, "SIM-DOWN", "tapOn", "stuck", true) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the step never started")
	}
	env.s.Shutdown("test")
	select {
	case <-env.s.Done():
		t.Fatal("the hub stopped while a lease teardown waited for work under it")
	case <-time.After(500 * time.Millisecond):
	}
	if n := stops.Load(); n != 0 {
		t.Fatalf("the device was shut down %d times before its teardown", n)
	}
	env.s.mu.Lock()
	_, attached := env.s.attached["SIM-DOWN"]
	env.s.mu.Unlock()
	if !attached {
		t.Fatal("the hub detached the device beside its lease's teardown")
	}
	once.Do(func() { close(unblock) })
	select {
	case <-env.s.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the hub never finished stopping")
	}
	if n := stops.Load(); n != 1 {
		t.Fatalf("the device was shut down %d times, want once", n)
	}
	if env.eventIndex("lease.released", "SIM-DOWN") < 0 {
		t.Fatal("the hub stopped before its lease's end was published")
	}
}

// The teardown bound: a stopping hub does not wait for good.
func TestLeases_ShutdownWaitIsBounded(t *testing.T) {
	prev := leaseShutdownWait
	leaseShutdownWait = 100 * time.Millisecond
	t.Cleanup(func() { leaseShutdownWait = prev })
	var p pending
	p.add()
	if p.wait(leaseShutdownWait) {
		t.Fatal("wait returned true with work pending")
	}
	p.done()
	if !p.wait(leaseShutdownWait) {
		t.Fatal("wait returned false with nothing pending")
	}
}

// A leased stop or delete finishes its lease's teardown before it
// answers: the lease's end is published before the stop's or delete's
// own event, and the device can be leased again at once.
func TestLeases_LeasedStopAndDeleteFinishTheirTeardown(t *testing.T) {
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	if err := recordCreated(CreatedRecord{Platform: "ios", ID: "SIM-UP", Name: "Shared Phone"}); err != nil {
		t.Fatal(err)
	}
	env.s.registry.deps.DeleteSimulator = func(string) error { return nil }
	for i := range 5 {
		env.lease(env.remote, LeaseRequest{Device: "mock-1"})
		if err := env.remote.StopDevice(ctx, "mock-1", true); err != nil {
			t.Fatal(err)
		}
		if env.s.leases.releasing(DeviceInfo{ID: "mock-1"}) {
			t.Fatalf("stop %d answered before its lease's teardown finished", i)
		}
		env.lease(env.remote, LeaseRequest{Device: "mock-1"})
		if _, err := env.remote.ReleaseLease(ctx, env.s.leases.list()[0].ID); err != nil {
			t.Fatal(err)
		}
	}
	if rel, stop := env.eventIndex("lease.released", "mock-1"), env.eventIndex("device.stopped", "mock-1"); rel < 0 || stop < rel {
		t.Fatalf("lease.released at %d, device.stopped at %d", rel, stop)
	}
	env.lease(env.remote, LeaseRequest{Device: "SIM-UP"})
	if _, err := env.remote.DeleteDevice(ctx, "SIM-UP", false); err != nil {
		t.Fatal(err)
	}
	if env.s.leases.releasing(DeviceInfo{ID: "SIM-UP"}) {
		t.Fatal("delete answered before its lease's teardown finished")
	}
	env.lease(env.remote, LeaseRequest{Device: "SIM-UP"})
	if rel, del := env.eventIndex("lease.released", "SIM-UP"), env.eventIndex("device.deleted", "SIM-UP"); rel < 0 || del < rel {
		t.Fatalf("lease.released at %d, device.deleted at %d", rel, del)
	}
}

// A stop or delete without a lease is refused while an emulator's AVD is
// reserved, by its AVD key: a selector booting it, or a teardown shutting
// it down. The AVD is checked under its avd: id and as the running
// emulator.
func TestLeases_UnleasedStopRespectsTheAVDReservation(t *testing.T) {
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	env.s.registry.deps.DeleteAVD = func(string) error { t.Error("deleted a reserved AVD"); return nil }
	for _, m := range []map[string]bool{env.s.leases.booting, env.s.leases.ending} {
		env.s.leases.mu.Lock()
		m["android:pixel"] = true
		env.s.leases.mu.Unlock()
		env.mu.Lock()
		env.downAVD, env.emuAVD = "Pixel", ""
		env.mu.Unlock()
		_, err := env.local.DeleteDevice(ctx, "avd:Pixel", false)
		wantErrCode(t, err, CodeDeviceInUse)
		env.mu.Lock()
		env.downAVD, env.emuAVD = "", "Pixel"
		env.mu.Unlock()
		wantErrCode(t, env.local.StopDevice(ctx, "emulator-5554", false), CodeDeviceInUse)
		env.s.leases.mu.Lock()
		delete(m, "android:pixel")
		env.s.leases.mu.Unlock()
	}
}

// A delete that shut the device down and then failed ends the device's
// lease as a stop does: the lease's teardown never shuts it down again.
func TestLeases_FailedDeleteAfterShutdownEndsTheLease(t *testing.T) {
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	prev := shutdownForDelete
	t.Cleanup(func() { shutdownForDelete = prev })
	var shutdowns atomic.Int32
	shutdownForDelete = func(d DeviceInfo) error {
		shutdowns.Add(1)
		env.mu.Lock()
		delete(env.booted, d.ID)
		env.mu.Unlock()
		return nil
	}
	if err := recordCreated(CreatedRecord{Platform: "ios", ID: "SIM-DOWN", Name: "Spare Phone"}); err != nil {
		t.Fatal(err)
	}
	env.s.registry.deps.DeleteSimulator = func(string) error { return io.ErrUnexpectedEOF }
	env.lease(env.remote, LeaseRequest{Select: &LeaseSelector{Name: "Spare Phone", Boot: true}})
	_, err := env.remote.DeleteDevice(ctx, "SIM-DOWN", false)
	wantErrCode(t, err, CodeDeviceError)
	if shutdowns.Load() != 1 {
		t.Fatalf("delete shut the device down %d times", shutdowns.Load())
	}
	if ls := env.s.leases.list(); len(ls) != 0 {
		t.Fatalf("lease after a delete that shut the device down: %+v", ls)
	}
	waitFor(t, func() bool { return !env.s.leases.releasing(DeviceInfo{ID: "SIM-DOWN"}) })
	env.mu.Lock()
	stopped := append([]string(nil), env.stopped...)
	env.mu.Unlock()
	if len(stopped) != 0 {
		t.Fatalf("the lease's teardown shut the device down again: %v", stopped)
	}
	// A delete that fails before it shut anything down leaves the lease.
	env.lease(env.remote, LeaseRequest{Device: "mock-1"})
	_, err = env.remote.DeleteDevice(ctx, "mock-1", false)
	wantErrCode(t, err, CodeUsage)
	if ls := env.s.leases.list(); len(ls) != 1 {
		t.Fatalf("a delete that stopped nothing ended the lease: %+v", ls)
	}
}

// trackedBody is a request body whose Read blocks until it is closed, and
// records that a Read returned.
type trackedBody struct {
	pr       *io.PipeReader
	returned atomic.Bool
}

func (b *trackedBody) Read(p []byte) (int, error) {
	n, err := b.pr.Read(p)
	b.returned.Store(true)
	return n, err
}

func (b *trackedBody) Close() error { return b.pr.Close() }

// A body read still pending when serveLeased answers without its handler
// returns before serveLeased does: net/http forbids reading the body after
// ServeHTTP returned. A writer without read deadlines (a request a peer
// hub relays) gets its body closed instead.
func TestLeases_PendingBodyReadEndsBeforeServeLeasedReturns(t *testing.T) {
	prev := leaseUnwindGrace
	leaseUnwindGrace = 50 * time.Millisecond
	t.Cleanup(func() { leaseUnwindGrace = prev })
	env := newHubEnv(t, Config{})
	e := env.s.leases.lookup(env.lease(env.remote, LeaseRequest{Device: "mock-1"}).Lease.ID)
	pr, pw := io.Pipe()
	defer pw.Close()
	body := &trackedBody{pr: pr}
	req := httptest.NewRequest(http.MethodPut, "/v1/devices/mock-1/blobs/x", nil)
	req.Body = body
	reading := make(chan struct{})
	h := func(w http.ResponseWriter, r *http.Request) {
		close(reading)
		_, _ = io.ReadAll(r.Body)
	}
	rec := httptest.NewRecorder()
	served := make(chan struct{})
	go func() {
		env.s.serveLeased(rec, req, e, h)
		close(served)
	}()
	<-reading
	env.s.endLease(e, endReleased)
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("serveLeased never answered")
	}
	if !body.returned.Load() {
		t.Fatal("the body read was still pending when serveLeased returned")
	}
	if code := errorFromResponse(rec.Code, rec.Header(), rec.Body.Bytes()).Code; code != CodeLeaseExpired {
		t.Fatalf("answer %d %s", rec.Code, rec.Body.Bytes())
	}
	waitFor(t, func() bool { return !env.s.leases.releasing(DeviceInfo{ID: "mock-1"}) })
}
