package cli

// hub_reservations_e2e_test.go: two hub binaries with mock devices,
// reserving through the entry hub over REST: a reservation by id on the
// other hub's device and one by selector, the overlap refusals, the
// lease that waits for its start, the sweeper starting it, a routed
// command under it, and a cancel.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

// startTwoHubs starts hub-a (mock-1, mock-2) and hub-b (mock-3) linked
// by --peer, and returns their infos.
func startTwoHubs(t *testing.T, e *e2e, prefix string, extraA ...string) (daemon.Info, daemon.Info, []string, []string) {
	t.Helper()
	homeA, homeB := filepath.Join(e.cwd, prefix+"-home-a"), filepath.Join(e.cwd, prefix+"-home-b")
	writeSharedMocks(t, homeA, "mock-1", "mock-2")
	writeSharedMocks(t, homeB, "mock-3")
	envA := []string{daemon.EnvHome + "=" + homeA, "MAESTRO_D_MOCK_DEVICES=mock-1,mock-2"}
	envB := []string{daemon.EnvHome + "=" + homeB, "MAESTRO_D_MOCK_DEVICES=mock-3"}
	t.Cleanup(func() {
		e.runEnv(envB, "maestro-d-hub", "stop")
		e.runEnv(envA, "maestro-d-hub", "stop")
	})
	mesh := []string{"--listen", "127.0.0.1:0", "--no-mdns", "--token", "res-tok", "--peer-token", "res-peer", "--json"}
	started := func(r result) daemon.Info {
		t.Helper()
		var out struct {
			Daemon daemon.Info `json:"daemon"`
		}
		if r.code != 0 || json.Unmarshal([]byte(r.stdout), &out) != nil || out.Daemon.HTTPAddr == "" {
			t.Fatalf("hub start: exit %d\n%s%s", r.code, r.stdout, r.stderr)
		}
		return out.Daemon
	}
	a := started(e.runEnv(envA, append(append([]string{"maestro-d-hub", "start", "--name", "hub-a"}, extraA...), mesh...)...))
	b := started(e.runEnv(envB, append([]string{"maestro-d-hub", "start", "--name", "hub-b", "--peer", a.HTTPAddr}, mesh...)...))
	return a, b, envA, envB
}

func TestE2E_HubReservations(t *testing.T) {
	e := newE2E(t)
	_, b, _, _ := startTwoHubs(t, e, "res", "--reservation-lead", "2h")
	c := daemon.NewHTTPClient("http://"+b.HTTPAddr, "res-tok", nil)
	ctx := context.Background()
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if devs, err := c.Devices(ctx, "mock"); err == nil && len(devs) == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("hub-b never listed hub-a's devices")
		}
	}
	// Far enough ahead that every check before the start runs before it,
	// even with the whole suite running under -race.
	start := time.Now().Add(10 * time.Second).UTC().Truncate(time.Second).Add(time.Second)
	rfc := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }

	// By id through hub-b, on hub-a's device.
	res, err := c.Reserve(ctx, daemon.ReservationRequest{Device: "mock-1@hub-a", StartAt: rfc(start), TTLSec: 30, Holder: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	l := res.Lease
	if l.State != daemon.LeaseScheduled || !strings.HasSuffix(l.ID, "@hub-a") || l.StartsAt != rfc(start) {
		t.Fatalf("reserved: %+v", l)
	}
	// hub-a's lead is 2h: a start past it is refused by hub-a.
	if _, err := c.Reserve(ctx, daemon.ReservationRequest{Device: "mock-2@hub-a", StartAt: rfc(start.Add(3 * time.Hour))}); !isCode(err, daemon.CodeUsage) {
		t.Fatalf("over hub-a's lead: %v", err)
	}
	// An overlapping reservation, and a lease taken now whose default
	// window reaches it, are refused.
	if _, err := c.Reserve(ctx, daemon.ReservationRequest{Device: "mock-1@hub-a", StartAt: rfc(start.Add(10 * time.Second)), TTLSec: 30}); !isCode(err, daemon.CodeDeviceInUse) {
		t.Fatalf("overlap: %v", err)
	}
	if _, err := c.AcquireLease(ctx, daemon.LeaseRequest{Device: "mock-1@hub-a"}); !isCode(err, daemon.CodeDeviceInUse) {
		t.Fatalf("a lease now into the reservation: %v", err)
	}
	// A selector reservation takes hub-b's own device first.
	sel, err := c.Reserve(ctx, daemon.ReservationRequest{Select: &daemon.LeaseSelector{Platform: "mock"}, StartAt: rfc(start.Add(time.Hour)), TTLSec: 60})
	if err != nil || sel.Lease.Device != "mock-3@hub-b" {
		t.Fatalf("selector: %+v %v", sel, err)
	}
	devs, _ := c.Devices(ctx, "mock")
	for _, d := range devs {
		if d.ID == "mock-1@hub-a" && (len(d.Reservations) != 1 || d.Reservations[0].Holder != "alice") {
			t.Fatalf("listing: %+v", d)
		}
	}
	// Before the start the lease answers LEASE_NOT_STARTED, through the mesh.
	c.SetLease("mock-1@hub-a", l.ID)
	if _, err := c.Command(ctx, "mock-1@hub-a", "tapOn", "x", true); !isCode(err, daemon.CodeLeaseNotStarted) {
		t.Fatalf("before the start: %v", err)
	}
	// After it, a second client claims it by id and drives the device.
	time.Sleep(time.Until(start) + 1500*time.Millisecond)
	c2 := daemon.NewHTTPClient("http://"+b.HTTPAddr, "res-tok", nil)
	if got, err := c2.Lease(ctx, l.ID); err != nil || got.Lease.State != daemon.LeaseActive {
		t.Fatalf("after the start: %+v %v", got, err)
	}
	c2.SetLease("mock-1@hub-a", l.ID)
	if _, err := c2.Attach(ctx, "mock-1@hub-a", daemon.AttachConfig{Platform: "mock"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c2.Command(ctx, "mock-1@hub-a", "tapOn", "x", true); err != nil {
		t.Fatal(err)
	}
	// Cancel the selector's reservation through hub-b.
	if _, err := c.ReleaseLease(ctx, sel.Lease.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Lease(ctx, sel.Lease.ID); !isCode(err, daemon.CodeLeaseExpired) {
		t.Fatalf("cancelled: %v", err)
	}
}

// A hub keeps its reservations across a clean stop and a crash: one whose
// start passed while it was down starts as it comes back; a lease that
// had started is gone after the crash, as every started lease is.
func TestE2E_HubReservationsRestart(t *testing.T) {
	e := newE2E(t)
	home := filepath.Join(e.cwd, "restart-home")
	writeSharedMocks(t, home, "mock-1", "mock-2")
	env := []string{daemon.EnvHome + "=" + home, "MAESTRO_D_MOCK_DEVICES=mock-1,mock-2"}
	t.Cleanup(func() { e.runEnv(env, "maestro-d-hub", "stop") })
	start := func() daemon.Info {
		t.Helper()
		r := e.runEnv(env, "maestro-d-hub", "start", "--name", "hub-r",
			"--listen", "127.0.0.1:0", "--no-mdns", "--token", "rst-tok", "--peer-token", "rst-peer", "--json")
		var out struct {
			Daemon daemon.Info `json:"daemon"`
		}
		if r.code != 0 || json.Unmarshal([]byte(r.stdout), &out) != nil || out.Daemon.HTTPAddr == "" {
			t.Fatalf("hub start: exit %d\n%s%s", r.code, r.stdout, r.stderr)
		}
		return out.Daemon
	}
	ctx := context.Background()
	rfc := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }
	info := start()
	c := daemon.NewHTTPClient("http://"+info.HTTPAddr, "rst-tok", nil)
	later, err := c.Reserve(ctx, daemon.ReservationRequest{Device: "mock-1", StartAt: rfc(time.Now().Add(5 * time.Minute)), TTLSec: 120})
	if err != nil {
		t.Fatal(err)
	}
	// mock-2's start leaves the hub stop below room to finish first: a
	// reservation that starts while the hub runs is a started lease, which
	// a stop ends.
	soonStart := time.Now().Add(8 * time.Second).UTC().Truncate(time.Second)
	soon, err := c.Reserve(ctx, daemon.ReservationRequest{Device: "mock-2", StartAt: rfc(soonStart), TTLSec: 120})
	if err != nil {
		t.Fatal(err)
	}
	if r := e.runEnv(env, "maestro-d-hub", "stop"); r.code != 0 {
		t.Fatalf("hub stop: %s%s", r.stdout, r.stderr)
	}
	path := filepath.Join(home, "hub", "scheduled-leases.json")
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%s: %v %v", path, fi, err)
	}
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), strings.TrimSuffix(soon.Lease.ID, "@hub-r")) {
		t.Fatalf("mock-2's reservation started before the hub stopped; %s:\n%s", path, data)
	}
	time.Sleep(time.Until(soonStart) + time.Second) // mock-2's start passes while the hub is down
	info = start()
	c = daemon.NewHTTPClient("http://"+info.HTTPAddr, "rst-tok", nil)
	l1, err1 := c.Lease(ctx, strings.TrimSuffix(later.Lease.ID, "@hub-r"))
	l2, err2 := c.Lease(ctx, strings.TrimSuffix(soon.Lease.ID, "@hub-r"))
	if err1 != nil || err2 != nil || l1.Lease.State != daemon.LeaseScheduled || l2.Lease.State != daemon.LeaseActive ||
		l2.Lease.ExpiresAt != soon.Lease.ExpiresAt || l1.Lease.ID != later.Lease.ID {
		t.Fatalf("after a stop: %+v %v / %+v %v", l1, err1, l2, err2)
	}
	// A crash keeps the reservation and loses the started lease.
	if p, err := os.FindProcess(info.PID); err == nil {
		_ = p.Signal(syscall.SIGKILL)
	}
	time.Sleep(time.Second)
	info = start()
	c = daemon.NewHTTPClient("http://"+info.HTTPAddr, "rst-tok", nil)
	ls, err := c.Leases(ctx)
	if err != nil || len(ls) != 1 || ls[0].ID != later.Lease.ID || ls[0].State != daemon.LeaseScheduled {
		t.Fatalf("after a crash: %+v %v", ls, err)
	}
}
