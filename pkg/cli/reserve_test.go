package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

func TestParseStartAt(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"2026-11-24T02:00:00Z":      time.Date(2026, 11, 24, 2, 0, 0, 0, time.UTC),
		"2026-11-23T18:00:00-08:00": time.Date(2026, 11, 24, 2, 0, 0, 0, time.UTC),
		"90m":                       now.Add(90 * time.Minute),
		"2h":                        now.Add(2 * time.Hour),
		"30":                        now.Add(30 * time.Second),
	} {
		if got, err := parseStartAt(in, now); err != nil || !got.Equal(want) {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"tomorrow", "-5m", "2026-11-24 02:00"} {
		if _, err := parseStartAt(bad, now); !isCode(err, daemon.CodeUsage) {
			t.Errorf("%q accepted: %v", bad, err)
		}
	}
}

// A client may hold several leases on one device: the one that started
// last is used, else the reservation that starts first; a record without
// a start has started and ranks earliest; the recorded end is not weighed.
func TestCurrentLease(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	recs := []leaseRecord{
		{ID: "old", Device: "d"},
		{ID: "ended", Device: "d", StartsAt: at(-2 * time.Hour), ExpiresAt: at(-time.Hour)},
		{ID: "later", Device: "d", StartsAt: at(3 * time.Hour)},
		{ID: "soon", Device: "d", StartsAt: at(time.Hour)},
		{ID: "other", Device: "e", StartsAt: at(-time.Minute)},
	}
	if got := currentLease(recs, "d", now); got == nil || got.ID != "ended" {
		t.Fatalf("latest started: %+v", got)
	}
	if got := currentLease(recs[:1], "d", now); got == nil || got.ID != "old" {
		t.Fatalf("legacy: %+v", got)
	}
	if got := currentLease(recs[2:4], "d", now); got == nil || got.ID != "soon" {
		t.Fatalf("soonest reservation: %+v", got)
	}
	if got := currentLease([]leaseRecord{recs[2], recs[0]}, "d", now); got == nil || got.ID != "old" {
		t.Fatalf("a started legacy record wins over a reservation: %+v", got)
	}
	if got := currentLease(recs, "x", now); got != nil {
		t.Fatalf("none: %+v", got)
	}
	// Once its start passes, a reservation is the one that started last.
	if got := currentLease(recs, "d", now.Add(90*time.Minute)); got == nil || got.ID != "soon" {
		t.Fatalf("after the start: %+v", got)
	}
}

func TestLeaseFile_SeveralLeasesPerDevice(t *testing.T) {
	t.Setenv(daemon.EnvHome, t.TempDir())
	now := time.Now().UTC()
	at := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }
	recordLease("h", daemon.Lease{ID: "a", Device: "d", StartsAt: at(-time.Minute), ExpiresAt: at(time.Hour)})
	recordLease("h", daemon.Lease{ID: "b", Device: "d", StartsAt: at(2 * time.Hour), ExpiresAt: at(3 * time.Hour)})
	// Re-recording a lease id replaces its entry; the device's other stays.
	recordLease("h", daemon.Lease{ID: "a", Device: "d", StartsAt: at(-time.Minute), ExpiresAt: at(2 * time.Hour)})
	recs := leasesFor("h")
	if len(recs) != 2 {
		t.Fatalf("records: %+v", recs)
	}
	if r := findLease("h", "d"); r == nil || r.ID != "a" || r.ExpiresAt != at(2*time.Hour) {
		t.Fatalf("find: %+v", r)
	}
	dropLease("h", "a")
	if r := findLease("h", "d"); r == nil || r.ID != "b" || r.StartsAt != at(2*time.Hour) {
		t.Fatalf("after the drop: %+v", r)
	}
	// A started lease replaces the device's other started records (they
	// have ended) and keeps its reservations.
	recordLease("h", daemon.Lease{ID: "old", Device: "d", StartsAt: at(-2 * time.Hour), ExpiresAt: at(-time.Hour)})
	recordLease("h", daemon.Lease{ID: "new", Device: "d", StartsAt: at(-time.Minute), ExpiresAt: at(time.Hour)})
	ids := map[string]bool{}
	for _, r := range leasesFor("h") {
		ids[r.ID] = true
	}
	if ids["old"] || !ids["new"] || !ids["b"] {
		t.Fatalf("after a second started lease: %v", ids)
	}
	// A file in the old shape (no startsAt) still loads.
	if err := saveLeases([]leaseRecord{{Hub: "h", Device: "x", ID: "legacy", ExpiresAt: at(time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	if r := findLease("h", "x"); r == nil || r.ID != "legacy" {
		t.Fatalf("legacy: %+v", r)
	}
}

func TestLease_PrintingReservations(t *testing.T) {
	var b bytes.Buffer
	printDevices(&b, []daemon.DeviceInfo{
		{ID: "a", Platform: "mock", Available: true, Reservations: []daemon.DeviceLease{{Holder: "nightly", StartsAt: "2026-11-24T02:00:00Z", ExpiresAt: "2026-11-24T03:00:00Z"}}},
		{ID: "b", Platform: "mock", Lease: &daemon.DeviceLease{Holder: "bob", ExpiresAt: time.Now().Add(time.Minute).Format(time.RFC3339)},
			Reservations: []daemon.DeviceLease{{Holder: "x", StartsAt: "2026-11-24T02:00:00Z"}}},
		{ID: "c", Platform: "mock", Available: true},
	})
	out := b.String()
	if !strings.Contains(out, "next nightly 2026-11-24T02:00:00Z") || !strings.Contains(out, "bob (") || !strings.Contains(out, "available") ||
		strings.Contains(out, "next x") {
		t.Fatalf("lease column:\n%s", out)
	}
	// The LEASE column is as wide as its longest cell: LOCATION lines up.
	lines := strings.Split(strings.TrimSpace(out), "\n")
	col := strings.Index(lines[0], "LOCATION")
	for _, l := range lines[1:] {
		if len(l) <= col || l[col-1] != ' ' || l[col] == ' ' {
			t.Fatalf("LOCATION misaligned at %d:\n%s", col, out)
		}
	}
}

// reserve records a reservation; before its start the hub refuses it and
// claim takes nothing; a lease taken now may not reach it; after the start
// claim records and attaches it, and commands from the home send it.
func TestReserveClaimRelease(t *testing.T) {
	_, url := leaseHub(t)
	ctx := context.Background()
	o := func() *daemonOpts { return &daemonOpts{Target: url, Token: "t", JSON: true} }
	key := leaseKey(daemon.NewHTTPClient(url, "t", nil), o())

	// Refusals before anything is sent.
	for _, tc := range []func(*daemonOpts){
		func(x *daemonOpts) { x.Device = "mock-1" },    // no --start-at
		func(x *daemonOpts) { x.lease.startAt = "1h" }, // no device or selector
		func(x *daemonOpts) { x.Device, x.lease.sel.Platform, x.lease.startAt = "mock-1", "mock", "1h" },
		func(x *daemonOpts) { x.Device, x.lease.startAt = "mock-1", "soon" },
	} {
		x := o()
		tc(x)
		if err := runReserve(ctx, x, nil); !isCode(err, daemon.CodeUsage) {
			t.Fatalf("%+v: %v", x, err)
		}
	}
	x := o()
	x.Device, x.lease.startAt = "mock-1", "2026-01-01T00:00:00Z"
	if err := runReserve(ctx, x, nil); !isCode(err, daemon.CodeUsage) {
		t.Fatalf("in the past: %v", err)
	}

	start := time.Now().Add(3 * time.Second)
	x = o()
	x.Device, x.lease.startAt, x.lease.ttl = "mock-1", start.UTC().Format(time.RFC3339), 60
	if err := runReserve(ctx, x, nil); err != nil {
		t.Fatal(err)
	}
	rec := findLease(key, "mock-1")
	if rec == nil || rec.StartsAt == "" || startedRecord(*rec) {
		t.Fatalf("on file: %+v", rec)
	}
	resID := rec.ID
	// Overlap, and a lease now whose default window reaches it, are refused;
	// the reservation on file does not stand in for a lease.
	x = o()
	x.Device, x.lease.startAt = "mock-1", start.Add(10*time.Second).UTC().Format(time.RFC3339)
	if err := runReserve(ctx, x, nil); !isCode(err, daemon.CodeDeviceInUse) {
		t.Fatalf("overlap: %v", err)
	}
	x = o()
	x.Device, x.lease.noAttach = "mock-1", true
	if err := runLease(ctx, x, nil); !isCode(err, daemon.CodeDeviceInUse) {
		t.Fatalf("lease into a reservation: %v", err)
	}
	// Before the start: commands carry it and get LEASE_NOT_STARTED, and the
	// record stays.
	c := daemon.NewHTTPClient(url, "t", nil)
	wireLeases(c, o())
	if _, err := c.Command(ctx, "mock-1", "tapOn", "x", true); !isCode(err, daemon.CodeLeaseNotStarted) {
		t.Fatalf("before the start: %v", err)
	}
	if findLease(key, "mock-1") == nil {
		t.Fatal("LEASE_NOT_STARTED dropped the record")
	}
	// claim before the start records nothing.
	dropLease(key, resID)
	x = o()
	x.lease.id = resID
	if err := runClaim(ctx, x, nil); !isCode(err, daemon.CodeLeaseNotStarted) {
		t.Fatalf("claim before the start: %v", err)
	}
	if findLease(key, "mock-1") != nil {
		t.Fatal("claim before the start recorded the lease")
	}
	// renew skips reservations; release --lease of an id not on file asks the hub.
	other := o()
	other.Device, other.lease.startAt, other.lease.ttl = "mock-1", start.Add(time.Hour).UTC().Format(time.RFC3339), 60
	if err := runReserve(ctx, other, nil); err != nil {
		t.Fatal(err)
	}
	laterID := findLease(key, "mock-1").ID
	if err := runRenew(ctx, o(), nil); !isCode(err, daemon.CodeLeaseRequired) {
		t.Fatalf("renew with only reservations on file: %v", err)
	}
	dropLease(key, laterID)
	x = o()
	x.lease.id = laterID
	if err := runRelease(ctx, x, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Lease(ctx, laterID); !isCode(err, daemon.CodeLeaseExpired) {
		t.Fatalf("release --lease: %v", err)
	}
	// An id the hub does not hold is the error, not a release.
	x = o()
	x.lease.id = "no-such-lease"
	if err := runRelease(ctx, x, nil); !isCode(err, daemon.CodeLeaseExpired) || !strings.Contains(err.Error(), "no lease no-such-lease") {
		t.Fatalf("release --lease of an unknown id: %v", err)
	}

	// After the start: claim records and attaches it.
	time.Sleep(time.Until(start) + 1500*time.Millisecond)
	x = o()
	x.lease.id = resID
	x.Attach.Platform = "mock"
	if err := runClaim(ctx, x, nil); err != nil {
		t.Fatal(err)
	}
	if r := findLease(key, "mock-1"); r == nil || r.ID != resID {
		t.Fatalf("claimed: %+v", r)
	}
	c2 := daemon.NewHTTPClient(url, "t", nil)
	wireLeases(c2, o())
	if _, err := c2.Command(ctx, "mock-1", "tapOn", "x", true); err != nil {
		t.Fatalf("a command under the claimed lease: %v", err)
	}
	// Two reservations for later, then release --lease cancels one and
	// release --device everything on the device.
	r1, r2 := o(), o()
	r1.Device, r1.lease.startAt, r1.lease.ttl = "mock-1", "2h", 60
	r2.Device, r2.lease.startAt, r2.lease.ttl = "mock-1", "3h", 60
	if err := runReserve(ctx, r1, nil); err != nil {
		t.Fatal(err)
	}
	if err := runReserve(ctx, r2, nil); err != nil {
		t.Fatal(err)
	}
	// wireLeases sends the started lease, not a reservation, for the device.
	c3 := daemon.NewHTTPClient(url, "t", nil)
	wireLeases(c3, o())
	if c3.LeaseOf("mock-1") != resID {
		t.Fatalf("wired %q, want the started %q", c3.LeaseOf("mock-1"), resID)
	}
	var first string
	for _, r := range leasesFor(key) {
		if r.ID != resID && (first == "" || r.StartsAt < findByID(key, first).StartsAt) {
			first = r.ID
		}
	}
	x = o()
	x.lease.id = first
	if err := runRelease(ctx, x, nil); err != nil {
		t.Fatal(err)
	}
	if ls, _ := c.Leases(ctx); len(ls) != 2 {
		t.Fatalf("after release --lease: %+v", ls)
	}
	x = o()
	x.Device = "mock-1"
	if err := runRelease(ctx, x, nil); err != nil {
		t.Fatal(err)
	}
	if ls, _ := c.Leases(ctx); len(ls) != 0 || len(leasesFor(key)) != 0 {
		t.Fatalf("after release --device: %+v %+v", ls, leasesFor(key))
	}
	for _, bad := range []func(*daemonOpts){
		func(x *daemonOpts) {},
		func(x *daemonOpts) { x.Device, x.lease.all = "mock-1", true },
		func(x *daemonOpts) { x.Device, x.lease.id = "mock-1", "l" },
	} {
		x := o()
		bad(x)
		if err := runRelease(ctx, x, nil); !isCode(err, daemon.CodeUsage) {
			t.Fatalf("release %+v: %v", x, err)
		}
	}
	x = o()
	if err := runClaim(ctx, x, nil); !isCode(err, daemon.CodeUsage) {
		t.Fatalf("claim without --lease: %v", err)
	}
}

func findByID(key, id string) leaseRecord {
	for _, r := range leasesFor(key) {
		if r.ID == id {
			return r
		}
	}
	return leaseRecord{}
}

// A hub that predates reservations answers "no route": reserve names it.
func TestReserve_HubWithoutReservations(t *testing.T) {
	t.Setenv(daemon.EnvHome, t.TempDir())
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/status" {
			_ = json.NewEncoder(w).Encode(daemon.StatusResult{Envelope: daemon.Envelope{OK: true}, Daemon: daemon.Info{Name: "old", Hub: "old-hub"}})
			return
		}
		e := daemon.Errorf(daemon.CodeUsage, "%s", daemon.NoRouteMessage(r.Method, r.URL.Path))
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(daemon.FailureEnvelope(e))
	}))
	defer hs.Close()
	o := &daemonOpts{Target: hs.URL, Token: "t", JSON: true}
	o.lease.sel.Platform, o.lease.startAt = "ios", "1h"
	err := runReserve(context.Background(), o, nil)
	if !isCode(err, daemon.CodeUsage) || !strings.Contains(err.Error(), "does not take reservations") {
		t.Fatalf("old hub: %v", err)
	}
}

// Through a current hub, a peer that predates reservations is named by
// the mesh, and reserve passes that answer on.
func TestReserve_OlderPeerNamedByTheMesh(t *testing.T) {
	t.Setenv(daemon.EnvHome, t.TempDir())
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/status" {
			_ = json.NewEncoder(w).Encode(daemon.StatusResult{Envelope: daemon.Envelope{OK: true}, Daemon: daemon.Info{Name: "hub-b", Hub: "hub-b"}})
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(daemon.FailureEnvelope(daemon.ReservationsUnsupported("hub-a")))
	}))
	defer hs.Close()
	o := &daemonOpts{Target: hs.URL, Token: "t", JSON: true}
	o.lease.sel.Platform, o.lease.startAt = "ios", "1h"
	err := runReserve(context.Background(), o, nil)
	if !isCode(err, daemon.CodeUsage) || !strings.Contains(err.Error(), "hub hub-a does not take reservations") {
		t.Fatalf("older peer: %v", err)
	}
}

func TestReserveAndClaimFlags(t *testing.T) {
	o, _, err := parseDaemonArgs([]string{"--platform", "ios", "--kind", "device", "--name", "Lab", "--os", "26", "--os-min", "17",
		"--os-max", "26", "--hub", "lab-a", "--start-at", "2h", "--ttl", "30m", "--holder", "nightly"}, reserveFlags, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := daemon.LeaseSelector{Platform: "ios", Kind: "device", Name: "Lab", OSVersion: "26", OSVersionMin: "17", OSVersionMax: "26", Hub: "lab-a"}
	if o.lease.sel != want || o.lease.startAt != "2h" || o.lease.ttl != 1800 || o.lease.holder != "nightly" {
		t.Fatalf("reserve flags: %+v", o.lease)
	}
	o, _, err = parseDaemonArgs([]string{"--lease", "l1@hub", "--no-attach", "--platform", "mock"}, claimFlags, nil)
	if err != nil || o.lease.id != "l1@hub" || !o.lease.noAttach || o.Attach.Platform != "mock" {
		t.Fatalf("claim flags: %+v %v", o.lease, err)
	}
	o, _, err = parseDaemonArgs([]string{"--lease", "l2"}, releaseFlags, nil)
	if err != nil || o.lease.id != "l2" {
		t.Fatalf("release flags: %+v %v", o.lease, err)
	}
}

// reserve and claim refuse a daemon that is not a hub, and claim without
// --no-attach on an active lease records it without attaching.
func TestReserveClaim_NotAHubAndNoAttach(t *testing.T) {
	t.Setenv(daemon.EnvHome, t.TempDir())
	plain := daemon.New(daemon.Config{Name: "plain", Token: "t", HTTPAddr: ":7433", ReportsDir: t.TempDir()}, daemon.Deps{
		ListDevices: func(string) []daemon.DeviceInfo { return nil },
	})
	hs := httptest.NewServer(plain.TCPHandler())
	defer hs.Close()
	ctx := context.Background()
	o := &daemonOpts{Target: hs.URL, Token: "t", JSON: true}
	o.Device, o.lease.startAt = "x", "1h"
	if err := runReserve(ctx, o, nil); !isCode(err, daemon.CodeUsage) || !strings.Contains(err.Error(), "not a hub") {
		t.Fatalf("reserve on a plain daemon: %v", err)
	}
	o = &daemonOpts{Target: hs.URL, Token: "t", JSON: true}
	o.lease.id = "l"
	if err := runClaim(ctx, o, nil); !isCode(err, daemon.CodeUsage) || !strings.Contains(err.Error(), "not a hub") {
		t.Fatalf("claim on a plain daemon: %v", err)
	}

	_, url := leaseHub(t)
	c := daemon.NewHTTPClient(url, "t", nil)
	res, err := c.AcquireLease(ctx, daemon.LeaseRequest{Device: "mock-1", TTLSec: 60})
	if err != nil {
		t.Fatal(err)
	}
	o = &daemonOpts{Target: url, Token: "t"}
	o.lease.id, o.lease.noAttach = res.Lease.ID, true
	if err := runClaim(ctx, o, nil); err != nil {
		t.Fatal(err)
	}
	key := leaseKey(c, o)
	if r := findLease(key, "mock-1"); r == nil || r.ID != res.Lease.ID {
		t.Fatalf("claimed: %+v", r)
	}
	if d, _ := c.Device(ctx, "mock-1"); d.Attached {
		t.Fatal("--no-attach attached")
	}
	o = &daemonOpts{Target: url, Token: "t"}
	o.lease.id = "no-such-lease"
	if err := runClaim(ctx, o, nil); !isCode(err, daemon.CodeLeaseExpired) {
		t.Fatalf("an unknown lease: %v", err)
	}
	// Printed, not JSON: a reservation's line names its start.
	o = &daemonOpts{Target: url, Token: "t"}
	o.Device, o.lease.startAt, o.lease.ttl = "mock-1", "2h", 60
	if err := runReserve(ctx, o, nil); err != nil {
		t.Fatal(err)
	}
}

// renew without --device drops a lease on file that has ended (a
// reservation followed it on its device) and renews the rest.
func TestRenew_SkipsAnEndedLeaseOnFile(t *testing.T) {
	_, url := leaseHub(t)
	ctx := context.Background()
	o := &daemonOpts{Target: url, Token: "t", JSON: true}
	c := daemon.NewHTTPClient(url, "t", nil)
	key := leaseKey(c, o)
	res, err := c.AcquireLease(ctx, daemon.LeaseRequest{Device: "mock-1", TTLSec: 60})
	if err != nil {
		t.Fatal(err)
	}
	// An ended lease on file beside the live one, as a reservation recorded
	// while it was pending leaves the lease before it.
	recordLease(key, res.Lease)
	updateLeases(func(recs []leaseRecord) []leaseRecord {
		return append(recs, leaseRecord{Hub: key, Device: "mock-1", ID: "ended-lease",
			StartsAt: time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339), ExpiresAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)})
	})
	o.lease.ttl = 30
	if err := runRenew(ctx, o, nil); err != nil {
		t.Fatalf("renew with an ended lease on file: %v", err)
	}
	if l, _ := c.Lease(ctx, res.Lease.ID); l.Lease.TTLSec != 30 {
		t.Fatalf("the live lease was not renewed: %+v", l.Lease)
	}
	for _, r := range leasesFor(key) {
		if r.ID == "ended-lease" {
			t.Fatal("the ended lease stayed on file")
		}
	}
	// Only ended leases on file: the error.
	if _, err := c.ReleaseLease(ctx, res.Lease.ID); err != nil {
		t.Fatal(err)
	}
	if err := runRenew(ctx, o, nil); !isCode(err, daemon.CodeLeaseExpired) {
		t.Fatalf("renew with only ended leases: %v", err)
	}
}

// A reservation this machine's clock takes for started, which the hub says
// has not, is passed over by renew, and the live lease is renewed.
func TestRenew_PassesOverANotStartedReservation(t *testing.T) {
	_, url := leaseHub(t)
	ctx := context.Background()
	o := &daemonOpts{Target: url, Token: "t", JSON: true}
	c := daemon.NewHTTPClient(url, "t", nil)
	key := leaseKey(c, o)
	l, err := c.AcquireLease(ctx, daemon.LeaseRequest{Device: "mock-1", TTLSec: 60})
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Reserve(ctx, daemon.ReservationRequest{Device: "mock-1", StartAt: l.Lease.ExpiresAt, TTLSec: 60})
	if err != nil {
		t.Fatal(err)
	}
	recordLease(key, l.Lease)
	// Recorded as if this clock were a minute ahead: started.
	updateLeases(func(recs []leaseRecord) []leaseRecord {
		return append(recs, leaseRecord{Hub: key, Device: "mock-1", ID: r.Lease.ID, StartsAt: time.Now().Add(-time.Second).UTC().Format(time.RFC3339)})
	})
	o.lease.ttl = 50
	if err := runRenew(ctx, o, nil); err != nil {
		t.Fatalf("renew: %v", err)
	}
	if got, _ := c.Lease(ctx, l.Lease.ID); got.Lease.TTLSec != 50 {
		t.Fatalf("the live lease was not renewed: %+v", got.Lease)
	}
}

func TestSessionSelectsActiveAlternateGrant(t *testing.T) {
	for _, active := range []bool{true, false} {
		t.Run(fmt.Sprint("active=", active), func(t *testing.T) {
			_, url := leaseHub(t)
			ctx := t.Context()
			o := &daemonOpts{Target: url, Token: "t", JSON: true}
			c := daemon.NewHTTPClient(url, "t", nil)
			var next *daemon.LeaseResult
			var err error
			if active {
				next, err = c.AcquireLease(ctx, daemon.LeaseRequest{Device: "mock-1"})
			} else {
				next, err = c.Reserve(ctx, daemon.ReservationRequest{Device: "mock-1", StartAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339), TTLSec: 60})
			}
			if err != nil {
				t.Fatal(err)
			}
			key, device := leaseKey(c, o), next.Lease.Device
			recordLease(key, daemon.Lease{ID: "ended", Device: device})
			later := next.Lease
			later.StartsAt = time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
			recordLease(key, later)
			wireLeases(c, o)
			_, _, err = leaseAndAttach(ctx, c, o, device, true)
			if !active {
				if !isCode(err, daemon.CodeLeaseNotStarted) {
					t.Fatalf("scheduled alternate: %v", err)
				}
				return
			}
			if err != nil || c.LeaseOf(device) != next.Lease.ID {
				t.Fatalf("alternate grant: %s, %v", c.LeaseOf(device), err)
			}
		})
	}
}
