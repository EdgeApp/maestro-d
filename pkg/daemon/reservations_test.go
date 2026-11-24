package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// at is the hub's clock plus d, as RFC 3339.
func (e *hubEnv) at(d time.Duration) string { return e.clock.now().Add(d).UTC().Format(time.RFC3339) }

// reserve sends a reservation with the hub's clock as the client's.
func (e *hubEnv) reserve(c *Client, req ReservationRequest) (*LeaseResult, error) {
	if req.ClientTime == "" {
		req.ClientTime = e.at(0)
	}
	return c.Reserve(context.Background(), req)
}

func (e *hubEnv) mustReserve(c *Client, req ReservationRequest) Lease {
	e.t.Helper()
	res, err := e.reserve(c, req)
	if err != nil {
		e.t.Fatalf("reserve %+v: %v", req, err)
	}
	return res.Lease
}

// postRaw posts body to path on c's daemon as is, and returns the error.
func postRaw(t *testing.T, c *Client, path, body string) *Error {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, c.BaseURL()+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer t")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var env Envelope
	if err := json.NewDecoder(res.Body).Decode(&env); err != nil {
		t.Fatal(err)
	}
	if env.Error == nil {
		return nil
	}
	return &Error{ErrorBody: *env.Error}
}

func TestReservations_BodyClockAndLimits(t *testing.T) {
	env := newHubEnv(t, Config{LeaseMaxDevices: []LeaseMax{{Device: "mock-2", Max: time.Hour}}})
	ctx := context.Background()
	usage := func(e *Error, field string) {
		t.Helper()
		if e == nil || e.Code != CodeUsage || (field != "" && e.Details["field"] != field) {
			t.Fatalf("want USAGE naming %q, got %+v", field, e)
		}
	}
	// The body is strict: a lease-only field or a typo is refused by name.
	usage(postRaw(t, env.remote, "/v1/reservations", `{"device":"mock-1","startAt":"`+env.at(time.Hour)+`","clientTime":"`+env.at(0)+`","attach":{}}`), "attach")
	usage(postRaw(t, env.remote, "/v1/reservations", `{"devce":"mock-1"}`), "devce")
	usage(postRaw(t, env.remote, "/v1/reservations", `[]`), "")
	usage(postRaw(t, env.remote, "/v1/reservations", `{"device":"mock-1","startAt":"`+env.at(time.Hour)+`"}`), "clientTime")
	for _, req := range []ReservationRequest{{StartAt: env.at(time.Hour)}, {Device: "mock-1", Select: &LeaseSelector{}, StartAt: env.at(time.Hour)}} {
		_, err := env.reserve(env.remote, req)
		wantErrCode(t, err, CodeUsage)
	}
	_, err := env.reserve(env.remote, ReservationRequest{Device: "mock-1", StartAt: env.at(time.Hour), TTLSec: -1})
	wantErrCode(t, err, CodeUsage)
	// The client's clock: up to 60 s either way.
	for _, skew := range []time.Duration{61 * time.Second, -61 * time.Second} {
		_, err := env.reserve(env.remote, ReservationRequest{Device: "mock-1", StartAt: env.at(time.Hour), ClientTime: env.at(skew)})
		de := wantErrCode(t, err, CodeUsage)
		if de.Details["reason"] != "clock-skew" || de.Details["skewSec"] != float64(skew/time.Second) {
			t.Fatalf("skew %v: %+v", skew, de.Details)
		}
	}
	_, err = env.reserve(env.remote, ReservationRequest{Device: "mock-1", StartAt: env.at(time.Hour), ClientTime: "yesterday"})
	usage(wantErrCode(t, err, CodeUsage), "clientTime")
	for i, skew := range []time.Duration{59 * time.Second, -59 * time.Second} {
		l := env.mustReserve(env.remote, ReservationRequest{Device: "mock-1", StartAt: env.at(time.Duration(i+1) * time.Hour), TTLSec: 60, ClientTime: env.at(skew)})
		if l.State != LeaseScheduled {
			t.Fatalf("skew %v: %+v", skew, l)
		}
	}
	// startAt: required, RFC 3339, not more than 60 s past.
	for _, start := range []string{"", "tomorrow", env.at(-61 * time.Second)} {
		_, err := env.reserve(env.remote, ReservationRequest{Device: "mock-1", StartAt: start})
		usage(wantErrCode(t, err, CodeUsage), "startAt")
	}
	// A start up to 60 s past means now: an active lease at once.
	now := env.mustReserve(env.remote, ReservationRequest{Device: "SIM-UP", StartAt: env.at(-30 * time.Second), TTLSec: 60})
	if now.State != LeaseActive || now.StartsAt != env.at(0) || now.ExpiresAt != env.at(time.Minute) {
		t.Fatalf("a start in the last minute: %+v", now)
	}
	// The lead: 24 h by default.
	if l := env.mustReserve(env.remote, ReservationRequest{Device: "mock-2", StartAt: env.at(24 * time.Hour), TTLSec: 60}); l.StartsAt != env.at(24*time.Hour) {
		t.Fatalf("at the lead: %+v", l)
	}
	_, err = env.reserve(env.remote, ReservationRequest{Device: "mock-2", StartAt: env.at(24*time.Hour + time.Second)})
	if de := wantErrCode(t, err, CodeUsage); de.Details["maxLeadSec"] != float64(86400) || de.Details["maxStartAt"] != env.at(24*time.Hour) {
		t.Fatalf("over the lead: %+v", de.Details)
	}
	// The window: the device's maximum, per device too.
	_, err = env.reserve(env.remote, ReservationRequest{Device: "mock-2", StartAt: env.at(2 * time.Hour), TTLSec: 3601})
	if de := wantErrCode(t, err, CodeUsage); de.Details["maxSec"] != float64(3600) {
		t.Fatalf("over mock-2's maximum: %+v", de.Details)
	}
	if l := env.mustReserve(env.remote, ReservationRequest{Device: "mock-2", StartAt: env.at(2 * time.Hour), TTLSec: 3600}); l.TTLSec != 3600 || l.MaxSec != 3600 {
		t.Fatalf("at mock-2's maximum: %+v", l)
	}
	_, err = env.reserve(env.remote, ReservationRequest{Device: "SIM-DOWN", StartAt: env.at(time.Hour), TTLSec: 28801})
	wantErrCode(t, err, CodeUsage)
	if l := env.mustReserve(env.remote, ReservationRequest{Device: "SIM-DOWN", StartAt: env.at(time.Hour)}); l.TTLSec != 28800 {
		t.Fatalf("the default window: %+v", l)
	}
	// Nothing boots, launches a cloud device, or takes an emulator.
	for field, sel := range map[string]LeaseSelector{"boot": {Platform: "ios", Boot: true}, "location": {Platform: "ios", Location: "cloud"},
		"provider": {Platform: "ios", Provider: "devicefarm"}} {
		_, err := env.reserve(env.remote, ReservationRequest{Select: &sel, StartAt: env.at(time.Hour)})
		usage(wantErrCode(t, err, CodeUsage), field)
	}
	env.mu.Lock()
	env.emuAVD = "Pixel_9"
	env.phones = []DeviceInfo{{ID: "df-0a1b2c3d", Platform: "android", Kind: "device", Name: "Google Pixel 8", Ready: true, Provider: "devicefarm"}}
	env.mu.Unlock()
	for _, id := range []string{"emulator-5554", AVDPrefix + "Pixel_9", "df-0a1b2c3d"} {
		_, err := env.reserve(env.local, ReservationRequest{Device: id, StartAt: env.at(time.Hour)})
		if de, ok := err.(*Error); !ok || de.Code == "" || de.Code == CodeDeviceInUse {
			t.Fatalf("%s: %v", id, err)
		} else if id != AVDPrefix+"Pixel_9" && (de.Code != CodeUsage || de.Details["field"] != "device") {
			t.Fatalf("%s: %+v", id, de)
		}
	}
	_, err = env.reserve(env.remote, ReservationRequest{Device: "mock-9", StartAt: env.at(time.Hour)})
	wantErrCode(t, err, CodeDeviceNotFound)
	// A plain daemon has no reservations.
	env.s.cfg.Hub = false
	_, err = env.reserve(env.remote, ReservationRequest{Device: "mock-1", StartAt: env.at(time.Hour)})
	wantErrCode(t, err, CodeUsage)
	env.s.cfg.Hub = true
	// --reservation-lead sets the lead.
	env.s.cfg.ReservationLead = 2 * time.Hour
	_, err = env.reserve(env.remote, ReservationRequest{Device: "mock-1", StartAt: env.at(2*time.Hour + time.Second)})
	if de := wantErrCode(t, err, CodeUsage); de.Details["maxLeadSec"] != float64(7200) {
		t.Fatalf("a 2h lead: %+v", de.Details)
	}
	if ls, _ := env.remote.Leases(ctx); len(ls) != 6 {
		t.Fatalf("leases: %+v", ls)
	}
}

func TestReservations_NoOverlap(t *testing.T) {
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	base := env.mustReserve(env.remote, ReservationRequest{Device: "mock-1", StartAt: env.at(time.Hour), TTLSec: 3600, Holder: "alice"})
	if base.State != LeaseScheduled || base.StartsAt != env.at(time.Hour) || base.ExpiresAt != env.at(2*time.Hour) {
		t.Fatalf("reserved: %+v", base)
	}
	for _, w := range [][2]time.Duration{{90 * time.Minute, time.Hour}, {30 * time.Minute, time.Hour}, {30 * time.Minute, 3 * time.Hour}, {time.Hour, time.Hour}} {
		_, err := env.reserve(env.remote, ReservationRequest{Device: "mock-1", StartAt: env.at(w[0]), TTLSec: int(w[1] / time.Second), Holder: "bob"})
		de := wantErrCode(t, err, CodeDeviceInUse)
		if de.Details["holder"] != "alice" || de.Details["startsAt"] != base.StartsAt || de.Details["nextStartsAt"] != base.StartsAt {
			t.Fatalf("window %v: %+v", w, de.Details)
		}
	}
	// Touching windows on either side are free.
	env.mustReserve(env.remote, ReservationRequest{Device: "mock-1", StartAt: env.at(2 * time.Hour), TTLSec: 3600})
	env.mustReserve(env.remote, ReservationRequest{Device: "mock-1", StartAt: env.at(30 * time.Minute), TTLSec: 1800})
	// Against a started lease: up to its end, and from its end.
	env.lease(env.remote, LeaseRequest{Device: "mock-2", TTLSec: 7200, Holder: "carol"})
	_, err := env.reserve(env.remote, ReservationRequest{Device: "mock-2", StartAt: env.at(time.Hour)})
	if de := wantErrCode(t, err, CodeDeviceInUse); de.Details["holder"] != "carol" || de.Details["nextStartsAt"] != nil {
		t.Fatalf("over a started lease: %+v", de.Details)
	}
	env.mustReserve(env.remote, ReservationRequest{Device: "mock-2", StartAt: env.at(2 * time.Hour), TTLSec: 60})
	// A lease taken now never reaches into a reservation: the default
	// window is refused, a shorter one fits.
	r := env.mustReserve(env.remote, ReservationRequest{Device: "SIM-UP", StartAt: env.at(2 * time.Hour), TTLSec: 60})
	_, err = env.local.AcquireLease(ctx, LeaseRequest{Device: "SIM-UP"})
	if de := wantErrCode(t, err, CodeDeviceInUse); de.Details["nextStartsAt"] != r.StartsAt {
		t.Fatalf("default window: %+v", de.Details)
	}
	_, err = env.local.AcquireLease(ctx, LeaseRequest{Device: "SIM-UP", TTLSec: 7201})
	wantErrCode(t, err, CodeDeviceInUse)
	got := env.lease(env.local, LeaseRequest{Device: "SIM-UP", TTLSec: 3600}).Lease
	if got.State != LeaseActive || got.StartsAt != env.at(0) {
		t.Fatalf("a lease now: %+v", got)
	}
	// A renew stops at the next reservation too, and leaves the lease as it was.
	_, err = env.local.RenewLease(ctx, got.ID, 7201)
	if de := wantErrCode(t, err, CodeDeviceInUse); de.Details["nextStartsAt"] != r.StartsAt {
		t.Fatalf("renew into a reservation: %+v", de.Details)
	}
	if l, _ := env.local.Lease(ctx, got.ID); l.Lease.ExpiresAt != got.ExpiresAt {
		t.Fatalf("a refused renew moved the lease: %+v", l.Lease)
	}
	if l, err := env.local.RenewLease(ctx, got.ID, 7200); err != nil || l.Lease.ExpiresAt != r.StartsAt || l.Lease.StartsAt != got.StartsAt {
		t.Fatalf("renew up to the reservation: %+v %v", l, err)
	}
	// A reservation is not renewed.
	_, err = env.remote.RenewLease(ctx, r.ID, 60)
	if de := wantErrCode(t, err, CodeUsage); de.Details["startsAt"] != r.StartsAt {
		t.Fatalf("renew a reservation: %+v", de.Details)
	}
}

func TestReservations_StartUseExpireCancel(t *testing.T) {
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	l := env.mustReserve(env.remote, ReservationRequest{Device: "mock-1", StartAt: env.at(10 * time.Second), TTLSec: 60, Holder: "alice"})
	env.remote.SetLease("mock-1", l.ID)
	// Before the start: LEASE_NOT_STARTED with the start; no lease at all
	// stays LEASE_REQUIRED.
	_, err := env.remote.Command(ctx, "mock-1", "tapOn", "x", true)
	if de := wantErrCode(t, err, CodeLeaseNotStarted); de.Details["startsAt"] != l.StartsAt || de.Details["lease"] != l.ID {
		t.Fatalf("before the start: %+v", de.Details)
	}
	_, err = env.local.Command(ctx, "mock-1", "tapOn", "x", true)
	wantErrCode(t, err, CodeLeaseRequired)
	devs, _ := env.remote.Devices(ctx, "mock")
	if d := devs[0]; d.ID != "mock-1" || d.Lease != nil || !d.Available || len(d.Reservations) != 1 ||
		d.Reservations[0] != (DeviceLease{Holder: "alice", StartsAt: l.StartsAt, ExpiresAt: l.ExpiresAt}) {
		t.Fatalf("listing before the start: %+v", d)
	}
	if got, err := env.remote.Lease(ctx, l.ID); err != nil || got.Lease.State != LeaseScheduled {
		t.Fatalf("GET a reservation: %+v %v", got, err)
	}
	if ls, _ := env.remote.Leases(ctx); len(ls) != 1 || ls[0].State != LeaseScheduled {
		t.Fatalf("list: %+v", ls)
	}
	// At the start a request starts it, without waiting for the sweeper.
	env.clock.advance(10 * time.Second)
	if got, _ := env.remote.Lease(ctx, l.ID); got.Lease.State != LeaseActive {
		t.Fatalf("at the start: %+v", got.Lease)
	}
	if ev := env.leaseEvents(); !slices.Equal(ev, []string{"lease.scheduled:mock-1", "lease.granted:mock-1"}) {
		t.Fatalf("events: %v", ev)
	}
	if _, err := env.remote.Attach(ctx, "mock-1", AttachConfig{Platform: "mock"}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.remote.Command(ctx, "mock-1", "tapOn", "x", true); err != nil {
		t.Fatal(err)
	}
	devs, _ = env.remote.Devices(ctx, "mock")
	if d := devs[0]; d.Lease == nil || d.Lease.Holder != "alice" || len(d.Reservations) != 0 {
		t.Fatalf("listing after the start: %+v", d)
	}
	// It expires as any lease does.
	env.clock.advance(60 * time.Second)
	env.sweep()
	_, err = env.remote.Command(ctx, "mock-1", "tapOn", "x", true)
	wantErrCode(t, err, CodeLeaseExpired)
	// The sweeper starts one on its own.
	l2 := env.mustReserve(env.remote, ReservationRequest{Device: "mock-1", StartAt: env.at(5 * time.Second), TTLSec: 60})
	env.clock.advance(5 * time.Second)
	env.sweep()
	env.s.leases.mu.Lock()
	started := env.s.leases.byDevice["mock-1"] != nil && env.s.leases.byDevice["mock-1"].id == l2.ID
	env.s.leases.mu.Unlock()
	if !started {
		t.Fatal("the sweeper did not start the reservation")
	}
	// Cancelling a reservation detaches nothing.
	r := env.mustReserve(env.remote, ReservationRequest{Device: "mock-2", StartAt: env.at(time.Hour), TTLSec: 60})
	if _, err := env.remote.Attach(ctx, "mock-2", AttachConfig{Platform: "mock"}); err == nil {
		t.Fatal("attached a device reserved for later without a lease")
	}
	before := len(env.leaseEvents())
	if _, err := env.remote.ReleaseLease(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	if ev := env.leaseEvents(); len(ev) != before+1 || ev[len(ev)-1] != "lease.released:mock-2" {
		t.Fatalf("cancel events: %v", ev)
	}
	if _, err := env.remote.Lease(ctx, r.ID); err == nil {
		t.Fatal("a cancelled reservation is still listed")
	}
	env.s.leases.mu.Lock()
	ending := len(env.s.leases.ending)
	env.s.leases.mu.Unlock()
	if ending != 0 {
		t.Fatal("a cancel reserved the device for a teardown")
	}
	// A start whose whole window passed while nothing swept is dropped.
	gone := env.mustReserve(env.remote, ReservationRequest{Device: "mock-2", StartAt: env.at(time.Hour), TTLSec: 60})
	env.clock.advance(2 * time.Hour)
	env.sweep()
	if _, err := env.remote.Lease(ctx, gone.ID); err == nil {
		t.Fatal("a reservation whose window passed is still listed")
	}
}

func TestReservations_DeleteEndsStopKeeps(t *testing.T) {
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	if err := recordCreated(CreatedRecord{Platform: "ios", ID: "SIM-UP", Name: "Shared Phone"}); err != nil {
		t.Fatal(err)
	}
	env.s.registry.deps.DeleteSimulator = func(string) error { return nil }
	kept := env.mustReserve(env.local, ReservationRequest{Device: "SIM-UP", StartAt: env.at(time.Hour), TTLSec: 60})
	if err := env.local.StopDevice(ctx, "SIM-UP", true); err != nil {
		t.Fatal(err)
	}
	if _, err := env.local.Lease(ctx, kept.ID); err != nil {
		t.Fatalf("a stop ended a reservation: %v", err)
	}
	if _, err := env.local.DeleteDevice(ctx, "SIM-UP", false); err != nil {
		t.Fatal(err)
	}
	if _, err := env.local.Lease(ctx, kept.ID); err == nil {
		t.Fatal("a delete kept the device's reservation")
	}
}

func TestReservations_UnlistedAndUnshared(t *testing.T) {
	env := newHubEnv(t, Config{ShareDevices: []string{"mock-1", "PHONE-A"}})
	ctx := context.Background()
	env.mu.Lock()
	env.phones = []DeviceInfo{{ID: "PHONE-A", Platform: "android", Kind: "device", Name: "Lab Phone", Ready: true}}
	env.mu.Unlock()
	l := env.mustReserve(env.remote, ReservationRequest{Device: "PHONE-A", StartAt: env.at(time.Hour), TTLSec: 60})
	other := env.mustReserve(env.remote, ReservationRequest{Device: "mock-1", StartAt: env.at(time.Hour), TTLSec: 60})
	// Unplugged before its start: the holder still hears LEASE_NOT_STARTED.
	env.mu.Lock()
	env.phones = nil
	env.mu.Unlock()
	env.s.forgetShared(Event{Type: "device.deleted"})
	env.remote.SetLease("PHONE-A", l.ID)
	_, err := env.remote.Command(ctx, "PHONE-A", "tapOn", "x", true)
	wantErrCode(t, err, CodeLeaseNotStarted)
	// Its lease routes still find it: read, and cancel, by id.
	if got, err := env.remote.Lease(ctx, l.ID); err != nil || got.Lease.State != LeaseScheduled {
		t.Fatalf("GET an unlisted device's reservation: %+v %v", got, err)
	}
	if ls, _ := env.remote.Leases(ctx); !slices.ContainsFunc(ls, func(x Lease) bool { return x.ID == l.ID }) {
		t.Fatalf("list without the unlisted device's reservation: %+v", ls)
	}
	// Another device's reservation id gets the answers it got before.
	env.remote.SetLease("PHONE-A", other.ID)
	_, err = env.remote.Command(ctx, "PHONE-A", "tapOn", "x", true)
	if c := codeOf(t, err); c != CodeLeaseExpired && c != CodeDeviceNotFound {
		t.Fatalf("another device's reservation: %v", err)
	}
	if _, err := env.remote.ReleaseLease(ctx, l.ID); err != nil {
		t.Fatalf("cancel an unlisted device's reservation: %v", err)
	}
	if _, err := env.local.Lease(ctx, l.ID); err == nil {
		t.Fatal("the reservation survived its cancel")
	}
	// A local reservation on an unshared device stays hidden from a remote
	// caller once the device is unplugged too.
	env.mu.Lock()
	env.phones = []DeviceInfo{{ID: "PHONE-PRIVATE", Platform: "android", Kind: "device", Name: "Private Phone", Ready: true}}
	env.mu.Unlock()
	private := env.mustReserve(env.local, ReservationRequest{Device: "PHONE-PRIVATE", StartAt: env.at(time.Hour), TTLSec: 60})
	env.mu.Lock()
	env.phones = nil
	env.mu.Unlock()
	if _, err := env.remote.Lease(ctx, private.ID); !isCode(err, CodeLeaseExpired) {
		t.Fatalf("remote GET of an unplugged unshared device's reservation: %v", err)
	}
	if _, err := env.remote.ReleaseLease(ctx, private.ID); !isCode(err, CodeLeaseExpired) {
		t.Fatalf("remote cancel of an unplugged unshared device's reservation: %v", err)
	}
	if ls, _ := env.remote.Leases(ctx); slices.ContainsFunc(ls, func(x Lease) bool { return x.ID == private.ID }) {
		t.Fatalf("remote list shows it: %+v", ls)
	}
	env.remote.SetLease("PHONE-PRIVATE", private.ID)
	if _, err := env.remote.Command(ctx, "PHONE-PRIVATE", "tapOn", "x", true); isCode(err, CodeLeaseNotStarted) {
		t.Fatal("a remote caller learns of an unshared device's reservation")
	}
	if _, err := env.local.Lease(ctx, private.ID); err != nil {
		t.Fatalf("the local holder lost it: %v", err)
	}
	// A remote caller never sees an unshared device's reservation.
	hidden := env.mustReserve(env.local, ReservationRequest{Device: "mock-2", StartAt: env.at(time.Hour), TTLSec: 60})
	ls, _ := env.remote.Leases(ctx)
	for _, x := range ls {
		if x.ID == hidden.ID {
			t.Fatalf("a remote caller sees mock-2's reservation: %+v", ls)
		}
	}
	_, err = env.remote.Lease(ctx, hidden.ID)
	wantErrCode(t, err, CodeLeaseExpired)
	_, err = env.reserve(env.remote, ReservationRequest{Device: "mock-2", StartAt: env.at(2 * time.Hour)})
	wantErrCode(t, err, CodeDeviceNotFound)
}

func TestReservations_Selector(t *testing.T) {
	env := newHubEnv(t, Config{})
	first := env.mustReserve(env.remote, ReservationRequest{Select: &LeaseSelector{Platform: "mock"}, StartAt: env.at(time.Hour), TTLSec: 60})
	second := env.mustReserve(env.remote, ReservationRequest{Select: &LeaseSelector{Platform: "mock"}, StartAt: env.at(time.Hour), TTLSec: 60})
	if first.Device != "mock-1" || second.Device != "mock-2" {
		t.Fatalf("selector order: %s, %s", first.Device, second.Device)
	}
	_, err := env.reserve(env.remote, ReservationRequest{Select: &LeaseSelector{Platform: "mock"}, StartAt: env.at(time.Hour), TTLSec: 60})
	if de := wantErrCode(t, err, CodeDeviceInUse); len(de.Details["candidates"].([]any)) != 2 {
		t.Fatalf("all taken: %+v", de.Details)
	}
	// A device leased now whose lease ends before the start qualifies; a
	// shut-down simulator never does.
	env.lease(env.local, LeaseRequest{Device: "SIM-UP", TTLSec: 1800})
	sim := env.mustReserve(env.remote, ReservationRequest{Select: &LeaseSelector{Platform: "ios", Kind: "simulator"}, StartAt: env.at(time.Hour), TTLSec: 60})
	if sim.Device != "SIM-UP" {
		t.Fatalf("simulator: %+v", sim)
	}
	_, err = env.reserve(env.remote, ReservationRequest{Select: &LeaseSelector{Platform: "ios", Kind: "simulator"}, StartAt: env.at(time.Hour), TTLSec: 60})
	wantErrCode(t, err, CodeDeviceInUse)
	// A window over every candidate's maximum is USAGE naming it.
	_, err = env.reserve(env.remote, ReservationRequest{Select: &LeaseSelector{Platform: "mock"}, StartAt: env.at(3 * time.Hour), TTLSec: 28801})
	wantErrCode(t, err, CodeUsage)
	_, err = env.reserve(env.remote, ReservationRequest{Select: &LeaseSelector{Platform: "nothing"}, StartAt: env.at(time.Hour)})
	wantErrCode(t, err, CodeDeviceNotFound)
	_, err = env.reserve(env.remote, ReservationRequest{Select: &LeaseSelector{Platform: "mock", Hub: "elsewhere"}, StartAt: env.at(time.Hour)})
	wantErrCode(t, err, CodeDeviceNotFound)
	// A running emulator is never picked.
	env.mu.Lock()
	env.emuAVD = "Pixel_9"
	env.mu.Unlock()
	_, err = env.reserve(env.local, ReservationRequest{Select: &LeaseSelector{Platform: "android"}, StartAt: env.at(time.Hour)})
	wantErrCode(t, err, CodeDeviceInUse)
	// A start of now leases at once, as POST /v1/leases.
	now := env.mustReserve(env.remote, ReservationRequest{Select: &LeaseSelector{Platform: "mock", Name: "Mock Two"}, StartAt: env.at(0), TTLSec: 60})
	if now.State != LeaseActive || now.Device != "mock-2" {
		t.Fatalf("a selector starting now: %+v", now)
	}
}

func TestReservations_BootPassSkipsAReservedSimulator(t *testing.T) {
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	env.mustReserve(env.local, ReservationRequest{Device: "SIM-DOWN", StartAt: env.at(time.Hour), TTLSec: 60})
	_, err := env.local.AcquireLease(ctx, LeaseRequest{Select: &LeaseSelector{Platform: "ios", Name: "Spare Phone", Boot: true}})
	wantErrCode(t, err, CodeDeviceInUse)
	env.mu.Lock()
	booted := len(env.booted)
	env.mu.Unlock()
	if booted != 0 {
		t.Fatal("booted a simulator reserved inside the window")
	}
	// A window that ends by the reservation's start boots it.
	res := env.lease(env.local, LeaseRequest{Select: &LeaseSelector{Platform: "ios", Name: "Spare Phone", Boot: true}, TTLSec: 3600})
	if !res.Lease.Booted {
		t.Fatalf("boot lease: %+v", res.Lease)
	}
}

// Back to back: the lease before a reservation ends at its start, and
// the reservation's requests wait until that lease's teardown is done.
// The simulator the first lease booted stays booted for the second, which
// shuts it down when it ends.
func TestReservations_BackToBackTeardownAndBootHandOff(t *testing.T) {
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	a := env.lease(env.local, LeaseRequest{Select: &LeaseSelector{Platform: "ios", Name: "Spare Phone", Boot: true}, TTLSec: 60, Holder: "alice"})
	if !a.Lease.Booted || a.Lease.Device != "SIM-DOWN" {
		t.Fatalf("lease A: %+v", a.Lease)
	}
	b := env.mustReserve(env.remote, ReservationRequest{Device: "SIM-DOWN", StartAt: a.Lease.ExpiresAt, TTLSec: 60, Holder: "bob"})
	// A step under A that outlives A's window holds its teardown.
	release := make(chan struct{})
	var once sync.Once
	entered := make(chan struct{})
	env.mu.Lock()
	env.onStep = func() {
		once.Do(func() { close(entered) })
		<-release
	}
	env.mu.Unlock()
	if _, err := env.local.Attach(ctx, "SIM-DOWN", AttachConfig{Platform: "mock"}); err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = env.local.Command(context.Background(), "SIM-DOWN", "tapOn", "x", true) }()
	<-entered
	env.mu.Lock()
	env.onStep = nil
	env.mu.Unlock()
	env.clock.advance(60 * time.Second)
	env.s.sweepLeases()
	if got, _ := env.remote.Lease(ctx, b.ID); got.Lease.State != LeaseActive {
		t.Fatalf("B did not start: %+v", got.Lease)
	}
	env.remote.SetLease("SIM-DOWN", b.ID)
	_, err := env.remote.Attach(ctx, "SIM-DOWN", AttachConfig{Platform: "mock"})
	if de := wantErrCode(t, err, CodeDeviceInUse); !strings.Contains(de.Message, "being released") {
		t.Fatalf("attach beside A's teardown: %v", de)
	}
	_, err = env.remote.Command(ctx, "SIM-DOWN", "tapOn", "x", true)
	wantErrCode(t, err, CodeDeviceInUse)
	close(release)
	waitFor(t, func() bool { return !env.s.leases.tearingDown("SIM-DOWN") })
	if _, err := env.remote.Attach(ctx, "SIM-DOWN", AttachConfig{Platform: "mock"}); err != nil {
		t.Fatalf("attach after A's teardown: %v", err)
	}
	if _, err := env.remote.Command(ctx, "SIM-DOWN", "tapOn", "x", true); err != nil {
		t.Fatal(err)
	}
	env.mu.Lock()
	stopped := slices.Contains(env.stopped, "SIM-DOWN")
	env.mu.Unlock()
	if stopped {
		t.Fatal("A's end shut down the simulator B holds")
	}
	// The stream sees A end before B starts on the same device.
	waitFor(t, func() bool {
		return env.lastEventIndex("lease.granted", "SIM-DOWN") > env.lastEventIndex("lease.expired", "SIM-DOWN") &&
			env.lastEventIndex("lease.expired", "SIM-DOWN") >= 0
	})
	if got, _ := env.remote.Lease(ctx, b.ID); !got.Lease.Booted {
		t.Fatalf("B did not take over the shutdown: %+v", got.Lease)
	}
	env.clock.advance(60 * time.Second)
	env.sweep()
	env.mu.Lock()
	stopped = slices.Contains(env.stopped, "SIM-DOWN")
	env.mu.Unlock()
	if !stopped {
		t.Fatal("B's end did not shut the simulator down")
	}
}

// A reservation that took over a shutdown and is cancelled before it
// starts shuts the device down, there being no lease after it.
func TestReservations_CancelledHolderOfABootShutsItDown(t *testing.T) {
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	a := env.lease(env.local, LeaseRequest{Select: &LeaseSelector{Platform: "ios", Name: "Spare Phone", Boot: true}, TTLSec: 60})
	start, _ := time.Parse(time.RFC3339, a.Lease.ExpiresAt)
	b := env.mustReserve(env.remote, ReservationRequest{Device: "SIM-DOWN", StartAt: start.Add(time.Minute).UTC().Format(time.RFC3339), TTLSec: 60})
	env.clock.advance(60 * time.Second)
	env.sweep()
	env.mu.Lock()
	stopped := slices.Contains(env.stopped, "SIM-DOWN")
	env.mu.Unlock()
	if stopped {
		t.Fatal("A's end shut down the simulator B is reserved on")
	}
	if _, err := env.remote.ReleaseLease(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		env.mu.Lock()
		defer env.mu.Unlock()
		return slices.Contains(env.stopped, "SIM-DOWN")
	})
}

func TestReservations_ClientReserveNoRoute(t *testing.T) {
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/reservations" {
			var req ReservationRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if _, err := time.Parse(time.RFC3339, req.ClientTime); err != nil {
				writeError(w, Errorf(CodeInternal, "no clientTime"))
				return
			}
		}
		writeError(w, Errorf(CodeUsage, "%s", NoRouteMessage(r.Method, r.URL.Path)))
	}))
	defer hs.Close()
	_, err := NewHTTPClient(hs.URL, "t", nil).Reserve(context.Background(), ReservationRequest{Device: "d", StartAt: "2026-11-23T00:00:00Z"})
	de := wantErrCode(t, err, CodeUsage)
	if de.Details["reason"] != "reservations-unsupported" || de.Details["hub"] != hs.URL || !strings.Contains(de.Message, "does not take reservations") {
		t.Fatalf("no route: %+v", de)
	}
}

// Lease times are whole seconds, as answers report them: a reservation
// from a lease's reported expiresAt only touches it, and is granted.
func TestReservations_TouchingTheReportedEnd(t *testing.T) {
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	env.clock.advance(400 * time.Millisecond)
	l := env.lease(env.remote, LeaseRequest{Device: "mock-1", TTLSec: 60}).Lease
	if l.StartsAt != env.clock.now().Truncate(time.Second).UTC().Format(time.RFC3339) {
		t.Fatalf("startsAt: %+v", l)
	}
	if _, err := env.reserve(env.remote, ReservationRequest{Device: "mock-1", StartAt: l.ExpiresAt, TTLSec: 60}); err != nil {
		t.Fatalf("a reservation from the reported expiresAt: %v", err)
	}
	// A renew lands on whole seconds too, and may end exactly at the next start.
	env.clock.advance(1700 * time.Millisecond)
	if _, err := env.remote.RenewLease(ctx, l.ID, 60); !isCode(err, CodeDeviceInUse) {
		t.Fatalf("renew past the reservation: %v", err)
	}
	r, err := env.remote.RenewLease(ctx, l.ID, 58)
	if err != nil {
		t.Fatal(err)
	}
	if want := env.clock.now().Truncate(time.Second).Add(58 * time.Second).UTC().Format(time.RFC3339); r.Lease.ExpiresAt != want || r.Lease.ExpiresAt != l.ExpiresAt {
		t.Fatalf("renewed expiresAt %s, want %s (the reservation's start %s)", r.Lease.ExpiresAt, want, l.ExpiresAt)
	}
	// expiresAt is startsAt plus ttlSec, within the device's maximum.
	s0, _ := time.Parse(time.RFC3339, l.StartsAt)
	e0, _ := time.Parse(time.RFC3339, l.ExpiresAt)
	if e0.Sub(s0) != 60*time.Second {
		t.Fatalf("a 60 s lease spans %s..%s", l.StartsAt, l.ExpiresAt)
	}
	// A lease taken now with ttlSec = nextStartsAt - the current second
	// ends at that start, as DEVICE_IN_USE's advice says, and is granted.
	next := env.mustReserve(env.remote, ReservationRequest{Device: "SIM-UP", StartAt: env.clock.now().Add(2 * time.Minute).Truncate(time.Second).UTC().Format(time.RFC3339), TTLSec: 60})
	ns, _ := time.Parse(time.RFC3339, next.StartsAt)
	fit := int(ns.Sub(env.clock.now().Truncate(time.Second)) / time.Second)
	if _, err := env.local.AcquireLease(ctx, LeaseRequest{Device: "SIM-UP", TTLSec: fit}); err != nil {
		t.Fatalf("ttlSec %d up to the reservation: %v", fit, err)
	}
	// A fractional startAt starts at its whole second, and a reservation
	// from its reported expiresAt touches it.
	f := env.mustReserve(env.remote, ReservationRequest{Device: "mock-2", StartAt: env.clock.now().Add(time.Hour + 600*time.Millisecond).UTC().Format(time.RFC3339Nano), TTLSec: 60})
	if want := env.clock.now().Add(time.Hour + 600*time.Millisecond).Truncate(time.Second).UTC().Format(time.RFC3339); f.StartsAt != want {
		t.Fatalf("fractional startAt: %+v, want start %s", f, want)
	}
	if _, err := env.reserve(env.remote, ReservationRequest{Device: "mock-2", StartAt: f.ExpiresAt, TTLSec: 60}); err != nil {
		t.Fatalf("from a fractional reservation's reported end: %v", err)
	}
}

// A reservation that starts in a later sweep than the lease before it
// ended, while that lease's teardown still runs, is announced after it.
func TestReservations_GrantAnnouncedAfterAnEarlierSweepsTeardown(t *testing.T) {
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	a := env.lease(env.local, LeaseRequest{Device: "mock-1", TTLSec: 60}).Lease
	end, _ := time.Parse(time.RFC3339, a.ExpiresAt)
	env.mustReserve(env.remote, ReservationRequest{Device: "mock-1", StartAt: end.Add(time.Second).UTC().Format(time.RFC3339), TTLSec: 60})
	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	env.mu.Lock()
	env.onStep = func() {
		once.Do(func() { close(entered) })
		<-release
	}
	env.mu.Unlock()
	if _, err := env.local.Attach(ctx, "mock-1", AttachConfig{Platform: "mock"}); err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = env.local.Command(context.Background(), "mock-1", "tapOn", "x", true) }()
	<-entered
	env.clock.advance(60 * time.Second)
	env.s.sweepLeases() // A ends; its teardown waits for the step
	env.clock.advance(time.Second)
	env.s.sweepLeases() // B starts while A's teardown still runs
	if env.lastEventIndex("lease.granted", "mock-1") > 0 && env.lastEventIndex("lease.expired", "mock-1") < 0 {
		t.Fatal("B was announced before A's end")
	}
	close(release)
	waitFor(t, func() bool {
		g, x := env.lastEventIndex("lease.granted", "mock-1"), env.lastEventIndex("lease.expired", "mock-1")
		return x >= 0 && g > x
	})
}

// lastEventIndex is the index of the last event of typ on device, or -1.
func (e *hubEnv) lastEventIndex(typ, device string) int {
	last := -1
	for i, ev := range e.s.events.Recent("", 0) {
		if ev.Type == typ && ev.Device == device {
			last = i
		}
	}
	return last
}

func isCode(err error, code Code) bool {
	de, ok := err.(*Error)
	return ok && de.Code == code
}

// A reservation far after a booting lease's end does not keep the
// simulator booted for it; one within the grace does, and if it is then
// missed (the hub stalled through its window) the device is shut down.
func TestReservations_BootHandOffOnlyToANearSuccessor(t *testing.T) {
	stopped := func(env *hubEnv) bool {
		env.mu.Lock()
		defer env.mu.Unlock()
		return slices.Contains(env.stopped, "SIM-DOWN")
	}
	env := newHubEnv(t, Config{})
	a := env.lease(env.local, LeaseRequest{Select: &LeaseSelector{Platform: "ios", Name: "Spare Phone", Boot: true}, TTLSec: 60}).Lease
	end, _ := time.Parse(time.RFC3339, a.ExpiresAt)
	far := env.mustReserve(env.remote, ReservationRequest{Device: "SIM-DOWN", StartAt: end.Add(2 * time.Hour).UTC().Format(time.RFC3339), TTLSec: 60})
	env.clock.advance(60 * time.Second)
	env.sweep()
	if !stopped(env) {
		t.Fatal("A's end kept the simulator booted for a reservation two hours away")
	}
	if l, _ := env.remote.Lease(context.Background(), far.ID); l.Lease.Booted {
		t.Fatalf("the far reservation took the shutdown: %+v", l.Lease)
	}

	env = newHubEnv(t, Config{})
	a = env.lease(env.local, LeaseRequest{Select: &LeaseSelector{Platform: "ios", Name: "Spare Phone", Boot: true}, TTLSec: 60}).Lease
	end, _ = time.Parse(time.RFC3339, a.ExpiresAt)
	near := env.mustReserve(env.remote, ReservationRequest{Device: "SIM-DOWN", StartAt: end.Add(30 * time.Second).UTC().Format(time.RFC3339), TTLSec: 60})
	env.clock.advance(60 * time.Second)
	env.sweep()
	if stopped(env) {
		t.Fatal("A's end shut down the simulator a reservation 30 s away holds")
	}
	if l, _ := env.remote.Lease(context.Background(), near.ID); !l.Lease.Booted {
		t.Fatalf("the near reservation did not take the shutdown: %+v", l.Lease)
	}
	env.clock.advance(5 * time.Minute) // its whole window passes unswept
	env.sweep()
	waitFor(t, func() bool { return stopped(env) })

	// Missed while the reservation after it starts in the same sweep: the
	// duty passes to that one, and the device stays booted until it ends.
	env = newHubEnv(t, Config{})
	a = env.lease(env.local, LeaseRequest{Select: &LeaseSelector{Platform: "ios", Name: "Spare Phone", Boot: true}, TTLSec: 60}).Lease
	end, _ = time.Parse(time.RFC3339, a.ExpiresAt)
	r1 := env.mustReserve(env.remote, ReservationRequest{Device: "SIM-DOWN", StartAt: end.Add(30 * time.Second).UTC().Format(time.RFC3339), TTLSec: 60})
	r1End := end.Add(90 * time.Second)
	r2 := env.mustReserve(env.remote, ReservationRequest{Device: "SIM-DOWN", StartAt: r1End.UTC().Format(time.RFC3339), TTLSec: 60})
	env.clock.advance(60 * time.Second)
	env.sweep() // A ends; r1 takes the shutdown
	env.clock.advance(100 * time.Second)
	env.sweep() // r1 missed, r2 starts
	if l, _ := env.remote.Lease(context.Background(), r2.ID); l.Lease.State != LeaseActive || !l.Lease.Booted {
		t.Fatalf("r2 after r1 was missed: %+v (r1 %s)", l.Lease, r1.ID)
	}
	if stopped(env) {
		t.Fatal("r1's miss shut down the simulator r2 holds")
	}
	env.clock.advance(60 * time.Second)
	env.sweep()
	waitFor(t, func() bool { return stopped(env) })
}

// A hub that stops hands no boot shutdown to a reservation: it shuts down
// what it booted, and the reservation keeps no duty.
func TestReservations_HubStopHandsNoBootOn(t *testing.T) {
	env := newHubEnv(t, Config{})
	a := env.lease(env.local, LeaseRequest{Select: &LeaseSelector{Platform: "ios", Name: "Spare Phone", Boot: true}, TTLSec: 60}).Lease
	end, _ := time.Parse(time.RFC3339, a.ExpiresAt)
	r := env.mustReserve(env.remote, ReservationRequest{Device: "SIM-DOWN", StartAt: end.Add(10 * time.Second).UTC().Format(time.RFC3339), TTLSec: 60})
	env.clock.advance(55 * time.Second) // r is within the grace of now
	env.s.endAllLeases()
	if l, err := env.local.Lease(context.Background(), r.ID); err != nil || l.Lease.Booted {
		t.Fatalf("the reservation took a duty at hub stop: %+v %v", l, err)
	}
	env.mu.Lock()
	defer env.mu.Unlock()
	if !slices.Contains(env.stopped, "SIM-DOWN") {
		t.Fatal("hub stop left the simulator it booted running")
	}
}

// A selector lease whose matches are all free now but reserved inside the
// window says so, with the earliest nextStartsAt, and a window that ends
// by it is granted.
func TestReservations_SelectorLeaseNamesTheReservation(t *testing.T) {
	env := newHubEnv(t, Config{})
	first := env.mustReserve(env.remote, ReservationRequest{Device: "mock-1", StartAt: env.at(2 * time.Hour), TTLSec: 60})
	env.mustReserve(env.remote, ReservationRequest{Device: "mock-2", StartAt: env.at(3 * time.Hour), TTLSec: 60})
	_, err := env.local.AcquireLease(context.Background(), LeaseRequest{Select: &LeaseSelector{Platform: "mock"}})
	de := wantErrCode(t, err, CodeDeviceInUse)
	if de.Details["nextStartsAt"] != first.StartsAt || !strings.Contains(de.Message, "reserved within the window") {
		t.Fatalf("all reserved within the window: %+v", de)
	}
	if l := env.lease(env.local, LeaseRequest{Select: &LeaseSelector{Platform: "mock"}, TTLSec: 3600}).Lease; l.Device != "mock-1" {
		t.Fatalf("a window that ends by the reservation: %+v", l)
	}
}

// Two teardowns can overlap on one device: a reservation released while
// the lease before it is still torn down. The device stays reserved until
// both are done.
func TestReservations_OverlappingTeardownsKeepTheDeviceReserved(t *testing.T) {
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	a := env.lease(env.local, LeaseRequest{Device: "mock-1", TTLSec: 60}).Lease
	b := env.mustReserve(env.remote, ReservationRequest{Device: "mock-1", StartAt: a.ExpiresAt, TTLSec: 60})
	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	env.mu.Lock()
	env.onStep = func() {
		once.Do(func() { close(entered) })
		<-release
	}
	env.mu.Unlock()
	if _, err := env.local.Attach(ctx, "mock-1", AttachConfig{Platform: "mock"}); err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = env.local.Command(context.Background(), "mock-1", "tapOn", "x", true) }()
	<-entered
	env.mu.Lock()
	env.onStep = nil
	env.mu.Unlock()
	env.clock.advance(60 * time.Second)
	env.s.sweepLeases() // A ends, its teardown held by the step; B starts
	if _, err := env.remote.ReleaseLease(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	// B's teardown is done; A's is not: the device is still reserved.
	_, err := env.local.AcquireLease(ctx, LeaseRequest{Device: "mock-1", TTLSec: 60})
	if de := wantErrCode(t, err, CodeDeviceInUse); !strings.Contains(de.Message, "being released") {
		t.Fatalf("a lease beside A's teardown: %v", de)
	}
	close(release)
	waitFor(t, func() bool { return !env.s.leases.tearingDown("mock-1") })
	env.lease(env.local, LeaseRequest{Device: "mock-1", TTLSec: 60})
}

// With --boot, a shut-down simulator reserved inside the window is named
// in the answer too.
func TestReservations_BootSelectorNamesTheReservation(t *testing.T) {
	env := newHubEnv(t, Config{})
	r := env.mustReserve(env.local, ReservationRequest{Device: "SIM-DOWN", StartAt: env.at(2 * time.Hour), TTLSec: 60})
	_, err := env.local.AcquireLease(context.Background(), LeaseRequest{Select: &LeaseSelector{Platform: "ios", Name: "Spare Phone", Boot: true}})
	if de := wantErrCode(t, err, CodeDeviceInUse); de.Details["nextStartsAt"] != r.StartsAt {
		t.Fatalf("boot pass: %+v", de)
	}
}

// A client whose clock is off the hub's sends a device's other lease when
// the one it chose is refused as not started or ended (OtherLease).
func TestReservations_ClientSendsTheOtherLease(t *testing.T) {
	env := newHubEnv(t, Config{})
	ctx := context.Background()
	l := env.lease(env.remote, LeaseRequest{Device: "mock-1", TTLSec: 60}).Lease
	r := env.mustReserve(env.remote, ReservationRequest{Device: "mock-1", StartAt: l.ExpiresAt, TTLSec: 60})
	c := NewHTTPClient(env.remote.BaseURL(), "t", nil)
	other := map[string]string{l.ID: r.ID, r.ID: l.ID}
	c.OtherLease = func(device, failed string) string { return other[failed] }
	// Ahead: it sends the reservation while the lease still runs.
	c.SetLease("mock-1", r.ID)
	if _, err := c.Attach(ctx, "mock-1", AttachConfig{Platform: "mock"}); err != nil {
		t.Fatalf("ahead: %v", err)
	}
	if c.LeaseOf("mock-1") != l.ID {
		t.Fatalf("the client kept sending %s", c.LeaseOf("mock-1"))
	}
	// Behind: it sends the ended lease once the reservation has started.
	env.clock.advance(60 * time.Second)
	env.sweep()
	c.SetLease("mock-1", l.ID)
	if _, err := c.Command(ctx, "mock-1", "tapOn", "x", true); err != nil && !isCode(err, CodeDeviceNotAttached) {
		t.Fatalf("behind: %v", err)
	}
	if c.LeaseOf("mock-1") != r.ID {
		t.Fatalf("the client kept sending %s", c.LeaseOf("mock-1"))
	}
	// With no other lease, the refusal is the answer.
	c.OtherLease = func(string, string) string { return "" }
	c.SetLease("mock-1", l.ID)
	if _, err := c.Command(ctx, "mock-1", "tapOn", "x", true); !isCode(err, CodeLeaseExpired) {
		t.Fatalf("no other lease: %v", err)
	}
}
