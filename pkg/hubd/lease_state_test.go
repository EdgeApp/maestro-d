package hubd

import (
	"encoding/json"
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

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

func stateOnDisk(t *testing.T, s *Server) leaseFile {
	t.Helper()
	var state leaseFile
	b := mustRead(t, LeasesPath(s.cfg.Name))
	if err := json.Unmarshal(b, &state); err != nil || state.Version != 2 {
		t.Fatal(string(b), err)
	}
	return state
}

func TestLeaseStateTransitions(t *testing.T) {
	env := newHubEnv(t, daemon.Config{})
	s := env.s.Server
	ctx := t.Context()
	description := strings.Repeat("測", 205)
	req := daemon.LeaseRequest{Device: "mock-1", Description: "  \x00" + description + "  ", Holder: "A", TTLSec: 30}
	active := env.lease(env.local, req).Lease
	pending := env.mustReserve(env.local, daemon.ReservationRequest{Device: "mock-2", StartAt: env.at(time.Minute), TTLSec: 40, Holder: "B", Description: "\x01 reservation \n"})
	file := stateOnDisk(t, s)
	want := strings.Repeat("測", 200)
	if len(file.Leases) != 2 || file.Leases[0].Lease != active || file.Leases[1].Lease != pending || active.Description != want || pending.Description != "reservation" {
		t.Fatal(file)
	}
	env.clock.advance(2 * time.Second)
	renewed, err := env.local.RenewLease(ctx, active.ID, 45)
	if err != nil || renewed.Lease.StartsAt != active.StartsAt || renewed.Lease.ExpiresAt == active.ExpiresAt {
		t.Fatal(renewed, err)
	}
	if f := stateOnDisk(t, s); f.Leases[0].Lease != renewed.Lease {
		t.Fatal(f)
	}
	devices, err := env.local.Devices(ctx, "mock")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range devices {
		switch d.ID {
		case "mock-1":
			if d.Lease == nil || d.Lease.Description != want || d.Lease.StartsAt != active.StartsAt {
				t.Fatal(d)
			}
		case "mock-2":
			if len(d.Reservations) != 1 || d.Reservations[0].Description != "reservation" {
				t.Fatal(d)
			}
		}
	}
	env.clock.advance(time.Minute)
	env.sweep()
	f := stateOnDisk(t, s)
	if len(f.Leases) != 1 || f.Leases[0].ID != pending.ID || f.Leases[0].State != daemon.LeaseActive || f.Leases[0].StartsAt != pending.StartsAt {
		t.Fatal(f)
	}
	if _, err := env.local.ReleaseLease(ctx, pending.ID); err != nil {
		t.Fatal(err)
	}
	if f = stateOnDisk(t, s); len(f.Leases) != 0 {
		t.Fatal(f)
	}
}

func TestLeaseStateCoalescesConcurrentRenewals(t *testing.T) {
	env := newHubEnv(t, daemon.Config{})
	s := env.s.Server
	l := env.lease(env.local, daemon.LeaseRequest{Device: "mock-1", TTLSec: 30}).Lease
	var writes atomic.Int32
	s.writer.write = func(path string, f leaseFile) error { writes.Add(1); return writeLeases(path, f) }
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"ttlSec":90}`))
			r.SetPathValue("lid", l.ID)
			w := httptest.NewRecorder()
			s.handleRenewLease(w, r)
			if w.Code != 200 {
				t.Error(w.Body)
			}
		}()
	}
	close(start)
	wg.Wait()
	if n := writes.Load(); n < 1 || n > 2 {
		t.Fatalf("%d writes for 100 renewals", n)
	}
	if stateOnDisk(t, s).Leases[0].ExpiresAt != env.at(90*time.Second) {
		t.Fatal("renew returned before persistence")
	}
	s.endAllLeases()
	if len(stateOnDisk(t, s).Leases) != 0 {
		t.Fatal("shutdown returned before persistence")
	}
}

func TestLeaseStateExcludesCloud(t *testing.T) {
	h := newCloudHub(t, daemon.Config{})
	l, err := h.remote.AcquireLease(t.Context(), daemon.LeaseRequest{Select: cloudSel()})
	if err != nil {
		t.Fatal(err)
	}
	envLease, err := h.local.AcquireLease(t.Context(), daemon.LeaseRequest{Device: "mock-1"})
	if err != nil {
		t.Fatal(err)
	}
	f := stateOnDisk(t, h.s.Server)
	if len(f.Leases) != 1 || f.Leases[0].ID != envLease.Lease.ID {
		t.Fatal(f)
	}
	for _, r := range f.AcquisitionReplays {
		if r.LeaseID == l.Lease.ID {
			t.Fatal(r)
		}
	}
}

// A keyed acquisition whose journal write fails answers INTERNAL, and no
// client resends that, so the grant and its replay end with the answer: the
// device is free at once and the key starts over once the disk recovers.
func TestLeaseStateEndsGrantWhenItsWriteFails(t *testing.T) {
	env := newHubEnv(t, daemon.Config{})
	s := env.s.Server
	var fail atomic.Bool
	fail.Store(true)
	s.writer.write = func(path string, f leaseFile) error {
		if fail.Load() {
			return errors.New("injected write failure")
		}
		return writeLeases(path, f)
	}
	var attaches atomic.Int32
	env.onAttach = func() { attaches.Add(1) }
	key := "failed"
	post := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/leases", strings.NewReader(`{"device":"mock-1","attach":{"platform":"mock"}}`))
		r.Header.Set(daemon.HeaderRequestID, key)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	for range 2 {
		if w := post(); w.Code != 500 {
			t.Fatal(w.Code, w.Body)
		}
		s.leases.mu.Lock()
		live, replays := len(s.leases.byID), len(s.leases.replays)
		s.leases.mu.Unlock()
		if live != 0 || replays != 0 {
			t.Fatalf("after a failed commit: %d grants, %d replays", live, replays)
		}
		if l, err := env.local.Leases(t.Context()); err != nil || len(l) != 0 {
			t.Fatal(l, err)
		}
	}
	// The failed key keeps its answer; a new request finds the device free.
	fail.Store(false)
	key = "original"
	w := post()
	if w.Code != 200 || w.Header().Get(daemon.HeaderReplayed) == "true" || attaches.Load() != 2 {
		t.Fatal(w.Code, w.Body, attaches.Load())
	}
	f := stateOnDisk(t, s)
	if len(f.Leases) != 1 || len(f.AcquisitionReplays) != 1 || f.AcquisitionReplays[0].LeaseID != f.Leases[0].ID {
		t.Fatal(f)
	}
	if _, err := env.local.ReleaseLease(t.Context(), f.Leases[0].ID); err != nil {
		t.Fatal(err)
	}
	f = stateOnDisk(t, s)
	if len(f.Leases) != 0 || len(f.AcquisitionReplays) != 1 || f.AcquisitionReplays[0].Device != "mock-1" {
		t.Fatal(f)
	}
	// Read a fresh table from the released revision. The exact original
	// successful response survives even though the device is free again.
	fresh := newHubEnv(t, daemon.Config{})
	path := LeasesPath(fresh.s.cfg.Name)
	if err := writeLeases(path, f); err != nil {
		t.Fatal(err)
	}
	if err := fresh.s.loadLeases(); err != nil {
		t.Fatal(err)
	}
	s = fresh.s.Server
	if w = post(); w.Code != 200 || w.Header().Get(daemon.HeaderReplayed) != "true" || len(fresh.leaseEvents()) != 0 {
		t.Fatal(w.Code, w.Body)
	}
}

func TestSelectorJournalDurability(t *testing.T) {
	s := newHubEnv(t, daemon.Config{}).s.Server
	a := daemon.SelectorAttempt{RequestID: "selector", AttemptID: "dispatch", Owner: "peer", Pass: "ready", StartedAt: time.Now(), ForwardedBody: json.RawMessage(`{"device":"one"}`)}
	if err := s.StoreSelectorAttempt(a); err != nil {
		t.Fatal(err)
	}
	f := stateOnDisk(t, s)
	if len(f.SelectorAttempts) != 1 || f.SelectorAttempts[0].Owner != "peer" {
		t.Fatal(f)
	}
	if r, p, err := s.SelectorOutcome(a.RequestID); err != nil || r != nil || p.AttemptID != "dispatch" {
		t.Fatal(r, p, err)
	}
	r := daemon.AcquisitionReplay{RequestID: a.RequestID, LeaseID: "lease@peer", Device: "one@peer", Status: 200, CompletedAt: time.Now(), Response: json.RawMessage(`{"ok":true}`)}
	if err := s.FinishSelectorAttempt(a.RequestID, &r); err != nil {
		t.Fatal(err)
	}
	f = stateOnDisk(t, s)
	if len(f.SelectorAttempts) != 0 || len(f.AcquisitionReplays) != 1 {
		t.Fatal(f)
	}
	// A full journal can evict a completed outcome, never a live dispatch.
	for i := 0; i < daemon.ReplayLimit-1; i++ {
		key := fmt.Sprint(i)
		s.leases.attempts[key] = daemon.SelectorAttempt{RequestID: key, StartedAt: time.Now()}
	}
	a.RequestID = "last"
	if err := s.StoreSelectorAttempt(a); err != nil {
		t.Fatal(err)
	} // evicts the completed replay
	if _, ok := s.leases.replays[r.RequestID]; ok {
		t.Fatal("full journal kept the completed replay")
	}
	if err := s.StoreSelectorAttempt(a); err != nil {
		t.Fatal(err)
	} // retry an existing slot
	a.RequestID = "full"
	if err := s.StoreSelectorAttempt(a); err == nil {
		t.Fatal("evicted a live attempt")
	}
	if err := s.FinishSelectorAttempt(a.RequestID, &r); err == nil {
		t.Fatal("journal overflow")
	}
	if err := s.FinishSelectorAttempt("last", nil); err != nil {
		t.Fatal(err)
	}
	// Expired attempts and released, expired outcomes free their slots.
	a.StartedAt = time.Now().Add(-2 * daemon.ReplayTTL)
	s.leases.attempts["expired"] = a
	r.CompletedAt = a.StartedAt
	s.leases.replays["expired"] = r
	if r, p, err := s.SelectorOutcome("expired"); err != nil || r != nil || p != nil {
		t.Fatal(r, p, err)
	}
}

func TestLeaseStateMigrationPrecedence(t *testing.T) {
	env := newHubEnv(t, daemon.Config{})
	path := LeasesPath(env.s.cfg.Name)
	old := filepath.Join(filepath.Dir(path), "scheduled-leases.json")
	if err := os.MkdirAll(filepath.Dir(old), 0700); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"reservations":[{"id":"old","device":"mock-1","startsAt":"` + env.at(time.Hour) + `","expiresAt":"` + env.at(2*time.Hour) + `"}]}`)
	if err := os.WriteFile(old, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := env.s.loadLeases(); err != nil {
		t.Fatal(err)
	}
	f := stateOnDisk(t, env.s.Server)
	if len(f.Leases) != 1 || f.Leases[0].State != daemon.LeaseScheduled || string(mustRead(t, old+".migrated")) != string(data) {
		t.Fatal(f)
	}
	if err := os.WriteFile(old, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readLeases(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := env.s.loadLeases(); err != nil {
		t.Fatal(err)
	}
	if string(mustRead(t, old+".bad")) != "invalid" {
		t.Fatal("bad schedule not preserved")
	}
}

func TestLeaseWriterWaitsForEachRevision(t *testing.T) {
	s := newHubEnv(t, daemon.Config{}).s.Server
	writing := make(chan struct{})
	resume := make(chan struct{})
	var writes atomic.Int32
	s.writer.write = func(path string, f leaseFile) error {
		if writes.Add(1) == 1 {
			close(writing)
			<-resume
		}
		return writeLeases(path, f)
	}
	first := s.queueLeases(true)
	<-writing
	second := s.queueLeases(true)
	if first == second {
		t.Fatal("later mutation joined a snapshot already in flight")
	}
	if waiter := s.queueLeases(false); waiter != second {
		t.Fatal("flush missed the pending revision")
	}
	close(resume)
	if err := first.wait(); err != nil {
		t.Fatal(err)
	}
	if err := second.wait(); err != nil {
		t.Fatal(err)
	}
	if writes.Load() != 2 || s.writer.committed != second.revision {
		t.Fatal("revision left unwritten")
	}
}
