package hubd

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

func TestLeases_AttachRunsUnderTheLease(t *testing.T) {
	env := newHubEnv(t, daemon.Config{})
	// The clock jumps past the window while the attach runs: the window is
	// fixed, so the lease has ended, and the device is detached again
	// rather than left attached without a lease.
	slow := env.s.deps.NewDriver
	env.s.deps.NewDriver = func(ctx context.Context, id string, cfg daemon.AttachConfig) (core.Driver, daemon.DriverInfo, func(), error) {
		env.clock.advance(5 * time.Second)
		return slow(ctx, id, cfg)
	}
	_, err := env.remote.AcquireLease(context.Background(), daemon.LeaseRequest{Device: "mock-1", TTLSec: 2, Attach: &daemon.AttachConfig{Platform: "mock"}})
	wantErrCode(t, err, daemon.CodeLeaseExpired)
	if ls := env.s.leases.list(); len(ls) != 0 {
		t.Fatalf("leases after a window ended in its attach: %+v", ls)
	}
	if devs, _ := env.local.Devices(context.Background(), "mock"); devs[0].Attached {
		t.Fatalf("left attached: %+v", devs[0])
	}
	// A window that outlasts the attach is granted as asked: the attach
	// does not move it.
	res, err := env.remote.AcquireLease(context.Background(), daemon.LeaseRequest{Device: "mock-1", TTLSec: 60, Attach: &daemon.AttachConfig{Platform: "mock"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := env.clock.now().Add(55 * time.Second).Format(time.RFC3339); res.Lease.ExpiresAt != want {
		t.Fatalf("expiresAt %s, want %s (from the grant)", res.Lease.ExpiresAt, want)
	}
}

// On a hub every device entry names the machine it is attached to: the
// host name, and the advertised address followed by the host's
// non-loopback IPs.
func TestHub_DeviceEntriesNameTheMachine(t *testing.T) {
	env := newHubEnv(t, daemon.Config{Advertise: "hub.example:7433"})
	devs, err := env.remote.Devices(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	host, _ := os.Hostname()
	for _, d := range devs {
		if d.Machine != host || len(d.Addresses) == 0 || d.Addresses[0] != "hub.example:7433" {
			t.Fatalf("%s: machine %q addresses %v", d.ID, d.Machine, d.Addresses)
		}
		for _, a := range d.Addresses[1:] {
			if ip := net.ParseIP(a); ip == nil || ip.IsLoopback() {
				t.Fatalf("%s: address %q", d.ID, a)
			}
		}
	}
	// A plain daemon's entries name the machine too, with no addresses
	// (it has no peers to be dialed by), and are local to a caller on it.
	plain := newTestEnv(t, 0)
	pd, _ := plain.c.Devices(context.Background(), "")
	if len(pd) == 0 || pd[0].Machine != daemon.MachineName() || pd[0].Addresses != nil || pd[0].Location != daemon.LocationLocal {
		t.Fatalf("plain daemon: %+v", pd)
	}
}
