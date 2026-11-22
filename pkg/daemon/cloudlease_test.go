package daemon

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/cloudprov"
	"github.com/devicelab-dev/maestro-runner/pkg/driver/mock"
)

// cloudHub is a hub test env whose devicefarm provider is a cloudFake.
type cloudHub struct {
	*hubEnv
	f       *cloudFake
	mu      sync.Mutex
	drivers []*fakeRemoteDriver
	delay   time.Duration
}

func newCloudHub(t *testing.T, cfg Config) *cloudHub {
	t.Helper()
	t.Setenv(envFastCloud, "1")
	if cfg.CloudMaxDevices == 0 {
		cfg.CloudMaxDevices = 2
	}
	h := &cloudHub{hubEnv: newHubEnv(t, cfg), f: newCloudFake()}
	h.s.deps.Providers = map[string]func() (cloudprov.RemoteProvider, error){
		"devicefarm": func() (cloudprov.RemoteProvider, error) { return h.f, nil },
	}
	h.s.deps.NewRemoteDriver = func(ctx context.Context, endpoint string, caps map[string]any) (RemoteDriver, DriverInfo, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		d := &fakeRemoteDriver{Driver: mock.New(mock.Config{Platform: "android", StepDelay: h.delay})}
		h.drivers = append(h.drivers, d)
		return d, DriverInfo{DriverName: "appium", Platform: "android", Kind: "cloud"}, nil
	}
	t.Cleanup(func() { h.s.releaseAllRemote("test-end") })
	return h
}

func cloudSel(extra ...func(*LeaseSelector)) *LeaseSelector {
	sel := &LeaseSelector{Location: "cloud", Platform: "android", Name: "Google Pixel 8"}
	for _, f := range extra {
		f(sel)
	}
	return sel
}

func TestCloudLease_LaunchBindRelease(t *testing.T) {
	h := newCloudHub(t, Config{ShareDevices: []string{"mock-1"}})
	ctx := context.Background()
	events, _, unsub := h.s.events.Subscribe("", 0)
	defer unsub()
	res, err := h.remote.AcquireLease(ctx, LeaseRequest{Select: cloudSel(), Cloud: &cloudprov.CloudSpec{IdleReleaseSec: 600}, Attach: &AttachConfig{}})
	if err != nil {
		t.Fatal(err)
	}
	id := res.Lease.Device
	if !strings.HasPrefix(id, "df-") || res.Device == nil || !res.Device.Attached || res.Lease.MaxSec != 9000 || res.Lease.TTLSec != 9000 {
		t.Fatalf("lease: %+v device %+v", res.Lease, res.Device)
	}
	// A remote caller sees it, though --share-device names another device,
	// and drives it under the lease.
	h.remote.SetLease(id, res.Lease.ID)
	if step, err := h.remote.Command(ctx, id, "tapOn", "Login", true); err != nil || !step.OK {
		t.Fatalf("step: %+v %v", step, err)
	}
	devs, _ := h.remote.Devices(ctx, "")
	var cd *DeviceInfo
	for i := range devs {
		if devs[i].ID == id {
			cd = &devs[i]
		}
	}
	if cd == nil || cd.Available || cd.Lease == nil || cd.MaxSec != 9000 || cd.Location != LocationCloud {
		t.Fatalf("listed: %+v", cd)
	}
	// It belongs to its lease: a second lease on it is DEVICE_IN_USE, and
	// a plain selector never takes it.
	if _, err := h.local.AcquireLease(ctx, LeaseRequest{Device: id}); codeOf(t, err) != CodeDeviceInUse {
		t.Fatalf("second lease: %v", err)
	}
	if _, err := h.local.AcquireLease(ctx, LeaseRequest{Select: &LeaseSelector{Platform: "android"}}); codeOf(t, err) != CodeDeviceNotFound {
		t.Fatalf("plain selector: %v", err)
	}
	// Renewing counts as activity, within the cap.
	if _, err := h.remote.RenewLease(ctx, res.Lease.ID, 600); err != nil {
		t.Fatalf("renew: %v", err)
	}
	if _, err := h.remote.RenewLease(ctx, res.Lease.ID, 9001); codeOf(t, err) != CodeUsage {
		t.Fatalf("renew over the cap: %v", err)
	}
	// Releasing the lease releases the device, with its minutes.
	rel, err := h.remote.ReleaseLease(ctx, res.Lease.ID)
	if err != nil || rel.DeviceMinutes == nil || rel.DeviceMinutes.Total != 2 {
		t.Fatalf("release: %+v %v", rel, err)
	}
	if h.s.remote(id) != nil || len(h.f.releasedRefs()) != 1 {
		t.Fatalf("still held: %v", h.f.releasedRefs())
	}
	var sawReleased, sawLease bool
	for len(events) > 0 {
		ev := <-events
		sawReleased = sawReleased || (ev.Type == "device.released" && ev.Message == "lease-end")
		sawLease = sawLease || ev.Type == "lease.released"
	}
	if !sawReleased || !sawLease {
		t.Fatalf("events: released %v lease %v", sawReleased, sawLease)
	}
}

func TestCloudLease_CapTTLAndRefusals(t *testing.T) {
	h := newCloudHub(t, Config{CloudMaxDevices: 1})
	ctx := context.Background()
	if _, err := h.remote.AcquireLease(ctx, LeaseRequest{Select: cloudSel(), TTLSec: 9001}); codeOf(t, err) != CodeUsage ||
		!strings.Contains(err.Error(), "a window of ttlSec 9001 is over a cloud device's maximum of 2h30m0s (maxSec 9000)") {
		t.Fatalf("ttl over the cap: %v", err)
	}
	res, err := h.remote.AcquireLease(ctx, LeaseRequest{Select: cloudSel(), TTLSec: 600})
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.remote.AcquireLease(ctx, LeaseRequest{Select: cloudSel()})
	if codeOf(t, err) != CodeBusy || !strings.Contains(err.Error(), "--cloud-max-devices 1") {
		t.Fatalf("over the cap: %v", err)
	}
	if st, _ := h.local.Status(ctx); len(st.Cloud) != 1 || st.Cloud[0].Held != 1 || st.Cloud[0].MaxDevices != 1 {
		t.Fatalf("status: %+v", st.Cloud)
	}
	_, _ = h.remote.ReleaseLease(ctx, res.Lease.ID)
	for _, c := range []struct {
		name string
		req  LeaseRequest
	}{
		{"app", LeaseRequest{Select: cloudSel(), Cloud: &cloudprov.CloudSpec{App: "/x.apk"}}},
		{"no platform", LeaseRequest{Select: cloudSel(func(s *LeaseSelector) { s.Platform = "" })}},
		{"idle", LeaseRequest{Select: cloudSel(), Cloud: &cloudprov.CloudSpec{IdleReleaseSec: 99999}}},
		{"provider", LeaseRequest{Select: cloudSel(func(s *LeaseSelector) { s.Provider = "browserstack" })}},
		{"location", LeaseRequest{Select: cloudSel(func(s *LeaseSelector) { s.Location = "mesh" })}},
	} {
		if _, err := h.remote.AcquireLease(ctx, c.req); codeOf(t, err) != CodeUsage {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	// A launch that fails frees the cap.
	h.f.allocErr = cloudprov.Errorf(cloudprov.NoCapacity, "account full")
	if _, err := h.remote.AcquireLease(ctx, LeaseRequest{Select: cloudSel()}); codeOf(t, err) != CodeBusy {
		t.Fatalf("launch failure: %v", err)
	}
	h.f.allocErr = nil
	if res, err := h.remote.AcquireLease(ctx, LeaseRequest{Select: cloudSel()}); err != nil {
		t.Fatalf("cap after a failed launch: %v", err)
	} else {
		_, _ = h.remote.ReleaseLease(ctx, res.Lease.ID)
	}
	// A hub without the provider says so.
	h.s.deps.Providers = nil
	if _, err := h.remote.AcquireLease(ctx, LeaseRequest{Select: cloudSel()}); codeOf(t, err) != CodeUsage {
		t.Fatalf("no provider: %v", err)
	}
	// A cap of 0 holds none.
	zero := newCloudHub(t, Config{CloudMaxDevices: -1})
	zero.s.cfg.CloudMaxDevices = 0
	if _, err := zero.remote.AcquireLease(ctx, LeaseRequest{Select: cloudSel()}); codeOf(t, err) != CodeBusy {
		t.Fatalf("cap 0: %v", err)
	}
}

func TestCloudLease_ExpiryReleasesTheDevice(t *testing.T) {
	h := newCloudHub(t, Config{})
	ctx := context.Background()
	res, err := h.remote.AcquireLease(ctx, LeaseRequest{Select: cloudSel(), TTLSec: 120})
	if err != nil {
		t.Fatal(err)
	}
	h.clock.advance(121 * time.Second)
	h.s.sweepLeases()
	waitUntil(t, 5*time.Second, "the device's release", func() bool { return h.s.remote(res.Lease.Device) == nil })
	if len(h.f.releasedRefs()) != 1 {
		t.Fatalf("released %v", h.f.releasedRefs())
	}
}

func TestCloudLease_IdleReleaseEndsTheLease(t *testing.T) {
	h := newCloudHub(t, Config{})
	ctx := context.Background()
	events, _, unsub := h.s.events.Subscribe("", 0)
	defer unsub()
	res, err := h.remote.AcquireLease(ctx, LeaseRequest{Select: cloudSel(), Cloud: &cloudprov.CloudSpec{IdleReleaseSec: 1}})
	if err != nil {
		t.Fatal(err)
	}
	h.remote.SetLease(res.Lease.Device, res.Lease.ID)
	var got []string
	waitUntil(t, 5*time.Second, "lease.expired", func() bool {
		for len(events) > 0 {
			ev := <-events
			if ev.Device == res.Lease.Device {
				got = append(got, ev.Type+":"+ev.Message)
			}
		}
		return strings.Contains(strings.Join(got, " "), "lease.expired")
	})
	if !strings.Contains(strings.Join(got, " "), "device.released:idle") {
		t.Fatalf("events: %v", got)
	}
	if _, err := h.remote.Lease(ctx, res.Lease.ID); codeOf(t, err) != CodeLeaseExpired {
		t.Fatalf("the lease after the device went: %v", err)
	}
	// Once the device has gone, a step under the ended lease still answers
	// LEASE_EXPIRED (clients forget the lease on it).
	waitUntil(t, 5*time.Second, "the device to go", func() bool { _, ok := h.s.registry.Lookup(res.Lease.Device); return !ok })
	_, err = h.remote.Command(ctx, res.Lease.Device, "tapOn", "x", true)
	var de *Error
	if !errors.As(err, &de) || de.Code != CodeLeaseExpired || de.Details["hint"] != "maestro-d lease --location cloud --platform ios|android" ||
		!strings.Contains(de.Message, "the device is gone with it") {
		t.Fatalf("a step under the ended lease: %v", err)
	}
	// Without a lease the gone device is not found (not offered for one).
	h.remote.SetLease(res.Lease.Device, "")
	if _, err := h.remote.Command(ctx, res.Lease.Device, "tapOn", "x", true); codeOf(t, err) != CodeDeviceNotFound {
		t.Fatalf("a step without a lease: %v", err)
	}
}

func TestCloudLease_ReleaseWaitsForWorkThenAnswers(t *testing.T) {
	h := newCloudHub(t, Config{})
	h.delay = 2 * time.Second
	ctx := context.Background()
	res, err := h.remote.AcquireLease(ctx, LeaseRequest{Select: cloudSel(), Attach: &AttachConfig{}})
	if err != nil {
		t.Fatal(err)
	}
	h.remote.SetLease(res.Lease.Device, res.Lease.ID)
	// A step that ends within the wait: the release answers once it has,
	// with the minutes.
	go func() { _, _ = h.remote.Command(ctx, res.Lease.Device, "tapOn", "x", true) }()
	time.Sleep(300 * time.Millisecond)
	rel, err := h.remote.ReleaseLease(ctx, res.Lease.ID)
	if err != nil || rel.DeviceMinutes == nil {
		t.Fatalf("release behind a step: %+v %v", rel, err)
	}
	// One still running when the wait runs out: no minutes in the answer.
	old := cloudLeaseReleaseWait
	cloudLeaseReleaseWait = 200 * time.Millisecond
	defer func() { cloudLeaseReleaseWait = old }()
	res, err = h.remote.AcquireLease(ctx, LeaseRequest{Select: cloudSel(), Attach: &AttachConfig{}})
	if err != nil {
		t.Fatal(err)
	}
	h.remote.SetLease(res.Lease.Device, res.Lease.ID)
	go func() { _, _ = h.remote.Command(ctx, res.Lease.Device, "tapOn", "x", true) }()
	time.Sleep(300 * time.Millisecond)
	rel, err = h.remote.ReleaseLease(ctx, res.Lease.ID)
	if err != nil || rel.DeviceMinutes != nil {
		t.Fatalf("release behind a long step: %+v %v", rel, err)
	}
	waitUntil(t, 5*time.Second, "the deferred release", func() bool { return h.s.remote(res.Lease.Device) == nil })
	// A non-cloud lease answers as before.
	l, err := h.remote.AcquireLease(ctx, LeaseRequest{Device: "mock-1"})
	if err != nil {
		t.Fatal(err)
	}
	if rel, err := h.remote.ReleaseLease(ctx, l.Lease.ID); err != nil || rel.DeviceMinutes != nil {
		t.Fatalf("mock release: %+v %v", rel, err)
	}
}

func TestCloudLease_StopUnderTheLease(t *testing.T) {
	h := newCloudHub(t, Config{})
	ctx := context.Background()
	res, err := h.remote.AcquireLease(ctx, LeaseRequest{Select: cloudSel()})
	if err != nil {
		t.Fatal(err)
	}
	h.remote.SetLease(res.Lease.Device, res.Lease.ID)
	events, _, unsub := h.s.events.Subscribe("", 0)
	defer unsub()
	st, err := h.remote.StopDeviceResult(ctx, res.Lease.Device, false)
	if err != nil || st.Device.DeviceMinutes == nil {
		t.Fatalf("stop: %+v %v", st, err)
	}
	if _, err := h.remote.Lease(ctx, res.Lease.ID); codeOf(t, err) != CodeLeaseExpired {
		t.Fatalf("the lease after a stop: %v", err)
	}
	if n := len(h.f.releasedRefs()); n != 1 {
		t.Fatalf("released %d times", n)
	}
	// The holder's own stop ends the lease as released, not as an expiry.
	var leaseEvents []string
	for len(events) > 0 {
		if ev := <-events; strings.HasPrefix(ev.Type, "lease.") {
			leaseEvents = append(leaseEvents, ev.Type)
		}
	}
	if strings.Join(leaseEvents, " ") != "lease.released" {
		t.Fatalf("lease events: %v", leaseEvents)
	}
}

// leaseDevice's bind runs on the granted entry while leaseMu is still held,
// so a lease that ends at once (the hub stopping) sees its cloud device.
func TestCloudLease_BindRunsUnderTheLeaseLock(t *testing.T) {
	env := newHubEnv(t, Config{})
	var held, published bool
	e, le := env.s.leaseDevice(DeviceInfo{ID: "mock-1"}, "bob", 0, claimNone, func(e *leaseEntry) {
		if env.s.leaseMu.TryLock() {
			env.s.leaseMu.Unlock()
		} else {
			held = true
		}
		published = env.s.leases.lookup(e.id) == e
	})
	if le != nil || e == nil || !held || !published {
		t.Fatalf("bind: lease %v %v, held %v, published %v", e, le, held, published)
	}
}

// A selector's OS bounds apply to the hub's own devices too, and what only a
// cloud launch reads is refused without select.location cloud.
func TestCloudLease_SelectorFieldsWithoutCloud(t *testing.T) {
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	// The shared simulator runs 26.5.
	for _, sel := range []LeaseSelector{{Platform: "ios", OSVersionMin: "27"}, {Platform: "ios", OSVersionMax: "26.4"}} {
		if _, err := env.remote.AcquireLease(ctx, LeaseRequest{Select: &sel}); codeOf(t, err) != CodeDeviceNotFound {
			t.Fatalf("%+v: %v", sel, err)
		}
	}
	res, err := env.remote.AcquireLease(ctx, LeaseRequest{Select: &LeaseSelector{Platform: "ios", Kind: "simulator", OSVersionMin: "26", OSVersionMax: "26"}})
	if err != nil || res.Lease.Device != "SIM-UP" {
		t.Fatalf("in range: %+v %v", res, err)
	}
	for _, req := range []LeaseRequest{
		{Select: &LeaseSelector{Platform: "ios", Provider: "devicefarm"}},
		{Select: &LeaseSelector{Platform: "ios"}, Cloud: &cloudprov.CloudSpec{IdleReleaseSec: 300}},
		{Select: &LeaseSelector{Platform: "ios", OSVersionMin: "x"}},
	} {
		if _, err := env.remote.AcquireLease(ctx, req); codeOf(t, err) != CodeUsage {
			t.Fatalf("%+v: %v", req.Select, err)
		}
	}
}

// A per-device maximum named for the model refuses an over-long window
// before any session is launched.
func TestCloudLease_ModelMaximumBeforeTheLaunch(t *testing.T) {
	h := newCloudHub(t, Config{LeaseMaxDevices: []LeaseMax{{Device: "Google Pixel 8", Max: 30 * time.Minute}}})
	ctx := context.Background()
	_, err := h.remote.AcquireLease(ctx, LeaseRequest{Select: cloudSel(), TTLSec: 3600})
	if codeOf(t, err) != CodeUsage || !strings.Contains(err.Error(), "maximum of 30m0s") {
		t.Fatalf("over the model's maximum: %v", err)
	}
	h.f.fakeProvider.mu.Lock()
	n := strings.Count(strings.Join(h.f.fakeProvider.calls, " "), "Allocate")
	h.f.fakeProvider.mu.Unlock()
	if n != 0 {
		t.Fatalf("launched %d session(s) for a refused window", n)
	}
	if _, err := h.remote.AcquireLease(ctx, LeaseRequest{Select: cloudSel(), TTLSec: 1800}); err != nil {
		t.Fatalf("within it: %v", err)
	}
}
