package hubd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/internal/daemontest"
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
	if err := s.stopLeases(true); err != nil {
		t.Fatal(err)
	}
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

func TestLeaseStateRetriesOriginalKeyAfterWriteFailure(t *testing.T) {
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
	post := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/leases", strings.NewReader(`{"device":"mock-1","attach":{"platform":"mock"}}`))
		r.Header.Set(daemon.HeaderRequestID, "original")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		return w
	}
	if w := post(); w.Code != 500 {
		t.Fatal(w.Code, w.Body)
	}
	if w := post(); w.Code != 500 {
		t.Fatal(w.Code, w.Body)
	}
	fail.Store(false)
	w := post()
	if w.Code != 200 || w.Header().Get(daemon.HeaderReplayed) != "true" || attaches.Load() != 1 {
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

func loadRestartState(t *testing.T, env *hubEnv, state leaseFile) {
	t.Helper()
	if err := writeLeases(LeasesPath(env.s.cfg.Name), state); err != nil {
		t.Fatal(err)
	}
	if err := env.s.loadLeases(); err != nil {
		t.Fatal(err)
	}
}

func TestLeases_RestartTransfersBootAcrossWindows(t *testing.T) {
	env := newHubEnv(t, daemon.Config{})
	a := env.lease(env.local, daemon.LeaseRequest{Select: &daemon.LeaseSelector{Name: "Spare Phone", Boot: true}, TTLSec: 60}).Lease
	successor := env.mustReserve(env.remote, daemon.ReservationRequest{Device: a.Device, StartAt: env.at(65 * time.Second), TTLSec: 180, Holder: "next"})
	env.s.Server.Server.Shutdown("stop")
	<-env.s.Done()
	if len(env.stopped) != 0 {
		t.Fatal(env.stopped)
	}
	saved := stateOnDisk(t, env.s.Server)
	env = newHubEnv(t, daemon.Config{})
	env.clock.advance(70 * time.Second)
	env.booted[a.Device] = true
	loadRestartState(t, env, saved)
	disk := stateOnDisk(t, env.s.Server)
	if len(disk.Leases) != 1 || disk.Leases[0].ID != successor.ID || !disk.Leases[0].Booted || len(disk.BootDuties) != 0 || len(env.stopped) != 0 {
		t.Fatal(disk, env.stopped)
	}
	env.remote.SetLease(a.Device, successor.ID)
	if _, err := env.remote.Attach(t.Context(), a.Device, daemon.AttachConfig{Platform: "mock"}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.remote.Command(t.Context(), a.Device, "tapOn", "x", true); err != nil {
		t.Fatal(err)
	}
	if _, err := env.remote.ReleaseLease(t.Context(), successor.ID); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(env.stopped, []string{a.Device}) {
		t.Fatal(env.stopped)
	}
}

func TestLeases_RestartVerifiesEmulatorAtUseAndCleanup(t *testing.T) {
	for _, expired := range []bool{false, true} {
		env := newHubEnv(t, daemon.Config{})
		env.emuAVD = "Replacement"
		configRequest(t, env.local, "PUT", "/avd:Original", "{}", 201)
		l := daemon.Lease{ID: "saved", Device: "emulator-5554", Holder: "Ada", Description: "original", Booted: true, State: daemon.LeaseActive, StartsAt: env.at(-time.Hour), ExpiresAt: env.at(time.Hour), TTLSec: 3600, MaxSec: 28800}
		if expired {
			l.ExpiresAt = env.at(-time.Minute)
		}
		record := leaseRecord{Lease: l, AVD: "android:original", Shared: true}
		clash := record
		clash.ID = "overlapping"
		loadRestartState(t, env, leaseFile{Version: 2, Leases: []leaseRecord{record, clash}})
		if env.s.leases.lookup(clash.ID) != nil {
			t.Fatal("overlapping entry restored")
		}
		if len(env.stopped) != 0 {
			t.Fatal("stopped replacement", env.stopped)
		}
		ctx := t.Context()
		if expired {
			if got := stateOnDisk(t, env.s.Server); len(got.BootDuties) != 1 || len(got.Leases) != 0 {
				t.Fatal(got)
			}
		} else {
			rows, err := env.local.HubDevices(ctx)
			if err != nil {
				t.Fatal(err)
			}
			i := slices.IndexFunc(rows.Devices, func(row daemon.HubConfigDevice) bool { return row.ID == "avd:Original" })
			if i < 0 || rows.Devices[i].Listed || rows.Devices[i].Lease == nil || rows.Devices[i].Lease.Holder != l.Holder {
				t.Fatalf("absent AVD config row: %+v", rows.Devices)
			}
			found, err := env.remote.Lease(ctx, l.ID)
			if err != nil || found.Lease.Device != "" || found.Lease.ID != l.ID || found.Lease.ExpiresAt != l.ExpiresAt {
				t.Fatal(found, err)
			}
			env.remote.SetLease(l.Device, l.ID)
			_, err = env.remote.Attach(ctx, l.Device, daemon.AttachConfig{Platform: "mock"})
			wantErrCode(t, err, daemon.CodeDeviceNotFound)
			_, err = env.remote.Command(ctx, l.Device, "tapOn", "x", true)
			wantErrCode(t, err, daemon.CodeDeviceNotFound)
			if _, attached := env.s.Registry().Record(l.Device); attached {
				t.Fatal("driver opened on replacement")
			}
		}
		const current = "emulator-5556"
		env.mu.Lock()
		env.phones = append(env.phones, daemon.DeviceInfo{ID: current, Platform: "android", Kind: "emulator", Ready: true, Name: "Original"})
		env.mu.Unlock()
		if expired {
			env.s.retryBootDuties()
			if err := env.s.queueLeases(false).wait(); err != nil {
				t.Fatal(err)
			}
		} else {
			found, err := env.remote.Lease(ctx, l.ID)
			if err != nil || found.Lease.Device != current || found.Lease.ID != l.ID || found.Lease.ExpiresAt != l.ExpiresAt {
				t.Fatal(found, err)
			}
			list, err := env.local.HubDevices(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range list.Devices {
				if d.ID == "avd:Original" && (!d.Listed || d.Lease == nil || d.Lease.Holder != l.Holder) {
					t.Fatalf("restored AVD config row: %+v", d)
				}
				if d.ID == l.Device && d.Lease != nil {
					t.Fatalf("replacement shows previous AVD's grant: %+v", d)
				}
			}
			_, err = env.local.AcquireLease(ctx, daemon.LeaseRequest{Device: current})
			wantErrCode(t, err, daemon.CodeDeviceInUse)
			env.remote.SetLease(current, l.ID)
			if _, err = env.remote.Attach(ctx, current, daemon.AttachConfig{Platform: "mock"}); err != nil {
				t.Fatal(err)
			}
			if _, err = env.remote.Command(ctx, current, "tapOn", "x", true); err != nil {
				t.Fatal(err)
			}
			if _, err = env.remote.ReleaseLease(ctx, l.ID); err != nil {
				t.Fatal(err)
			}
		}
		if !slices.Equal(env.stopped, []string{current}) {
			t.Fatal(env.stopped)
		}
		if got := stateOnDisk(t, env.s.Server); len(got.BootDuties) != 0 || len(got.Leases) != 0 {
			t.Fatal(got)
		}
	}
}

func TestRestoredAVDExpiresBeforeNextGrant(t *testing.T) {
	env := newHubEnv(t, daemon.Config{})
	env.emuAVD = "Pixel"
	device := daemon.DeviceInfo{ID: "emulator-5554", Name: "Pixel", Platform: "android", Kind: "emulator"}
	l := daemon.Lease{ID: "previous", Device: "emulator-5560", State: daemon.LeaseActive, StartsAt: env.at(-time.Minute), ExpiresAt: env.at(time.Minute)}
	loadRestartState(t, env, leaseFile{Version: 2, Leases: []leaseRecord{{Lease: l, AVD: avdKey(device)}}})
	env.clock.advance(2 * time.Minute)
	if !env.s.leases.releasing(device) || !env.s.leases.reserved(device) {
		t.Fatal("expired grant must reserve its discovered device until teardown")
	}
	if _, err := env.s.leases.grant(device.ID, avdKey(device), "next", time.Time{}, time.Minute, time.Hour, false); err == nil || err.Code != daemon.CodeDeviceInUse {
		t.Fatalf("unsettled grant admitted a successor: %v", err)
	}
	res, err := env.local.AcquireLease(t.Context(), daemon.LeaseRequest{Device: device.ID})
	if err != nil || env.s.leases.lookup(l.ID) != nil || env.s.leases.releasing(device) {
		t.Fatalf("successor %+v, previous grant or teardown still present: %v", res, err)
	}
}

func TestRestoreRemovedLeaseExpiry(t *testing.T) {
	env := newHubEnv(t, daemon.Config{})
	now := env.clock.now()
	loadRestartState(t, env, leaseFile{Version: 2, Removed: removedWindows{
		"ended":  {Device: "mock-1", Until: now},
		"future": {Device: "mock-1", Until: now.Add(time.Minute)},
	}})
	if disk := stateOnDisk(t, env.s.Server); len(disk.Removed) != 1 || !env.s.removedLeaseMatches("mock-1", "future") {
		t.Fatalf("unexpected restored windows: %v", disk.Removed)
	}
	env.clock.advance(time.Minute)
	env.s.sweepLeases()
	if err := env.s.saveLeases(); err != nil {
		t.Fatal(err)
	}
	if disk := stateOnDisk(t, env.s.Server); len(disk.Removed) != 0 || env.s.removedLeaseMatches("mock-1", "future") {
		t.Fatalf("retained expired credential: %v", disk.Removed)
	}
}

func TestOfflineRemovalRemembersCurrentEmulator(t *testing.T) {
	t.Setenv(daemon.EnvHome, t.TempDir())
	path := daemontest.DeviceConfig(t, "avd:Original")
	grant := leaseRecord{Lease: daemon.Lease{ID: "former", Device: "emulator-5554", Holder: "Ada", State: daemon.LeaseActive, ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339)}, AVD: "android:original"}
	if err := writeLeases(LeasesPath("hub"), leaseFile{Version: 2, Leases: []leaseRecord{grant}}); err != nil {
		t.Fatal(err)
	}
	absent, err := OpenOfflineConfig(path, daemon.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	row := absent.Listing().Devices[0]
	if row.Listed || row.Lease == nil || row.Lease.Holder != grant.Holder {
		t.Fatal(row)
	}
	f, err := OpenOfflineConfig(path, daemon.Deps{ListDevices: func(string) []daemon.DeviceInfo {
		return []daemon.DeviceInfo{
			{ID: "emulator-5554", Name: "Replacement", Platform: "android", Kind: "emulator", Ready: true},
			{ID: "emulator-5556", Name: "Original", Platform: "android", Kind: "emulator", Ready: true},
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Remove("avd:Original", true); err != nil {
		t.Fatal(err)
	}
	disk, err := readLeases(LeasesPath("hub"))
	if err != nil || len(disk.Leases) != 0 || disk.Removed[grant.ID].Device != "emulator-5556" {
		t.Fatal(disk, err)
	}
}

func TestOfflineBootDutyRetriedBeforeConfigRemoval(t *testing.T) {
	t.Setenv(daemon.EnvHome, t.TempDir())
	const id = "sim"
	path := daemontest.DeviceConfig(t, id)
	now := time.Now()
	grant := leaseRecord{Lease: daemon.Lease{ID: "l", Device: id, State: daemon.LeaseActive, Booted: true, StartsAt: now.Add(-time.Minute).Format(time.RFC3339), ExpiresAt: now.Add(time.Minute).Format(time.RFC3339)}}
	if err := writeLeases(LeasesPath("hub"), leaseFile{Version: 2, Leases: []leaseRecord{grant}}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	deps := daemon.Deps{ListDevices: func(string) []daemon.DeviceInfo { return []daemon.DeviceInfo{{ID: id, State: "Booted"}} }, ShutdownDevice: func(info daemon.DeviceInfo) error {
		calls++
		if calls == 1 {
			return fmt.Errorf("simulator temporarily unavailable")
		}
		return nil
	}}
	for attempt := 1; attempt <= 2; attempt++ {
		f, err := OpenOfflineConfig(path, deps)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := f.config.Lookup(id); !ok {
			t.Fatal("config changed before cleanup succeeded")
		}
		err = f.Remove(id, true)
		state, readErr := readLeases(LeasesPath("hub"))
		if readErr != nil || len(state.Leases) != 0 || state.Removed[grant.ID].Device != id || calls != attempt {
			t.Fatal(state, readErr, calls)
		}
		if attempt == 1 {
			if err == nil || len(state.BootDuties) != 1 {
				t.Fatal(err, state)
			}
		} else if err != nil || len(state.BootDuties) != 0 {
			t.Fatal(err, state)
		}
	}
}

func TestRestoreWithoutVerifiedAVDIdentity(t *testing.T) {
	env := newHubEnv(t, daemon.Config{})
	env.emuAVD = "Elsewhere"
	lease := daemon.Lease{ID: "legacy", Device: "emulator-5554", State: daemon.LeaseActive, Booted: true, StartsAt: env.at(-time.Minute), ExpiresAt: env.at(time.Hour)}
	loadRestartState(t, env, leaseFile{Version: 2, Leases: []leaseRecord{{Lease: lease, Shared: true}}})
	got, err := env.remote.Lease(t.Context(), lease.ID)
	if err != nil || got.Lease.Device != "" || got.Lease.ID != lease.ID {
		t.Fatal(got, err)
	}
	replacement := env.lease(env.local, daemon.LeaseRequest{Device: lease.Device})
	if replacement.Lease.ID == lease.ID {
		t.Fatal(replacement)
	}
	if _, err := env.remote.ReleaseLease(t.Context(), lease.ID); err != nil {
		t.Fatal(err)
	}
	if len(env.stopped) != 0 || env.s.leases.lookup(replacement.Lease.ID).booted {
		t.Fatal("used recycled serial to discharge an unknown duty")
	}
}

func TestBootCleanupWaitsForNamedDaemon(t *testing.T) {
	for _, mode := range []string{"restart", "offline removal"} {
		t.Run(mode, func(t *testing.T) {
			env := newHubEnv(t, daemon.Config{Name: "hub", HubProcess: true})
			const id = "SIM-UP"
			owner := &daemon.Info{Name: "other", PID: os.Getpid(), APIVersion: daemon.APIVersion}
			if err := daemon.WriteInfo(owner); err != nil {
				t.Fatal(err)
			}
			if err := atomicWriteJSON(daemon.DevicesPath(owner.Name), []daemon.DeviceInfo{{ID: id, Attached: true}}); err != nil {
				t.Fatal(err)
			}
			grant := leaseRecord{Lease: daemon.Lease{ID: "boot", Device: id, State: daemon.LeaseActive, Booted: true, StartsAt: env.at(-time.Hour), ExpiresAt: env.at(-time.Minute)}}
			state := leaseFile{Version: 2, Leases: []leaseRecord{grant}}
			if mode == "offline removal" {
				if err := writeLeases(LeasesPath("hub"), state); err != nil {
					t.Fatal(err)
				}
				deps := daemon.Deps{ListDevices: env.s.Registry().Live, ShutdownDevice: env.s.stop}
				offline, err := OpenOfflineConfig(daemontest.DeviceConfig(t, id), deps)
				if err != nil {
					t.Fatal(err)
				}
				if err := offline.Remove(id, true); err != nil {
					t.Fatal(err)
				}
				state = stateOnDisk(t, env.s.Server)
				if len(state.BootDuties) != 1 || len(state.Leases) != 0 || len(env.stopped) != 0 {
					t.Fatal(state, env.stopped)
				}
			}
			loadRestartState(t, env, state)
			env.s.retryBootDuties()
			if got := stateOnDisk(t, env.s.Server); len(got.BootDuties) != 1 || len(env.stopped) != 0 {
				t.Fatal(got, env.stopped)
			}
			if err := os.Remove(daemon.DevicesPath(owner.Name)); err != nil {
				t.Fatal(err)
			}
			env.s.retryBootDuties()
			if err := env.s.queueLeases(false).wait(); err != nil {
				t.Fatal(err)
			}
			if got := stateOnDisk(t, env.s.Server); len(got.BootDuties) != 0 || !slices.Equal(env.stopped, []string{id}) {
				t.Fatal(got, env.stopped)
			}
		})
	}
}

func TestEndLeasesStopWaitsForJournal(t *testing.T) {
	env := newHubEnv(t, daemon.Config{})
	s := env.s.Server
	lease := env.lease(env.local, daemon.LeaseRequest{Device: "mock-1"}).Lease
	fail := failLeaseWrites(s)
	for range 2 {
		if err := env.local.ShutdownHub(t.Context(), true); err == nil || !strings.Contains(err.Error(), "journal unwritable") {
			t.Fatalf("stop with failed write: %v", err)
		}
		if s.ShuttingDown() || stateOnDisk(t, s).Leases[0].ID != lease.ID {
			t.Fatal("failed stop must allow another attempt")
		}
	}
	fail.Store(false)
	if err := env.local.ShutdownHub(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	<-s.Done()
	if len(stateOnDisk(t, s).Leases) != 0 {
		t.Fatal("acknowledged stop left an active lease on disk")
	}
}

func failLeaseWrites(s *Server) *atomic.Bool {
	var fail atomic.Bool
	fail.Store(true)
	s.writer.write = func(path string, f leaseFile) error {
		if fail.Load() {
			return errors.New("journal unwritable")
		}
		return writeLeases(path, f)
	}
	return &fail
}

func TestReleaseRetryWaitsForJournal(t *testing.T) {
	env := newHubEnv(t, daemon.Config{})
	s := env.s.Server
	lease := env.lease(env.local, daemon.LeaseRequest{Device: "mock-1"}).Lease
	fail := failLeaseWrites(s)
	for range 2 {
		if _, err := env.local.ReleaseLease(t.Context(), lease.ID); err == nil || !strings.Contains(err.Error(), "journal unwritable") {
			t.Fatalf("release with failed write: %v", err)
		}
		if stateOnDisk(t, s).Leases[0].ID != lease.ID {
			t.Fatal("expected uncommitted removal")
		}
	}
	fail.Store(false)
	_, err := env.local.ReleaseLease(t.Context(), lease.ID)
	_ = wantErrCode(t, err, daemon.CodeLeaseExpired)
	if len(stateOnDisk(t, s).Leases) != 0 {
		t.Fatal("acknowledged release left an active lease on disk")
	}
}

func TestIncompleteAVDDiscoveryRetainsBootDuty(t *testing.T) {
	env := newHubEnv(t, daemon.Config{Name: "hub", HubProcess: true})
	const serial, stable = "emulator-5556", "avd:Lab"
	duty := bootDuty{Device: "emulator-5554", AVD: "android:lab"}
	env.phones = []daemon.DeviceInfo{{ID: stable, Platform: "android", Kind: "emulator", Name: "Lab", State: "Unknown"}}
	state := leaseFile{Version: 2, BootDuties: []bootDuty{duty}}
	if err := writeLeases(LeasesPath("hub"), state); err != nil {
		t.Fatal(err)
	}
	offline, err := OpenOfflineConfig(daemontest.DeviceConfig(t, stable), daemon.Deps{ListDevices: env.s.Registry().Live, ShutdownDevice: env.s.stop})
	if err != nil {
		t.Fatal(err)
	}
	if err := offline.Remove(stable, true); err != nil {
		t.Fatal(err)
	}
	state = stateOnDisk(t, env.s.Server)
	if !slices.Equal(state.BootDuties, []bootDuty{duty}) {
		t.Fatal("offline cleanup lost duty", state)
	}
	loadRestartState(t, env, state)
	if state = stateOnDisk(t, env.s.Server); !slices.Equal(state.BootDuties, []bootDuty{duty}) {
		t.Fatal("startup cleanup lost duty", state)
	}
	env.phones = []daemon.DeviceInfo{{ID: serial, Platform: "android", Kind: "emulator", Name: "Lab", State: "device", Ready: true}}
	env.s.retryBootDuties()
	if err := env.s.queueLeases(false).wait(); err != nil {
		t.Fatal(err)
	}
	if state = stateOnDisk(t, env.s.Server); len(state.BootDuties) != 0 || !slices.Equal(env.stopped, []string{serial}) {
		t.Fatal(state, env.stopped)
	}
}
