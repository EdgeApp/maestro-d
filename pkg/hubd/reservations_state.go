package hubd

// reservations_state.go: a hub's reservations outlive its process.
//
// A hub writes the reservations that have not started to
// <home>/hub/scheduled-leases.json (<home>/hub/daemons/<daemon>/… for a
// daemon other than hub, beside its secrets/), mode 0600, through a temp
// file and a rename, after every change to that set: a reservation, a
// cancel, a start, a device delete that ends one, and a load that drops or
// starts entries. When it starts it reads the file back: a reservation
// whose window is over is dropped, one whose start passed while the hub
// was down starts at once with its own end, and the rest are scheduled
// again under their own lease ids, so the ids clients hold stay valid.
// Started leases are not written: their device sessions end with the hub.
// Nor is the duty to shut down a device an earlier lease booted: a stopping
// hub shuts down what it booted, and after a crash the device is no longer
// known as the hub's, so a reservation read back never shuts one down. A
// file that does not parse is renamed .bad and the hub starts without it.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/logger"
)

// reservationRecord is one reservation in scheduled-leases.json.
type reservationRecord struct {
	ID        string `json:"id"`
	Device    string `json:"device"`
	Holder    string `json:"holder,omitempty"`
	StartsAt  string `json:"startsAt"`
	ExpiresAt string `json:"expiresAt"`
	TTLSec    int    `json:"ttlSec"`
	MaxSec    int    `json:"maxSec"`
	// Shared: the device was in the hub's shared pool when it was
	// reserved (leaseEntry.sharedAtGrant).
	Shared bool `json:"shared,omitempty"`
}

type reservationFile struct {
	Reservations []reservationRecord `json:"reservations"`
}

// ReservationsPath is where hub daemon name keeps its reservations.
func ReservationsPath(name string) string {
	if name == "" || name == "hub" {
		return filepath.Join(daemon.Home(), "hub", "scheduled-leases.json")
	}
	return filepath.Join(daemon.Home(), "hub", "daemons", name, "scheduled-leases.json")
}

// snapshot lists the reservations that have not started.
func (t *leaseTable) snapshot() []reservationRecord {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := []reservationRecord{}
	for _, list := range t.scheduled {
		for _, e := range list {
			out = append(out, reservationRecord{ID: e.id, Device: e.device, Holder: e.holder,
				StartsAt: e.starts.UTC().Format(time.RFC3339), ExpiresAt: e.expires.UTC().Format(time.RFC3339),
				TTLSec: int(e.ttl / time.Second), MaxSec: int(e.max / time.Second), Shared: e.sharedAtGrant.Load()})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Device != out[j].Device {
			return out[i].Device < out[j].Device
		}
		return out[i].StartsAt < out[j].StartsAt
	})
	return out
}

// saveReservations writes the hub's reservations. A failure is logged:
// the reservations still hold in this process.
func (s *Server) saveReservations() {
	if err := s.flushReservations(); err != nil {
		logger.Warn("reservations: %v", err)
	}
}

func (s *Server) flushReservations() error {
	s.reservationsMu.Lock()
	defer s.reservationsMu.Unlock()
	return writeReservations(ReservationsPath(s.cfg.Name), s.leases.snapshot())
}

func writeReservations(path string, recs []reservationRecord) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(reservationFile{Reservations: recs}, "", "  ")
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp.%d", path, os.Getpid())
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// restore puts a reservation read back from disk into the table: started
// at once when its start has passed (its end stays its own), else
// scheduled. False when its window is over, or another lease's overlaps it
// (a file edited by hand).
func (t *leaseTable) restore(r reservationRecord) (*leaseEntry, bool) {
	starts, err1 := time.Parse(time.RFC3339, r.StartsAt)
	expires, err2 := time.Parse(time.RFC3339, r.ExpiresAt)
	if err1 != nil || err2 != nil || r.ID == "" || r.Device == "" || !starts.Before(expires) {
		return nil, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if !now.Before(expires) || t.byID[r.ID] != nil || t.overlapLocked(r.Device, starts, expires, nil) != nil {
		return nil, false
	}
	e := &leaseEntry{id: r.ID, device: r.Device, holder: r.Holder, ttl: time.Duration(r.TTLSec) * time.Second,
		max: time.Duration(r.MaxSec) * time.Second, starts: starts, expires: expires, ended: make(chan struct{})}
	e.sharedAtGrant.Store(r.Shared)
	t.byID[e.id] = e
	if starts.After(now) {
		t.scheduleLocked(e)
	} else {
		e.started.Store(true)
		t.byDevice[e.device] = e
	}
	return e, true
}

// loadReservations reads the hub's reservations back as it starts.
func (s *Server) loadReservations() {
	path := ReservationsPath(s.cfg.Name)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	var f reservationFile
	if err == nil {
		err = json.Unmarshal(data, &f)
	}
	if err != nil {
		bad := path + ".bad"
		logger.Warn("reservations: %s is unreadable (%v); moved to %s, starting without it", path, err, bad)
		_ = os.Rename(path, bad)
		return
	}
	kept, started, dropped := 0, 0, 0
	for _, r := range f.Reservations {
		e, ok := s.leases.restore(r)
		switch {
		case !ok:
			dropped++
		case e.pending():
			kept++
		default:
			started++
			logger.Info("lease %s: %s reserved by %s started while the hub was down; it runs until %s", e.id, e.device,
				daemon.HolderName(e.holder), e.expires.UTC().Format(time.RFC3339))
			s.Events().Publish(daemon.Event{Type: "lease.granted", Device: e.device, Message: daemon.HolderName(e.holder)})
		}
	}
	logger.Info("reservations: %d kept, %d started, %d over (from %s)", kept, started, dropped, path)
	if started+dropped > 0 {
		s.saveReservations()
	}
}
