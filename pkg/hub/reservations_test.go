package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

func rfc(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// noRoute makes h's daemon answer POST /v1/reservations as a daemon that
// predates it does.
func noRoute(h *testHub) {
	h.setHook(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/reservations" {
			return false
		}
		e := daemon.Errorf(daemon.CodeUsage, "%s", daemon.NoRouteMessage(r.Method, r.URL.Path))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(daemon.FailureEnvelope(e))
		return true
	})
}

func TestMesh_ReservationsByIDAndSelector(t *testing.T) {
	a, b := twoHubs(t)
	c := b.client()
	ctx := context.Background()
	start := time.Now().Add(2 * time.Second)
	// By id, through the entry hub, on the other hub's device.
	res, err := c.Reserve(ctx, daemon.ReservationRequest{Device: "a1@hub-a", StartAt: rfc(start), TTLSec: 60, Holder: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	l := res.Lease
	if l.Device != "a1@hub-a" || !strings.HasSuffix(l.ID, "@hub-a") || l.State != daemon.LeaseScheduled {
		t.Fatalf("by id: %+v", l)
	}
	// An unqualified id resolves as on the device routes.
	if r2, err := c.Reserve(ctx, daemon.ReservationRequest{Device: "a2", StartAt: rfc(start), TTLSec: 60}); err != nil || r2.Lease.Device != "a2@hub-a" {
		t.Fatalf("unqualified: %+v %v", r2, err)
	}
	// A selector passes over a hub whose matches are reserved for the window.
	if _, err := c.Reserve(ctx, daemon.ReservationRequest{Select: &daemon.LeaseSelector{Platform: "mock", Hub: "hub-a"}, StartAt: rfc(start), TTLSec: 60}); !isCode(err, daemon.CodeDeviceInUse) {
		t.Fatalf("hub-a reserved: %v", err)
	}
	var got []string
	for i := 0; i < 2; i++ {
		r, err := c.Reserve(ctx, daemon.ReservationRequest{Select: &daemon.LeaseSelector{Platform: "mock"}, StartAt: rfc(start), TTLSec: 60})
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, r.Lease.Device)
	}
	if strings.Join(got, ",") != "b1@hub-b,b2@hub-b" {
		t.Fatalf("selector order: %v", got)
	}
	if _, err := c.Reserve(ctx, daemon.ReservationRequest{Select: &daemon.LeaseSelector{Platform: "mock"}, StartAt: rfc(start), TTLSec: 60}); !isCode(err, daemon.CodeDeviceInUse) {
		t.Fatalf("all reserved: %v", err)
	}
	// The merged list carries both hubs' reservations.
	ls, err := c.Leases(ctx)
	if err != nil || len(ls) != 4 {
		t.Fatalf("leases: %+v %v", ls, err)
	}
	for _, x := range ls {
		if x.State != daemon.LeaseScheduled {
			t.Fatalf("state: %+v", x)
		}
	}
	// The owner's clock judges clientTime and startAt; its USAGE is the answer.
	_, err = c.Reserve(ctx, daemon.ReservationRequest{Device: "a1@hub-a", StartAt: rfc(start.Add(time.Hour)), ClientTime: rfc(time.Now().Add(5 * time.Minute))})
	if !isCode(err, daemon.CodeUsage) || !strings.Contains(err.Error(), "clock") {
		t.Fatalf("skew through the mesh: %v", err)
	}
	_, err = c.Reserve(ctx, daemon.ReservationRequest{Select: &daemon.LeaseSelector{Platform: "mock", Hub: "hub-a"}, StartAt: rfc(start.Add(48 * time.Hour))})
	if !isCode(err, daemon.CodeUsage) {
		t.Fatalf("over the lead through the mesh: %v", err)
	}
	// A misspelt key inside select reaches the owner and is refused there;
	// select.location is refused naming the field, never routed to a
	// cloud hub.
	for body, field := range map[string]string{
		`{"select":{"platfrom":"mock"},"startAt":"` + rfc(start.Add(time.Hour)) + `","clientTime":"` + rfc(time.Now()) + `"}`:                   "platfrom",
		`{"select":{"platform":"ios","location":"cloud"},"startAt":"` + rfc(start.Add(time.Hour)) + `","clientTime":"` + rfc(time.Now()) + `"}`: "location",
	} {
		status, data := b.do(t, http.MethodPost, "/v1/reservations", body)
		var env daemon.Envelope
		_ = json.Unmarshal(data, &env)
		if status != http.StatusBadRequest || env.Error == nil || env.Error.Code != daemon.CodeUsage || env.Error.Details["field"] != field {
			t.Fatalf("%s: %d %s", field, status, data)
		}
	}
	// A retried request id reserves once.
	body := `{"device":"b1@hub-b","startAt":"` + rfc(start.Add(time.Hour)) + `","ttlSec":60,"clientTime":"` + rfc(time.Now()) + `"}`
	s1, d1 := b.do(t, http.MethodPost, "/v1/reservations", body, daemon.HeaderRequestID, "rid-1")
	s2, d2 := b.do(t, http.MethodPost, "/v1/reservations", body, daemon.HeaderRequestID, "rid-1")
	if s1 != http.StatusOK || s2 != http.StatusOK || string(d1) != string(d2) {
		t.Fatalf("retry: %d %s / %d %s", s1, d1, s2, d2)
	}
	// After the start the reservation is a lease the entry hub routes.
	time.Sleep(time.Until(start) + 1500*time.Millisecond)
	got1, err := c.Lease(ctx, l.ID)
	if err != nil || got1.Lease.State != daemon.LeaseActive {
		t.Fatalf("after the start: %+v %v", got1, err)
	}
	c.SetLease("a1@hub-a", l.ID)
	if _, err := c.Attach(ctx, "a1@hub-a", daemon.AttachConfig{Platform: "mock"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Command(ctx, "a1@hub-a", "tapOn", "x", true); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReleaseLease(ctx, l.ID); err != nil {
		t.Fatal(err)
	}
	_ = a
}

func TestMesh_ReservationsOnAHubThatPredatesThem(t *testing.T) {
	a, b := twoHubs(t)
	c := b.client()
	ctx := context.Background()
	noRoute(a)
	start := rfc(time.Now().Add(time.Hour))
	// By id: the owner is named.
	_, err := c.Reserve(ctx, daemon.ReservationRequest{Device: "a1@hub-a", StartAt: start, TTLSec: 60})
	var de *daemon.Error
	if !isCode(err, daemon.CodeUsage) || !strings.Contains(err.Error(), "hub hub-a does not take reservations") {
		t.Fatalf("by id: %v", err)
	}
	de = err.(*daemon.Error)
	if de.Details["reason"] != "reservations-unsupported" || de.Details["hub"] != "hub-a" {
		t.Fatalf("details: %+v", de.Details)
	}
	// A selector passes over the older hub: with hub-b's devices taken, the
	// answer names hub-a.
	for i := 0; i < 2; i++ {
		if _, err := c.Reserve(ctx, daemon.ReservationRequest{Select: &daemon.LeaseSelector{Platform: "mock"}, StartAt: start, TTLSec: 60}); err != nil {
			t.Fatal(err)
		}
	}
	_, err = c.Reserve(ctx, daemon.ReservationRequest{Select: &daemon.LeaseSelector{Platform: "mock"}, StartAt: start, TTLSec: 60})
	if !isCode(err, daemon.CodeDeviceInUse) {
		t.Fatalf("hub-b's in-use wins over hub-a's no route: %v", err)
	}
	_, err = c.Reserve(ctx, daemon.ReservationRequest{Select: &daemon.LeaseSelector{Platform: "mock", Hub: "hub-a"}, StartAt: start, TTLSec: 60})
	if !isCode(err, daemon.CodeUsage) {
		t.Fatalf("only hub-a: %v", err)
	}
	de = err.(*daemon.Error)
	if de.Details["reason"] != "reservations-unsupported" {
		t.Fatalf("only hub-a: %+v", de)
	}
	// A newer peer later in the order still grants: an older entry's own
	// devices are passed over.
	noRoute(b)
	a.setHook(func(http.ResponseWriter, *http.Request) bool { return false })
	r, err := c.Reserve(ctx, daemon.ReservationRequest{Select: &daemon.LeaseSelector{Platform: "mock"}, StartAt: start, TTLSec: 60})
	if err != nil || !strings.HasSuffix(r.Lease.Device, "@hub-a") {
		t.Fatalf("passed over to hub-a: %+v %v", r, err)
	}
}

// A start past one hub's lead is that hub's limit: a selector goes on to a
// peer whose lead takes it, and answers the USAGE only when none does.
func TestMesh_ReservationSelectorPassesAHubsLead(t *testing.T) {
	_, b := twoHubs(t, func(m *Config, d *daemon.Config) { m.ReservationLead, d.ReservationLead = 2*time.Hour, 2*time.Hour })
	c := b.client()
	ctx := context.Background()
	r, err := c.Reserve(ctx, daemon.ReservationRequest{Select: &daemon.LeaseSelector{Platform: "mock"}, StartAt: rfc(time.Now().Add(3 * time.Hour)), TTLSec: 60})
	if err != nil || !strings.HasSuffix(r.Lease.Device, "@hub-a") {
		t.Fatalf("past hub-b's 2h lead: %+v %v", r, err)
	}
	_, err = c.Reserve(ctx, daemon.ReservationRequest{Select: &daemon.LeaseSelector{Platform: "mock"}, StartAt: rfc(time.Now().Add(30 * time.Hour)), TTLSec: 60})
	if !isCode(err, daemon.CodeUsage) || !strings.Contains(err.Error(), "ahead") {
		t.Fatalf("past every hub's lead: %v", err)
	}
}

func TestMesh_HubStatusReportsTheReservationLead(t *testing.T) {
	setupHome(t)
	h := startHub(t, "hub-l", testDevices("l"), func(m *Config, d *daemon.Config) {
		m.ReservationLead, d.ReservationLead = 2*time.Hour, 2*time.Hour
	})
	st, err := h.client().Hub(context.Background())
	if err != nil || st.Hub.ReservationLeadSec != 7200 {
		t.Fatalf("status: %+v %v", st, err)
	}
	_, err = h.client().Reserve(context.Background(), daemon.ReservationRequest{Device: "l1", StartAt: rfc(time.Now().Add(3 * time.Hour))})
	if !isCode(err, daemon.CodeUsage) {
		t.Fatalf("over a 2h lead: %v", err)
	}
}

// Through the mesh, a peer's DEVICE_IN_USE that names the reservation a
// shorter window would end by wins over the entry hub's generic one.
func TestMesh_SelectorLeasePrefersTheReservationAnswer(t *testing.T) {
	a, b := twoHubs(t)
	c := b.client()
	ctx := context.Background()
	for _, id := range []string{"b1", "b2"} {
		if _, err := c.AcquireLease(ctx, daemon.LeaseRequest{Device: id + "@hub-b", TTLSec: 3600}); err != nil {
			t.Fatal(err)
		}
	}
	next := rfc(time.Now().Add(2 * time.Hour))
	for _, id := range []string{"a1", "a2"} {
		if _, err := c.Reserve(ctx, daemon.ReservationRequest{Device: id + "@hub-a", StartAt: next, TTLSec: 60}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := c.AcquireLease(ctx, daemon.LeaseRequest{Select: &daemon.LeaseSelector{Platform: "mock"}})
	var de *daemon.Error
	if !isCode(err, daemon.CodeDeviceInUse) {
		t.Fatalf("all taken: %v", err)
	}
	de = err.(*daemon.Error)
	if de.Details["nextStartsAt"] != next {
		t.Fatalf("the peer's reservation answer was lost: %+v", de)
	}
	_ = a
}
