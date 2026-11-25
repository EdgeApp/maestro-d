package hubd

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

// The computer is local only: a remote caller never sees it, leases it or
// attaches it, whatever the config lists; the unix socket does all three.
var macDevice = daemon.DeviceInfo{ID: "mac", Platform: "macos", Kind: "computer", Name: "Demo Mac", OSVersion: "26.6.1", State: "ready", Ready: true}

func TestLeases_ComputerLocalOnly(t *testing.T) {
	ctx := context.Background()
	for _, cfg := range []daemon.Config{{}, {ShareConfig: []string{"mac", "Demo Mac", "mock-1"}}} {
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
				t.Fatalf("%v: remote listing has the computer", cfg.ShareConfig)
			}
		}
		_, err = env.remote.Device(ctx, "mac")
		wantErrCode(t, err, daemon.CodeDeviceNotFound)
		_, err = env.remote.AcquireLease(ctx, daemon.LeaseRequest{Device: "mac"})
		wantErrCode(t, err, daemon.CodeDeviceNotFound)
		_, err = env.remote.AcquireLease(ctx, daemon.LeaseRequest{Select: &daemon.LeaseSelector{Kind: "computer"}})
		wantErrCode(t, err, daemon.CodeDeviceNotFound)
		evCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err = env.remote.Events(evCtx, "mac", 0, func(daemon.Event) bool { return false })
		cancel()
		wantErrCode(t, err, daemon.CodeDeviceNotFound)
		_, err = env.remote.AcquireLease(ctx, daemon.LeaseRequest{Device: "mock-1", Attach: &daemon.AttachConfig{Platform: "macos"}})
		wantErrCode(t, err, daemon.CodeForbidden)
		if env.s.shared("mac") {
			t.Fatal("shared(mac)")
		}

		// Locally: by id, and by a selector that asks for it.
		if all, _ := env.local.Devices(ctx, ""); !slices.ContainsFunc(all, func(d daemon.DeviceInfo) bool { return d.ID == "mac" }) {
			t.Fatal("local listing lacks the computer")
		}
		l := env.lease(env.local, daemon.LeaseRequest{Device: "mac"}).Lease
		if _, err := env.local.ReleaseLease(ctx, l.ID); err != nil {
			t.Fatal(err)
		}
		l = env.lease(env.local, daemon.LeaseRequest{Select: &daemon.LeaseSelector{Kind: "computer"}}).Lease
		if l.Device != "mac" {
			t.Fatalf("kind selector leased %s", l.Device)
		}
		_, _ = env.local.ReleaseLease(ctx, l.ID)
		// A selector for a simulator never lands on the Mac.
		l = env.lease(env.local, daemon.LeaseRequest{Select: &daemon.LeaseSelector{OSVersion: "26"}}).Lease
		if l.Device == "mac" {
			t.Fatal("an osVersion selector leased the computer")
		}
		_, _ = env.local.ReleaseLease(ctx, l.ID)
	}
}
