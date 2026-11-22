package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/cloudprov"
	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/driver/mock"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

// fakeRemoteDriver is a held cloud driver: a mock driver (without its
// screen capture) with counted keepalives, restarts and closes.
type fakeRemoteDriver struct {
	core.Driver
	pings, restarts, closes atomic.Int32
	pingErr, restartErr     atomic.Pointer[error]
	// pingStarted, when set, is closed by the first ping.
	inPing atomic.Bool
	// installChecks counts AppInstalled calls; the first notYet of them
	// answer false (a negative notYet: always), and checkErr fails them.
	installChecks, notYet atomic.Int32
	checkErr              atomic.Pointer[error]
	// restartFails makes that many restarts fail first; restartDelay
	// makes each restart take that long.
	restartFails atomic.Int32
	restartDelay atomic.Int64
	restartedAt  atomic.Int64 // when the last restart finished (UnixNano)
}

func (d *fakeRemoteDriver) Keepalive() error {
	d.inPing.Store(true)
	defer d.inPing.Store(false)
	d.pings.Add(1)
	if p := d.pingErr.Load(); p != nil {
		return *p
	}
	return nil
}
func (d *fakeRemoteDriver) RestartSession() error {
	d.restarts.Add(1)
	time.Sleep(time.Duration(d.restartDelay.Load()))
	defer func() { d.restartedAt.Store(time.Now().UnixNano()) }()
	if p := d.restartErr.Load(); p != nil {
		return *p
	}
	if d.restartFails.Add(-1) >= 0 {
		return errors.New("UiAutomation not connected")
	}
	return nil
}
func (d *fakeRemoteDriver) AppInstalled(string) (bool, error) {
	n := d.installChecks.Add(1)
	if p := d.checkErr.Load(); p != nil {
		return false, *p
	}
	ny := d.notYet.Load()
	return ny >= 0 && n > ny, nil
}
func (d *fakeRemoteDriver) Close() error { d.closes.Add(1); return nil }

// Execute answers recording steps as the Appium driver does: it has no
// screen capture.
func (d *fakeRemoteDriver) Execute(step flow.Step) *core.CommandResult {
	switch step.(type) {
	case *flow.StartRecordingStep, *flow.StopRecordingStep:
		err := fmt.Errorf("unsupported step type: %T", step)
		return &core.CommandResult{Success: false, Error: err, Message: err.Error()}
	}
	return d.Driver.Execute(step)
}

// cloudFake is an in-process provider whose Allocate runs the hooks and
// records what it did.
type cloudFake struct {
	*fakeProvider
	mu         sync.Mutex
	refs       []string
	released   []string
	abandoned  []string
	state      string // Refresh's answer (default RUNNING)
	refreshErr error  // Refresh's error, when set
	expires    time.Duration
	allocErr   error
	releaseFn  func(ref string) error
	// failAfterCreated fails Allocate after the session was recorded,
	// without stopping it (a stop the provider refused); hold, when set,
	// blocks Allocate after recording until it is closed or the request
	// is cancelled.
	failAfterCreated error
	hold             chan struct{}
}

func newCloudFake() *cloudFake {
	return &cloudFake{fakeProvider: newFakeProvider(), state: "RUNNING", expires: 150 * time.Minute}
}

func (f *cloudFake) Allocate(ctx context.Context, spec cloudprov.CloudSpec, h cloudprov.AllocateHooks) (*cloudprov.Allocation, error) {
	f.call("Allocate")
	if f.allocErr != nil {
		return nil, f.allocErr
	}
	ref := "arn:aws:devicefarm:us-west-2:1:session:p/" + h.DeviceID + "/00000"
	if h.Progress != nil {
		h.Progress("PENDING")
	}
	if err := h.Created(ref); err != nil {
		f.mu.Lock()
		f.released = append(f.released, ref)
		f.mu.Unlock()
		h.Abandoned(ref)
		return nil, cloudprov.Errorf(cloudprov.Other, "cannot record the cloud session; it was stopped (%v)", err)
	}
	f.mu.Lock()
	f.refs = append(f.refs, ref)
	f.mu.Unlock()
	if f.failAfterCreated != nil {
		return nil, f.failAfterCreated
	}
	if f.hold != nil {
		select {
		case <-f.hold:
		case <-ctx.Done():
		}
	}
	if ctx.Err() != nil {
		h.Abandoned(ref)
		return nil, cloudprov.Errorf(cloudprov.Other, "cancelled")
	}
	now := time.Now()
	return &cloudprov.Allocation{Ref: ref, Endpoint: "https://example.invalid/remote-endpoint/SECRETTOKEN/wd/hub",
		Caps: map[string]any{"platformName": "Android"}, Started: now, Expires: now.Add(f.expires),
		Device: cloudprov.DeviceDesc{Model: "Google Pixel 8", Platform: spec.Platform, OSVersion: "14", FormFactor: "phone", Manufacturer: "Google",
			DashboardURL: "https://console.aws.amazon.com/devicefarm/home#/x", AppiumVersion: "2"}}, nil
}

func (f *cloudFake) Refresh(context.Context, *cloudprov.Allocation) (string, string, error) {
	f.call("Refresh")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refreshErr != nil {
		return "", "", f.refreshErr
	}
	return f.state, "", nil
}

func (f *cloudFake) Release(_ context.Context, a *cloudprov.Allocation) (*cloudprov.ReleaseResult, error) {
	f.call("Release")
	if f.releaseFn != nil {
		if err := f.releaseFn(a.Ref); err != nil {
			return nil, err
		}
	}
	f.mu.Lock()
	f.released = append(f.released, a.Ref)
	f.mu.Unlock()
	return &cloudprov.ReleaseResult{Minutes: &cloudprov.DeviceMinutes{Metered: 2, Total: 2}}, nil
}

func (f *cloudFake) ReleaseRef(_ context.Context, ref string) (*cloudprov.DeviceMinutes, error) {
	f.call("ReleaseRef")
	if f.releaseFn != nil {
		if err := f.releaseFn(ref); err != nil {
			return nil, err
		}
	}
	f.mu.Lock()
	f.released = append(f.released, ref)
	f.mu.Unlock()
	return &cloudprov.DeviceMinutes{Metered: 4, Total: 4, Estimated: true}, nil
}

func (f *cloudFake) releasedRefs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.released...)
}

// remoteEnv is a plain test daemon with a devicefarm cloudFake and a fake
// remote driver for every launch.
type remoteEnv struct {
	*testEnv
	f       *cloudFake
	drivers []*fakeRemoteDriver
	mu      sync.Mutex
	// stepDelay slows every step of a launched device.
	stepDelay time.Duration
}

func newRemoteEnv(t *testing.T, cfg func(*Config)) *remoteEnv {
	t.Helper()
	t.Setenv(EnvHome, t.TempDir())
	t.Setenv(envFastCloud, "1")
	e := &remoteEnv{testEnv: &testEnv{t: t}, f: newCloudFake()}
	deps := testDeps(&e.cleanups, 0)
	deps.Providers = map[string]func() (cloudprov.RemoteProvider, error){
		"devicefarm": func() (cloudprov.RemoteProvider, error) { return e.f, nil },
	}
	deps.NewRemoteDriver = func(ctx context.Context, endpoint string, caps map[string]any) (RemoteDriver, DriverInfo, error) {
		if strings.Contains(endpoint, "SECRETTOKEN") {
			return nil, DriverInfo{}, errors.New("the driver saw the provider's endpoint")
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		d := &fakeRemoteDriver{Driver: mock.New(mock.Config{Platform: "android", StepDelay: e.stepDelay})}
		e.drivers = append(e.drivers, d)
		return d, DriverInfo{DriverName: "appium", Platform: "android", Kind: "cloud"}, nil
	}
	c := Config{Name: "test-" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-")), ReportsDir: filepath.Join(t.TempDir(), "reports")}
	if cfg != nil {
		cfg(&c)
	}
	e.s = New(c, deps)
	e.s.info = Info{Name: e.s.cfg.Name, PID: os.Getpid(), APIVersion: APIVersion, Version: "test"}
	hs := httptest.NewServer(e.s.Handler())
	t.Cleanup(hs.Close)
	t.Cleanup(func() { e.s.releaseAllRemote("test-end") })
	e.c = NewHTTPClient(hs.URL, "", nil)
	return e
}

func (e *remoteEnv) launch(spec cloudprov.CloudSpec) *DeviceInfo {
	e.t.Helper()
	if spec.Platform == "" {
		spec.Platform = "android"
	}
	dev, err := e.c.CreateDevice(context.Background(), BootRequest{Create: &CreateSpec{Cloud: &spec}})
	if err != nil {
		e.t.Fatalf("launch: %v", err)
	}
	return dev
}

func (e *remoteEnv) driver(i int) *fakeRemoteDriver {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.drivers[i]
}

func waitUntil(t *testing.T, d time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRemote_LaunchDriveStop(t *testing.T) {
	e := newRemoteEnv(t, nil)
	ctx := context.Background()
	dev := e.launch(cloudprov.CloudSpec{Name: "Google Pixel 8", AppID: "org.example", IdleReleaseSec: 900})
	if !strings.HasPrefix(dev.ID, "df-") || dev.Kind != "cloud" || dev.State != "allocated" || dev.Location != LocationCloud ||
		dev.IdleReleaseSec != 900 || dev.ExpiresAt == "" || dev.IdleReleaseAt == "" || dev.ProviderRef == "" || dev.CreatedBy != CreatedByMaestroD {
		t.Fatalf("launched: %+v", dev)
	}
	// Recorded in the ledger and devices.json, with no endpoint anywhere.
	entries, _ := ReadLedger()
	if len(entries) != 1 || entries[0].ID != dev.ID || entries[0].Daemon != e.s.cfg.Name || entries[0].PID != os.Getpid() {
		t.Fatalf("ledger: %+v", entries)
	}
	data, _ := os.ReadFile(DevicesPath(e.s.cfg.Name))
	if !strings.Contains(string(data), dev.ID) || strings.Contains(string(data), "SECRETTOKEN") {
		t.Fatalf("devices.json: %s", data)
	}
	// Attach adopts the held driver; the app id defaults to the launch's.
	info, err := e.c.Attach(ctx, dev.ID, AttachConfig{})
	if err != nil || info.AppID != "org.example" || info.Kind != "cloud" {
		t.Fatalf("attach: %+v %v", info, err)
	}
	if res, err := e.c.Command(ctx, dev.ID, "tapOn", "Login", true); err != nil || !res.OK || len(res.Warnings) != 0 {
		t.Fatalf("step: %+v %v", res, err)
	}
	if err := e.c.Detach(ctx, dev.ID); err != nil {
		t.Fatal(err)
	}
	if n := e.driver(0).closes.Load(); n != 0 {
		t.Fatalf("detach closed the held driver (%d)", n)
	}
	if _, err := e.c.Attach(ctx, dev.ID, AttachConfig{}); err != nil {
		t.Fatalf("re-attach: %v", err)
	}
	// Stop releases: the answer carries the minutes, and everything goes.
	res, err := e.c.StopDeviceResult(ctx, dev.ID, false)
	if err != nil || res.Device.DeviceMinutes == nil || res.Device.DeviceMinutes.Total != 2 || res.Device.ID != dev.ID {
		t.Fatalf("stop: %+v %v", res, err)
	}
	if e.driver(0).closes.Load() != 1 || len(e.f.releasedRefs()) != 1 {
		t.Fatalf("closes %d released %v", e.driver(0).closes.Load(), e.f.releasedRefs())
	}
	if entries, _ := ReadLedger(); len(entries) != 0 {
		t.Fatalf("ledger after stop: %+v", entries)
	}
	if _, err := e.c.Device(ctx, dev.ID); codeOf(t, err) != CodeDeviceNotFound {
		t.Fatalf("still known: %v", err)
	}
	// A non-cloud stop still answers an empty device.
	if res, err := e.c.StopDeviceResult(ctx, "mock-1", true); err != nil || res.Device.ID != "" {
		t.Fatalf("mock stop: %+v %v", res, err)
	}

	// Delete releases too, and its device carries the minutes.
	dev = e.launch(cloudprov.CloudSpec{})
	del, err := e.c.DeleteDevice(ctx, dev.ID, false)
	if err != nil || del.DeviceMinutes == nil {
		t.Fatalf("delete: %+v %v", del, err)
	}
	if st, _ := e.c.Status(ctx); len(st.Cloud) != 1 || st.Cloud[0].Held != 0 {
		t.Fatalf("status after delete: %+v", st.Cloud)
	}
}

func TestRemote_CreateRefusals(t *testing.T) {
	e := newRemoteEnv(t, nil)
	ctx := context.Background()
	create := func(req BootRequest) error {
		_, err := e.c.CreateDevice(ctx, req)
		return err
	}
	cloud := func(s cloudprov.CloudSpec) *CreateSpec { return &CreateSpec{Cloud: &s} }
	for _, c := range []struct {
		name string
		req  BootRequest
	}{
		{"no platform", BootRequest{Create: cloud(cloudprov.CloudSpec{})}},
		{"bad platform", BootRequest{Create: cloud(cloudprov.CloudSpec{Platform: "web"})}},
		{"platforms differ", BootRequest{Platform: "ios", Create: cloud(cloudprov.CloudSpec{Platform: "android"})}},
		{"idle too short", BootRequest{Create: cloud(cloudprov.CloudSpec{Platform: "android", IdleReleaseSec: -5})}},
		{"idle too long", BootRequest{Create: cloud(cloudprov.CloudSpec{Platform: "android", IdleReleaseSec: 9001})}},
		{"aux apps", BootRequest{Create: cloud(cloudprov.CloudSpec{Platform: "android", AuxiliaryApps: []string{"a", "b", "c", "d"}})}},
		{"appium", BootRequest{Create: cloud(cloudprov.CloudSpec{Platform: "android", AppiumVersion: "4"})}},
		{"boot timeout", BootRequest{Create: cloud(cloudprov.CloudSpec{Platform: "android", BootTimeoutSec: -1})}},
		{"simulator field", BootRequest{Create: &CreateSpec{Type: "iPhone 16", Cloud: &cloudprov.CloudSpec{Platform: "ios"}}}},
		{"boot false", BootRequest{Boot: new(bool), Create: cloud(cloudprov.CloudSpec{Platform: "android"})}},
		{"bad blob", BootRequest{Create: cloud(cloudprov.CloudSpec{Platform: "android", App: "blob:sha256:00/x.apk"})}},
	} {
		if err := create(c.req); codeOf(t, err) != CodeUsage {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	// The top-level platform fills in an empty cloud.platform.
	if _, err := e.c.CreateDevice(ctx, BootRequest{Platform: "android", Create: cloud(cloudprov.CloudSpec{})}); err != nil {
		t.Fatalf("top-level platform: %v", err)
	}
	// A provider failure maps through its kind.
	e.f.allocErr = cloudprov.Errorf(cloudprov.NotFound, "no model matches; closest: Google Pixel 8 14")
	if err := create(BootRequest{Create: cloud(cloudprov.CloudSpec{Platform: "android", Name: "Nokia"})}); codeOf(t, err) != CodeDeviceNotFound {
		t.Fatalf("not found: %v", err)
	}
	e.f.allocErr = nil

	// A remote caller of a plain daemon is FORBIDDEN.
	r := httptest.NewRequest("POST", "/v1/devices", strings.NewReader(`{"create":{"cloud":{"platform":"android"}}}`))
	r = r.WithContext(withCaller(r.Context(), Caller{Remote: true, Addr: "10.0.0.9:1"}))
	w := httptest.NewRecorder()
	e.s.handleBootDevice(w, r)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "local caller") {
		t.Fatalf("remote caller: %d %s", w.Code, w.Body.String())
	}

	// A hub refuses every cloud create with the lease hint, before its
	// share list is looked at.
	hub := newRemoteEnv(t, func(c *Config) { c.Hub, c.HubName, c.ShareDevices = true, "h", []string{"mock-1"} })
	for _, caller := range []Caller{{}, {Remote: true, Addr: "10.0.0.9:1"}} {
		r := httptest.NewRequest("POST", "/v1/devices", strings.NewReader(`{"create":{"cloud":{"platform":"android"}}}`))
		r = r.WithContext(withCaller(r.Context(), caller))
		w := httptest.NewRecorder()
		hub.s.handleBootDevice(w, r)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "lease --location cloud") {
			t.Fatalf("hub, %+v: %d %s", caller, w.Code, w.Body.String())
		}
	}
}

func TestRemote_AttachConflicts(t *testing.T) {
	e := newRemoteEnv(t, nil)
	ctx := context.Background()
	dev := e.launch(cloudprov.CloudSpec{})
	for _, cfg := range []AttachConfig{
		{Driver: "uiautomator2"}, {AppiumURL: "http://x"}, {Platform: "ios"},
		{Extra: map[string]any{"capsFile": "c.json"}},
	} {
		if _, err := e.c.Attach(ctx, dev.ID, cfg); codeOf(t, err) != CodeUsage {
			t.Errorf("%+v: %v", cfg, err)
		}
	}
	if _, err := e.c.Attach(ctx, "df:android:google-pixel-8:14", AttachConfig{}); codeOf(t, err) != CodeUsage {
		t.Fatalf("catalog id: %v", err)
	}
	// The held driver has no screen capture.
	if _, err := e.c.Attach(ctx, dev.ID, AttachConfig{Driver: "appium", Platform: "android"}); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(e.c.base + "/v1/devices/" + dev.ID + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("X-Maestro-Error-Code") != string(CodeCaptureUnavailable) {
		t.Fatalf("stream: %d %s", resp.StatusCode, resp.Header.Get("X-Maestro-Error-Code"))
	}
}

func TestRemoteIdle_LongStepAndRelease(t *testing.T) {
	e := newRemoteEnv(t, nil)
	e.stepDelay = 3 * time.Second
	ctx := context.Background()
	events, _, unsub := e.s.events.Subscribe("", 0)
	defer unsub()
	dev := e.launch(cloudprov.CloudSpec{IdleReleaseSec: 1})
	if _, err := e.c.Attach(ctx, dev.ID, AttachConfig{}); err != nil {
		t.Fatal(err)
	}
	pings := e.driver(0).pings.Load()
	// A step that outlasts the idle timeout completes: the clock stops
	// while it runs, and no keepalive is sent beside it.
	res, err := e.c.Command(ctx, dev.ID, "tapOn", "Login", true)
	if err != nil || !res.OK {
		t.Fatalf("long step: %+v %v", res, err)
	}
	stepEnd := time.Now()
	if e.driver(0).pings.Load() != pings {
		t.Fatalf("pinged during the step")
	}
	var rel Event
	waitUntil(t, 5*time.Second, "the idle release", func() bool {
		for {
			select {
			case ev := <-events:
				if ev.Type == "device.released" && ev.Device == dev.ID {
					rel = ev
					return true
				}
			default:
				return false
			}
		}
	})
	if since := time.Since(stepEnd); since < 700*time.Millisecond || rel.Message != "idle" || rel.DeviceMinutes == nil {
		t.Fatalf("released %s after the step: %+v", since, rel)
	}
	if _, err := e.c.Command(ctx, dev.ID, "tapOn", "Login", true); err == nil {
		t.Fatal("a step on a released device passed")
	}
}

func TestRemoteIdle_OverlappingRequestsAndBlobs(t *testing.T) {
	e := newRemoteEnv(t, nil)
	dev := e.launch(cloudprov.CloudSpec{IdleReleaseSec: 1})
	rd := e.s.remote(dev.ID)
	done1, err := e.s.cloudBusy(dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	done2, _ := e.s.cloudBusy(dev.ID)
	time.Sleep(1500 * time.Millisecond)
	done1()
	time.Sleep(1200 * time.Millisecond)
	if rel, _ := rd.due(time.Now()); rel {
		t.Fatal("released while a request was in flight")
	}
	done2()
	waitUntil(t, 3*time.Second, "release after the last request", func() bool { return e.s.remote(dev.ID) == nil })
	// A non-cloud id: cloudBusy does nothing.
	done, err := e.s.cloudBusy("mock-1")
	if err != nil {
		t.Fatal(err)
	}
	done()
}

func TestRemoteIdle_RaceNeverHalfReleased(t *testing.T) {
	for i := 0; i < 200; i++ {
		rd := &remoteDevice{id: "df-x", idle: time.Millisecond, timing: cloudTimings(), lastActivity: time.Now().Add(-time.Second)}
		var released, began bool
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); released, _ = rd.due(time.Now()) }()
		go func() { defer wg.Done(); began = rd.begin() == nil }()
		wg.Wait()
		if released && began {
			t.Fatalf("iteration %d: a request began on a device released for idleness", i)
		}
		if !released && !began {
			t.Fatalf("iteration %d: neither released nor began", i)
		}
	}
}

func TestRemoteIdle_KeepaliveAndDaemonTimer(t *testing.T) {
	e := newRemoteEnv(t, nil)
	e.s.idle = newIdleTimer(time.Second, func() {})
	ctx := context.Background()
	dev := e.launch(cloudprov.CloudSpec{IdleReleaseSec: 60})
	d := e.driver(0)
	waitUntil(t, 4*time.Second, "a keepalive", func() bool { return d.pings.Load() > 0 })
	// The held device holds the daemon's idle timer.
	time.Sleep(1500 * time.Millisecond)
	if e.s.idle.stopped {
		t.Fatal("the daemon's idle timer fired with a cloud device held")
	}
	// A ping that fails while the provider says RUNNING keeps the device.
	boom := errors.New("socket hang up")
	d.pingErr.Store(&boom)
	n := d.pings.Load()
	waitUntil(t, 4*time.Second, "a failing ping", func() bool { return d.pings.Load() > n+1 })
	if e.s.remote(dev.ID) == nil {
		t.Fatal("a failed ping ended a running session")
	}
	// A ping waits while a step holds the device.
	if _, err := e.c.Attach(ctx, dev.ID, AttachConfig{}); err != nil {
		t.Fatal(err)
	}
	a := e.s.getAttached(dev.ID)
	a.busy.Lock()
	n = d.pings.Load()
	time.Sleep(1500 * time.Millisecond)
	if d.pings.Load() != n {
		t.Fatal("pinged while a step held the device")
	}
	a.busy.Unlock()
	// Once the provider ends the session, the next failed ping ends the
	// device.
	e.f.mu.Lock()
	e.f.state = "COMPLETED"
	e.f.mu.Unlock()
	waitUntil(t, 5*time.Second, "the device to end", func() bool {
		info, ok := e.s.registry.Record(dev.ID)
		return ok && info.State == "ended"
	})
	if _, err := e.c.Command(ctx, dev.ID, "tapOn", "x", true); codeOf(t, err) != CodeDeviceError {
		t.Fatalf("step on an ended device: %v", err)
	}
	// Deleting it drops it, and lets the daemon's timer run again.
	if _, err := e.c.DeleteDevice(ctx, dev.ID, false); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, 3*time.Second, "the daemon's idle timer", func() bool {
		e.s.idle.mu.Lock()
		defer e.s.idle.mu.Unlock()
		return e.s.idle.stopped
	})
}

func TestRemoteIdle_PingDoesNotMakeWaitFalseBusy(t *testing.T) {
	e := newRemoteEnv(t, nil)
	ctx := context.Background()
	dev := e.launch(cloudprov.CloudSpec{IdleReleaseSec: 60})
	if _, err := e.c.Attach(ctx, dev.ID, AttachConfig{}); err != nil {
		t.Fatal(err)
	}
	rd, a := e.s.remote(dev.ID), e.s.getAttached(dev.ID)
	// A ping holds the lock: a wait=false step waits it out.
	a.busy.Lock()
	rd.mu.Lock()
	rd.pinging = true
	rd.mu.Unlock()
	go func() {
		time.Sleep(200 * time.Millisecond)
		rd.mu.Lock()
		rd.pinging = false
		rd.mu.Unlock()
		a.busy.Unlock()
	}()
	if _, err := e.c.Command(ctx, dev.ID, "tapOn", "x", false); err != nil {
		t.Fatalf("wait=false behind a ping: %v", err)
	}
	// A step holds it: BUSY as before.
	a.busy.Lock()
	defer a.busy.Unlock()
	if _, err := e.c.Command(ctx, dev.ID, "tapOn", "x", false); codeOf(t, err) != CodeBusy {
		t.Fatalf("wait=false behind a step: %v", err)
	}
	if lockWithin(&a.busy, 50*time.Millisecond) {
		t.Fatal("lockWithin took a held lock")
	}
}

func TestRemoteSessionCap(t *testing.T) {
	e := newRemoteEnv(t, nil)
	e.f.expires = 32 * time.Second // the daemon releases 30 s before
	ctx := context.Background()
	events, _, unsub := e.s.events.Subscribe("", 0)
	defer unsub()
	dev := e.launch(cloudprov.CloudSpec{IdleReleaseSec: 600})
	if _, err := e.c.Attach(ctx, dev.ID, AttachConfig{}); err != nil {
		t.Fatal(err)
	}
	res, err := e.c.Command(ctx, dev.ID, "tapOn", "x", true)
	if err != nil || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "cloud session ends at") {
		t.Fatalf("warnings: %+v %v", res, err)
	}
	t.Logf("step result warnings: %v", res.Warnings)
	waitUntil(t, 5*time.Second, "the session-limit release", func() bool {
		for {
			select {
			case ev := <-events:
				if ev.Type == "device.released" && ev.Device == dev.ID {
					t.Logf("event %s %s reason=%s deviceMinutes=%+v", ev.Time, ev.Type, ev.Message, *ev.DeviceMinutes)
					return ev.Message == "session-limit"
				}
			default:
				return false
			}
		}
	})
}

func TestRemote_LedgerWriteFailsTheLaunch(t *testing.T) {
	e := newRemoteEnv(t, nil)
	old := ledgerLockWait
	ledgerLockWait = 100 * time.Millisecond
	defer func() { ledgerLockWait = old }()
	_ = os.MkdirAll(RunRoot(), 0o700)
	f, err := lockLedger(ledgerLockPath(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.c.CreateDevice(context.Background(), BootRequest{Create: &CreateSpec{Cloud: &cloudprov.CloudSpec{Platform: "android"}}})
	unlockRunDir(f)
	if codeOf(t, err) != CodeDeviceError || !strings.Contains(err.Error(), "cannot record") {
		t.Fatalf("launch with the ledger locked: %v", err)
	}
	if len(e.f.releasedRefs()) != 1 {
		t.Fatalf("the session was not stopped: %v", e.f.releasedRefs())
	}
}

func TestRemote_LaunchFailures(t *testing.T) {
	e := newRemoteEnv(t, nil)
	ctx := context.Background()
	// The WebDriver session does not open: DEVICE_ERROR, redacted, and the
	// session released.
	e.s.deps.NewRemoteDriver = func(context.Context, string, map[string]any) (RemoteDriver, DriverInfo, error) {
		return nil, DriverInfo{}, errors.New("POST https://x/remote-endpoint/SECRET/wd/hub/session: refused")
	}
	e.f.fakeProvider = newFakeProvider()
	_, err := e.c.CreateDevice(ctx, BootRequest{Create: &CreateSpec{Cloud: &cloudprov.CloudSpec{Platform: "android"}}})
	if codeOf(t, err) != CodeDeviceError || len(e.f.releasedRefs()) != 1 {
		t.Fatalf("WebDriver failure: %v %v", err, e.f.releasedRefs())
	}
	if entries, _ := ReadLedger(); len(entries) != 0 {
		t.Fatalf("ledger: %+v", entries)
	}
	// The release fails too: the entry stays for --orphans.
	e.f.releaseFn = func(string) error { return errors.New("throttled") }
	_, _ = e.c.CreateDevice(ctx, BootRequest{Create: &CreateSpec{Cloud: &cloudprov.CloudSpec{Platform: "android"}}})
	if entries, _ := ReadLedger(); len(entries) != 1 {
		t.Fatalf("ledger after a failed release: %+v", entries)
	}
	// No remote driver configured.
	e.s.deps.NewRemoteDriver = nil
	if _, err := e.s.launchCloud(ctx, cloudprov.CloudSpec{Provider: "devicefarm", Platform: "android"}); err == nil || err.Code != CodeInternal {
		t.Fatalf("no driver factory: %v", err)
	}
	// An unknown provider.
	if _, err := e.s.launchCloud(ctx, cloudprov.CloudSpec{Provider: "nope", Platform: "android"}); err == nil || err.Code != CodeUsage {
		t.Fatalf("unknown provider: %v", err)
	}
}

func TestRemote_ReleaseFailureAndShutdown(t *testing.T) {
	e := newRemoteEnv(t, nil)
	ctx := context.Background()
	a := e.launch(cloudprov.CloudSpec{})
	b := e.launch(cloudprov.CloudSpec{})
	// A stop whose release fails answers DEVICE_ERROR, and the entry stays.
	e.f.releaseFn = func(ref string) error {
		if strings.Contains(ref, a.ID) {
			return cloudprov.Errorf(cloudprov.Other, "throttled")
		}
		return nil
	}
	if _, err := e.c.StopDeviceResult(ctx, a.ID, false); codeOf(t, err) != CodeDeviceError {
		t.Fatalf("failed release: %v", err)
	}
	if entries, _ := ReadLedger(); len(entries) != 2 {
		t.Fatalf("ledger: %+v", entries)
	}
	// Daemon exit releases the rest.
	e.s.releaseAllRemote("daemon-exit")
	if e.s.remote(b.ID) != nil {
		t.Fatal("still held after releaseAllRemote")
	}
	if entries, _ := ReadLedger(); len(entries) != 1 || entries[0].ID != a.ID {
		t.Fatalf("ledger after exit: %+v", entries)
	}
}

func TestRemote_SelfRelease(t *testing.T) {
	e := newRemoteEnv(t, nil)
	self := e.s.cfg.Name
	for _, en := range []CloudEntry{
		{Daemon: self, PID: 999999, ID: "df-old", Provider: "devicefarm", ProviderRef: "ref-old", StartedAt: "1"},
		{Daemon: self, PID: os.Getpid(), ID: "df-mine", Provider: "devicefarm", ProviderRef: "ref-mine", StartedAt: "2"},
		{Daemon: "other", PID: 999999, ID: "df-other", Provider: "devicefarm", ProviderRef: "ref-other", StartedAt: "3"},
		{Daemon: self, PID: 999998, ID: "df-bs", Provider: "browserstack", ProviderRef: "ref-bs", StartedAt: "4"},
		{Daemon: self, PID: 999997, ID: "df-fail", Provider: "devicefarm", ProviderRef: "ref-fail", StartedAt: "5"},
	} {
		if err := addLedgerEntry(en); err != nil {
			t.Fatal(err)
		}
	}
	e.f.releaseFn = func(ref string) error {
		if ref == "ref-fail" {
			return errors.New("throttled")
		}
		return nil
	}
	events, _, unsub := e.s.events.Subscribe("", 0)
	e.s.selfRelease()
	unsub()
	if got := e.f.releasedRefs(); len(got) != 1 || got[0] != "ref-old" {
		t.Fatalf("released %v", got)
	}
	// The release reports its minutes on the event.
	var released *Event
	for len(events) > 0 {
		if ev := <-events; ev.Type == "device.released" {
			released = &ev
		}
	}
	if released == nil || released.Message != "orphaned" || released.DeviceMinutes == nil || released.DeviceMinutes.Total != 4 {
		t.Fatalf("device.released: %+v", released)
	}
	entries, _ := ReadLedger()
	var left []string
	for _, en := range entries {
		left = append(left, en.ID)
	}
	if strings.Join(left, ",") != "df-mine,df-other,df-bs,df-fail" {
		t.Fatalf("left %v", left)
	}
	// A daemon whose provider cannot be built leaves its entries.
	e.s.deps.Providers["devicefarm"] = func() (cloudprov.RemoteProvider, error) { return nil, errors.New("no credentials") }
	e.s.cloud.made = nil
	e.s.selfRelease()
}

func TestRemote_DecorateAndWarningsWithoutDevice(t *testing.T) {
	s := &Server{}
	if s.cloudWarnings("x") != nil {
		t.Fatal("warnings for an unknown device")
	}
	devs := []DeviceInfo{{ID: "df-1@h", LocalID: "df-1", Provider: "devicefarm"}, {ID: "u"}}
	s.decorateCloud(devs)
	if devs[0].IdleReleaseAt != "" || devs[0].LocalIDOrID() != "df-1" || devs[1].LocalIDOrID() != "u" {
		t.Fatalf("decorate: %+v", devs)
	}
	if s.isPinging("x") {
		t.Fatal("pinging an unknown device")
	}
	if minutesNote(nil) != "" || !strings.Contains(minutesNote(&cloudprov.DeviceMinutes{Total: 3}), "3") {
		t.Fatal("minutesNote")
	}
}

func TestRemote_RecordingIsCaptureUnavailable(t *testing.T) {
	e := newRemoteEnv(t, nil)
	ctx := context.Background()
	dev := e.launch(cloudprov.CloudSpec{})
	if _, err := e.c.Attach(ctx, dev.ID, AttachConfig{}); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{"startRecording", "stopRecording"} {
		res, err := e.c.Command(ctx, dev.ID, cmd, "rec", true)
		if err == nil && res.OK {
			t.Fatalf("%s passed on a cloud device", cmd)
		}
		var body *ErrorBody
		if res != nil {
			body = res.Error
		}
		if body == nil || body.Code != CodeCaptureUnavailable || body.Details["reason"] != "unsupported-platform" ||
			strings.Contains(body.Message, "*flow.") || !strings.Contains(body.Message, "has no screen capture") ||
			strings.Contains(res.Message, "*flow.") {
			t.Fatalf("%s: %+v %v", cmd, body, err)
		}
	}
}

func TestRemote_ListsTheAppiumVersion(t *testing.T) {
	e := newRemoteEnv(t, nil)
	dev := e.launch(cloudprov.CloudSpec{})
	if dev.AppiumVersion != "2" || dev.Model != "Google Pixel 8" {
		t.Fatalf("launched: %+v", dev)
	}
	info, err := e.c.Device(context.Background(), dev.ID)
	if err != nil || info.AppiumVersion != "2" {
		t.Fatalf("listed: %+v %v", info, err)
	}
}

func TestRemote_SessionAnswers(t *testing.T) {
	d := &fakeRemoteDriver{Driver: mock.New(mock.Config{})}
	s := &Server{}
	rd := &remoteDevice{driver: d}
	if !s.sessionAnswers(rd) || d.pings.Load() != 1 {
		t.Fatal("a live session")
	}
	gone := errors.New("invalid session id")
	d.pingErr.Store(&gone)
	if s.sessionAnswers(rd) {
		t.Fatal("a dead session answered")
	}
}

// A release that fails is retried in the background until the provider
// confirms; its ledger entry stays until then.
func TestRemote_FailedReleaseIsRetried(t *testing.T) {
	e := newRemoteEnv(t, nil)
	ctx := context.Background()
	dev := e.launch(cloudprov.CloudSpec{})
	var fails atomic.Int32
	fails.Store(2)
	e.f.releaseFn = func(string) error {
		if fails.Add(-1) >= 0 {
			return errors.New("ServiceUnavailableException")
		}
		return nil
	}
	events, _, unsub := e.s.events.Subscribe(dev.ID, 0)
	defer unsub()
	_, err := e.c.StopDeviceResult(ctx, dev.ID, false)
	if codeOf(t, err) != CodeDeviceError || !strings.Contains(err.Error(), "keeps retrying") {
		t.Fatalf("a failed release: %v", err)
	}
	if entries, _ := ReadLedger(); len(entries) != 1 {
		t.Fatalf("the ledger dropped a session still running: %+v", entries)
	}
	waitUntil(t, 5*time.Second, "the retried release", func() bool {
		entries, _ := ReadLedger()
		return len(entries) == 0 && len(e.f.releasedRefs()) == 1
	})
	var retried bool
	for len(events) > 0 {
		if ev := <-events; ev.Type == "device.released" && ev.Message == "retried" && ev.DeviceMinutes != nil {
			retried = true
		}
	}
	if !retried {
		t.Fatal("no device.released for the retry")
	}
}

// A session the provider no longer knows (NotFound) has ended: the device
// turns ended instead of failing every step with driver errors.
func TestRemote_UnknownSessionEndsTheDevice(t *testing.T) {
	e := newRemoteEnv(t, nil)
	dev := e.launch(cloudprov.CloudSpec{})
	rd := e.s.remote(dev.ID)
	e.f.mu.Lock()
	e.f.refreshErr = cloudprov.Errorf(cloudprov.Other, "slow down")
	e.f.mu.Unlock()
	if e.s.checkRemote(rd) {
		t.Fatal("a failed refresh ended the device")
	}
	e.f.mu.Lock()
	e.f.refreshErr = cloudprov.Errorf(cloudprov.NotFound, "no such session")
	e.f.mu.Unlock()
	if !e.s.checkRemote(rd) {
		t.Fatal("an unknown session did not end the device")
	}
	waitUntil(t, 5*time.Second, "the ended record", func() bool {
		info, ok := e.s.registry.Lookup(dev.ID)
		return ok && info.State == "ended" && strings.Contains(info.EndedReason, "no such session")
	})
}

// On a plain daemon serving remote callers, a direct-mode cloud device
// follows --share-device: unlisted, a remote caller cannot see or drive it.
func TestRemote_PlainDaemonDoesNotShareCloudDevices(t *testing.T) {
	e := newRemoteEnv(t, func(c *Config) { c.ShareDevices = []string{"mock-1"} })
	dev := e.launch(cloudprov.CloudSpec{})
	if !e.s.sharedInfo(DeviceInfo{ID: "mock-1"}) {
		t.Fatal("mock-1 is listed")
	}
	if e.s.sharedInfo(DeviceInfo{ID: dev.ID, Provider: "devicefarm"}) {
		t.Fatal("a plain daemon shared its cloud device past --share-device")
	}
	hub := &Server{cfg: Config{Hub: true, ShareDevices: []string{"mock-1"}}}
	if !hub.sharedInfo(DeviceInfo{ID: dev.ID, Provider: "devicefarm"}) {
		t.Fatal("a hub's cloud device is not in its pool")
	}
}

// A launch that fails after its session exists, and whose release the
// provider refuses, releases it in the background: after the WebDriver
// session failed to open, and after the provider gave up a session it could
// not stop.
func TestRemote_FailedLaunchReleaseIsRetried(t *testing.T) {
	e := newRemoteEnv(t, nil)
	ctx := context.Background()
	e.s.deps.NewRemoteDriver = func(context.Context, string, map[string]any) (RemoteDriver, DriverInfo, error) {
		return nil, DriverInfo{}, errors.New("session not created")
	}
	var fails atomic.Int32
	fails.Store(1)
	e.f.releaseFn = func(string) error {
		if fails.Add(-1) >= 0 {
			return errors.New("ServiceUnavailableException")
		}
		return nil
	}
	if _, err := e.c.CreateDevice(ctx, BootRequest{Create: &CreateSpec{Cloud: &cloudprov.CloudSpec{Platform: "android"}}}); codeOf(t, err) != CodeDeviceError {
		t.Fatalf("a launch whose driver fails: %v", err)
	}
	waitUntil(t, 5*time.Second, "the retried release", func() bool {
		entries, _ := ReadLedger()
		return len(entries) == 0 && len(e.f.releasedRefs()) == 1
	})
	// The provider gives up a session it could not stop: the daemon
	// releases the ledger's entry for it.
	e.f.releaseFn = nil
	e.f.failAfterCreated = cloudprov.Errorf(cloudprov.DeviceBusy, "busy; stopping the session failed")
	if _, err := e.c.CreateDevice(ctx, BootRequest{Create: &CreateSpec{Cloud: &cloudprov.CloudSpec{Platform: "android"}}}); err == nil {
		t.Fatal("the launch passed")
	}
	waitUntil(t, 5*time.Second, "the abandoned session's release", func() bool {
		entries, _ := ReadLedger()
		return len(entries) == 0 && len(e.f.releasedRefs()) == 2
	})
}

// A daemon that shuts down while a launch waits cancels it: no device is
// registered after the teardown, and the session is released.
func TestRemote_ShutdownCancelsALaunch(t *testing.T) {
	e := newRemoteEnv(t, nil)
	e.f.hold = make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := e.c.CreateDevice(context.Background(), BootRequest{Create: &CreateSpec{Cloud: &cloudprov.CloudSpec{Platform: "android"}}})
		done <- err
	}()
	waitUntil(t, 5*time.Second, "the session to be recorded", func() bool {
		entries, _ := ReadLedger()
		return len(entries) == 1
	})
	e.s.Shutdown("test")
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the launch passed during shutdown")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the launch outlived the shutdown")
	}
	e.s.remoteMu.Lock()
	n := len(e.s.remotes)
	e.s.remoteMu.Unlock()
	if n != 0 {
		t.Fatalf("%d device(s) registered after the shutdown", n)
	}
	if entries, _ := ReadLedger(); len(entries) != 0 {
		t.Fatalf("the ledger still holds %+v", entries)
	}
}

// Polling a cloud device's info, which any caller may, does not keep it
// from its idle release.
func TestRemote_InfoDoesNotHoldTheIdleClock(t *testing.T) {
	e := newRemoteEnv(t, nil)
	dev := e.launch(cloudprov.CloudSpec{IdleReleaseSec: 1})
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(100 * time.Millisecond):
				r := httptest.NewRequest("GET", "/v1/devices/"+dev.ID+"/info", nil)
				e.s.Handler().ServeHTTP(httptest.NewRecorder(), r)
			}
		}
	}()
	waitUntil(t, 5*time.Second, "the idle release while info is polled", func() bool { return e.s.remote(dev.ID) == nil })
}
