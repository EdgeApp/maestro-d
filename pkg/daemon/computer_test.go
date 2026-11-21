package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
)

var macDevice = DeviceInfo{ID: "mac", Platform: "macos", Kind: "computer", Name: "Demo Mac", OSVersion: "26.6.1", State: "ready", Ready: true}

func TestLeaseSelector_ComputerOnlyWhenAsked(t *testing.T) {
	for _, tc := range []struct {
		sel  LeaseSelector
		want bool
	}{
		{LeaseSelector{}, false},
		{LeaseSelector{OSVersion: "26"}, false},
		{LeaseSelector{Platform: "ios"}, false},
		{LeaseSelector{Platform: "macos"}, true},
		{LeaseSelector{Platform: "macOS", OSVersion: "26"}, true},
		{LeaseSelector{Kind: "computer"}, true},
		{LeaseSelector{Kind: "Computer", OSVersion: "26"}, true},
		{LeaseSelector{Name: "Demo Mac"}, true},
		{LeaseSelector{Name: "mac"}, true},
	} {
		if got := tc.sel.matches(macDevice); got != tc.want {
			t.Errorf("%s: %v", tc.sel, got)
		}
	}
}

// The computer is local only: a remote caller never sees it, leases it or
// attaches it, whatever --share-device says; the unix socket does all three.
func TestLeases_ComputerLocalOnly(t *testing.T) {
	ctx := context.Background()
	for _, cfg := range []Config{{}, {ShareDevices: []string{"mac", "Demo Mac", "mock-1"}}} {
		env := newHubEnv(t, cfg)
		env.mu.Lock()
		env.phones = append(env.phones, macDevice)
		env.mu.Unlock()

		devs, err := env.remote.Devices(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range devs {
			if d.ID == "mac" {
				t.Fatalf("%v: remote listing has the computer", cfg.ShareDevices)
			}
		}
		_, err = env.remote.Device(ctx, "mac")
		wantErrCode(t, err, CodeDeviceNotFound)
		_, err = env.remote.AcquireLease(ctx, LeaseRequest{Device: "mac"})
		wantErrCode(t, err, CodeDeviceNotFound)
		_, err = env.remote.AcquireLease(ctx, LeaseRequest{Select: &LeaseSelector{Kind: "computer"}})
		wantErrCode(t, err, CodeDeviceNotFound)
		evCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err = env.remote.Events(evCtx, "mac", 0, func(Event) bool { return false })
		cancel()
		wantErrCode(t, err, CodeDeviceNotFound)
		_, err = env.remote.AcquireLease(ctx, LeaseRequest{Device: "mock-1", Attach: &AttachConfig{Platform: "macos"}})
		wantErrCode(t, err, CodeForbidden)
		if env.s.shared("mac") {
			t.Fatal("shared(mac)")
		}

		// Locally: by id, and by a selector that asks for it.
		if all, _ := env.local.Devices(ctx, ""); !hasID(all, "mac") {
			t.Fatal("local listing lacks the computer")
		}
		l := env.lease(env.local, LeaseRequest{Device: "mac"}).Lease
		if _, err := env.local.ReleaseLease(ctx, l.ID); err != nil {
			t.Fatal(err)
		}
		l = env.lease(env.local, LeaseRequest{Select: &LeaseSelector{Kind: "computer"}}).Lease
		if l.Device != "mac" {
			t.Fatalf("kind selector leased %s", l.Device)
		}
		_, _ = env.local.ReleaseLease(ctx, l.ID)
		// A selector for a simulator never lands on the Mac.
		l = env.lease(env.local, LeaseRequest{Select: &LeaseSelector{OSVersion: "26"}}).Lease
		if l.Device == "mac" {
			t.Fatal("an osVersion selector leased the computer")
		}
		_, _ = env.local.ReleaseLease(ctx, l.ID)
	}
}

// A cached positive answer for another id and an empty --share-device never
// share the computer, nor does a registry record of kind computer under
// another id.
func TestShared_ComputerBeforeCache(t *testing.T) {
	s := &Server{cfg: Config{}, sharedIDs: map[string]bool{"mac": true}, shareSeen: map[string]shareAnswer{}}
	s.registry = NewRegistry("plain", func(string) []DeviceInfo { return nil })
	if s.shared("mac") {
		t.Fatal("shared(mac) with an empty --share-device")
	}
	s.registry.MarkAttached(DeviceInfo{ID: "other-mac", Platform: "macos", Kind: "computer"})
	if s.shared("other-mac") {
		t.Fatal("a computer record shared")
	}
	if s.sharedInfo(macDevice) {
		t.Fatal("sharedInfo(mac)")
	}
	if !s.shared("mock-1") {
		t.Fatal("an ordinary device stopped being shared")
	}
}

func TestShutdownDevice_ComputerIsANoOp(t *testing.T) {
	if err := shutdownDevice(macDevice); err != nil {
		t.Fatalf("stop mac: %v", err)
	}
	if err := shutdownDevice(DeviceInfo{ID: "phone", Platform: "ios", Kind: "device"}); err == nil || !strings.Contains(err.Error(), "physical device") {
		t.Fatalf("a phone: %v", err)
	}
}

func TestCheckRemoteAttach_Computer(t *testing.T) {
	if e := checkRemoteAttach(&AttachConfig{Platform: "macOS"}); e == nil || e.Code != CodeForbidden || !strings.Contains(e.Message, "local only") {
		t.Fatalf("%v", e)
	}
	if e := checkRemoteAttach(&AttachConfig{Platform: "android"}); e != nil {
		t.Fatalf("android refused: %v", e)
	}
}

func hasID(list []DeviceInfo, id string) bool {
	for _, d := range list {
		if d.ID == id {
			return true
		}
	}
	return false
}

// A driver factory's own code survives attach: a bad computer.json is
// USAGE, not DEVICE_ERROR.
func TestAttach_KeepsTheFactorysCode(t *testing.T) {
	env := newTestEnv(t, 0)
	env.s.deps.NewDriver = func(context.Context, string, AttachConfig) (core.Driver, DriverInfo, func(), error) {
		return nil, DriverInfo{}, nil, Errorf(CodeUsage, "computer.json is readable by others (mode 0644)")
	}
	_, err := env.c.Attach(context.Background(), "mac", AttachConfig{})
	if codeOf(t, err) != CodeUsage || !strings.Contains(err.Error(), "attach mac: computer.json is readable by others") {
		t.Fatalf("%v", err)
	}
	env.s.deps.NewDriver = func(context.Context, string, AttachConfig) (core.Driver, DriverInfo, func(), error) {
		return nil, DriverInfo{}, nil, errors.New("no such device")
	}
	if _, err := env.c.Attach(context.Background(), "mock-1", AttachConfig{}); codeOf(t, err) != CodeDeviceError {
		t.Fatalf("a plain error: %v", err)
	}
}
