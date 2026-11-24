package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readReservations(t *testing.T, path string) []reservationRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f reservationFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	return f.Reservations
}

func TestReservationsState_Path(t *testing.T) {
	t.Setenv(EnvHome, "/h")
	if got := ReservationsPath("hub"); got != "/h/hub/scheduled-leases.json" {
		t.Fatal(got)
	}
	if got := ReservationsPath("lab-2"); got != "/h/hub/daemons/lab-2/scheduled-leases.json" {
		t.Fatal(got)
	}
}

// Every change to the set of reservations rewrites the file, and only
// reservations that have not started are in it.
func TestReservationsState_Written(t *testing.T) {
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	path := ReservationsPath("hubtest")
	if err := recordCreated(CreatedRecord{Platform: "ios", ID: "SIM-UP", Name: "Shared Phone"}); err != nil {
		t.Fatal(err)
	}
	env.s.registry.deps.DeleteSimulator = func(string) error { return nil }
	a := env.mustReserve(env.remote, ReservationRequest{Device: "mock-1", StartAt: env.at(time.Hour), TTLSec: 60, Holder: "alice"})
	b := env.mustReserve(env.remote, ReservationRequest{Device: "mock-2", StartAt: env.at(10 * time.Second), TTLSec: 60})
	c := env.mustReserve(env.local, ReservationRequest{Device: "SIM-UP", StartAt: env.at(time.Hour), TTLSec: 60})
	env.lease(env.remote, LeaseRequest{Device: "SIM-DOWN", TTLSec: 60})
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file: %v %v", fi, err)
	}
	recs := readReservations(t, path)
	if len(recs) != 3 || recs[0].ID != c.ID || recs[1].ID != a.ID || recs[1].Holder != "alice" || recs[1].StartsAt != a.StartsAt || recs[1].ExpiresAt != a.ExpiresAt {
		t.Fatalf("written: %+v", recs)
	}
	// A start takes it out of the file.
	env.clock.advance(10 * time.Second)
	env.sweep()
	if recs = readReservations(t, path); len(recs) != 2 {
		t.Fatalf("after a start: %+v (%s)", recs, b.ID)
	}
	// A cancel, and a device delete, take theirs out; a restart does not
	// bring them back.
	if _, err := env.remote.ReleaseLease(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.local.DeleteDevice(ctx, "SIM-UP", false); err != nil {
		t.Fatal(err)
	}
	if recs = readReservations(t, path); len(recs) != 0 {
		t.Fatalf("after cancel and delete: %+v", recs)
	}
	// A hub that stops ends its started leases and keeps reservations.
	d := env.mustReserve(env.remote, ReservationRequest{Device: "mock-1", StartAt: env.at(2 * time.Hour), TTLSec: 60})
	env.s.endAllLeases()
	if _, err := env.remote.Lease(ctx, d.ID); err != nil {
		t.Fatalf("hub stop ended a reservation: %v", err)
	}
	if recs = readReservations(t, path); len(recs) != 1 || recs[0].ID != d.ID {
		t.Fatalf("after hub stop: %+v", recs)
	}
	// No temp files are left beside it.
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("dir: %v", entries)
	}
}

func TestReservationsState_Load(t *testing.T) {
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	path := ReservationsPath("hubtest")
	recs := []reservationRecord{
		{ID: "future", Device: "mock-1", Holder: "alice", StartsAt: env.at(time.Hour), ExpiresAt: env.at(2 * time.Hour), TTLSec: 3600, MaxSec: 28800},
		{ID: "started", Device: "mock-2", Holder: "bob", StartsAt: env.at(-time.Minute), ExpiresAt: env.at(time.Minute), TTLSec: 120, MaxSec: 28800},
		{ID: "over", Device: "SIM-UP", StartsAt: env.at(-2 * time.Hour), ExpiresAt: env.at(-time.Hour), TTLSec: 3600, MaxSec: 28800},
		{ID: "gone", Device: "PHONE-GONE", StartsAt: env.at(3 * time.Hour), ExpiresAt: env.at(4 * time.Hour), TTLSec: 3600, MaxSec: 28800},
		{ID: "clash", Device: "mock-1", StartsAt: env.at(90 * time.Minute), ExpiresAt: env.at(3 * time.Hour), TTLSec: 5400, MaxSec: 28800},
		{ID: "", Device: "mock-1", StartsAt: "x", ExpiresAt: "y"},
	}
	if err := writeReservations(path, recs); err != nil {
		t.Fatal(err)
	}
	env.s.loadReservations()
	if l, err := env.local.Lease(ctx, "future"); err != nil || l.Lease.State != LeaseScheduled || l.Lease.Holder != "alice" || l.Lease.StartsAt != recs[0].StartsAt {
		t.Fatalf("future: %+v %v", l, err)
	}
	if l, err := env.local.Lease(ctx, "started"); err != nil || l.Lease.State != LeaseActive || l.Lease.ExpiresAt != recs[1].ExpiresAt || l.Lease.Booted {
		t.Fatalf("started: %+v %v", l, err)
	}
	if _, err := env.local.Lease(ctx, "over"); err == nil {
		t.Fatal("a reservation whose window is over came back")
	}
	if l, err := env.local.Lease(ctx, "gone"); err != nil || l.Lease.State != LeaseScheduled {
		t.Fatalf("a reservation on an unlisted device is kept: %+v %v", l, err)
	}
	if _, err := env.local.Lease(ctx, "clash"); err == nil {
		t.Fatal("an overlapping record came back")
	}
	if ev := env.leaseEvents(); len(ev) != 1 || ev[0] != "lease.granted:mock-2" {
		t.Fatalf("events: %v", ev)
	}
	// The load rewrote the file with what it kept.
	if got := readReservations(t, path); len(got) != 2 || got[0].ID != "gone" || got[1].ID != "future" {
		t.Fatalf("rewritten: %+v", got)
	}
}

func TestReservationsState_UnreadableFile(t *testing.T) {
	env := newHubEnv(t, Config{})
	path := ReservationsPath("hubtest")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	env.s.loadReservations()
	if _, err := os.Stat(path + ".bad"); err != nil {
		t.Fatalf("not moved aside: %v", err)
	}
	if ls, _ := env.local.Leases(context.Background()); len(ls) != 0 {
		t.Fatalf("leases: %+v", ls)
	}
	// No file at all is no reservations.
	env.s.loadReservations()
	// A plain daemon writes nothing.
	env.s.cfg.Hub = false
	env.s.saveReservations()
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a plain daemon wrote reservations")
	}
}

// The duty to shut down a device an earlier lease booted never outlives
// the hub process: a file carrying it (from an older run) is read back
// without it, and loading shuts nothing down, whether the reservation's
// window passed or not.
func TestReservationsState_BootDutyIsNotReadBack(t *testing.T) {
	env := newHubEnv(t, Config{})
	path := ReservationsPath("hubtest")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	data := `{"reservations": [
  {"id": "over", "device": "SIM-DOWN", "startsAt": "` + env.at(-2*time.Hour) + `", "expiresAt": "` + env.at(-time.Hour) + `", "ttlSec": 3600, "maxSec": 28800, "booted": true},
  {"id": "later", "device": "SIM-UP", "startsAt": "` + env.at(time.Hour) + `", "expiresAt": "` + env.at(2*time.Hour) + `", "ttlSec": 3600, "maxSec": 28800, "booted": true}
]}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	env.s.loadReservations()
	if l, err := env.local.Lease(context.Background(), "later"); err != nil || l.Lease.Booted {
		t.Fatalf("later: %+v %v", l, err)
	}
	time.Sleep(100 * time.Millisecond)
	env.mu.Lock()
	defer env.mu.Unlock()
	if len(env.stopped) != 0 {
		t.Fatalf("the load shut down %v", env.stopped)
	}
	if strings.Contains(string(mustRead(t, path)), "booted") {
		t.Fatal("the rewritten file still carries the duty")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
