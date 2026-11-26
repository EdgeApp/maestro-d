package hubd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/logger"
)

type leaseRecord struct {
	daemon.Lease
	AVD    string `json:"avd,omitempty"`
	Shared bool   `json:"shared,omitempty"`
}

type leaseFile struct {
	Version            int                        `json:"version"`
	Leases             []leaseRecord              `json:"leases"`
	AcquisitionReplays []daemon.AcquisitionReplay `json:"acquisitionReplays,omitempty"`
	SelectorAttempts   []daemon.SelectorAttempt   `json:"selectorAttempts,omitempty"`
}

// LeasesPath locates the lease journal for the named hub.
func LeasesPath(name string) string {
	dir := filepath.Join(daemon.Home(), "hub")
	if name != "" && name != "hub" {
		dir = filepath.Join(dir, "daemons", name)
	}
	return filepath.Join(dir, "leases.json")
}

func (t *leaseTable) snapshot() leaseFile {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneJournal(time.Now())
	f := leaseFile{Version: 2, Leases: []leaseRecord{}}
	for _, e := range t.byID {
		if e.durable {
			f.Leases = append(f.Leases, leaseRecord{Lease: e.view(), AVD: e.avd, Shared: e.sharedAtGrant.Load()})
		}
	}
	sort.Slice(f.Leases, func(i, j int) bool {
		a, b := f.Leases[i], f.Leases[j]
		if a.Device != b.Device {
			return a.Device < b.Device
		}
		return a.StartsAt < b.StartsAt
	})
	for _, r := range t.replays {
		f.AcquisitionReplays = append(f.AcquisitionReplays, r)
	}
	for _, a := range t.attempts {
		f.SelectorAttempts = append(f.SelectorAttempts, a)
	}
	sort.Slice(f.AcquisitionReplays, func(i, j int) bool { return f.AcquisitionReplays[i].RequestID < f.AcquisitionReplays[j].RequestID })
	sort.Slice(f.SelectorAttempts, func(i, j int) bool { return f.SelectorAttempts[i].RequestID < f.SelectorAttempts[j].RequestID })
	return f
}

func writeLeases(path string, state leaseFile) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".leases-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err = enc.Encode(state); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return nil
	} // Windows cannot flush directory handles.
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err = d.Sync(); errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) {
		return nil
	}
	return err
}

// readLeases upgrades the old schedule before archiving it. A v2 file
// always wins, including when a crash left both names in the directory.
func readLeases(path string) (leaseFile, error) {
	var f leaseFile
	data, err := os.ReadFile(path)
	if !errors.Is(err, fs.ErrNotExist) {
		if err == nil {
			err = json.Unmarshal(data, &f)
		}
		if err == nil && f.Version != 2 {
			err = fmt.Errorf("unsupported leases version %d", f.Version)
		}
		if err != nil {
			return f, &os.PathError{Op: "read lease state", Path: path, Err: err}
		}
		return f, nil
	}
	legacy := filepath.Join(filepath.Dir(path), "scheduled-leases.json")
	data, err = os.ReadFile(legacy)
	if errors.Is(err, fs.ErrNotExist) {
		return leaseFile{Version: 2}, nil
	}
	var old struct {
		Reservations []leaseRecord `json:"reservations"`
	}
	if err == nil {
		err = json.Unmarshal(data, &old)
	}
	if err != nil {
		return f, &os.PathError{Op: "read lease state", Path: legacy, Err: err}
	}
	f = leaseFile{Version: 2, Leases: old.Reservations}
	for i := range f.Leases {
		f.Leases[i].State = daemon.LeaseScheduled
		f.Leases[i].Booted = false
	}
	if err = writeLeases(path, f); err != nil {
		return f, fmt.Errorf("migrate lease state: %w", err)
	}
	return f, os.Rename(legacy, legacy+".migrated")
}

func (t *leaseTable) restore(r leaseRecord) (*leaseEntry, bool) {
	starts, err1 := time.Parse(time.RFC3339, r.StartsAt)
	expires, err2 := time.Parse(time.RFC3339, r.ExpiresAt)
	if (r.State != daemon.LeaseActive && r.State != daemon.LeaseScheduled) || err1 != nil || err2 != nil || r.ID == "" || r.Device == "" || !starts.Before(expires) {
		return nil, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if !now.Before(expires) || t.byID[r.ID] != nil || t.overlapLocked(r.Device, starts, expires, nil) != nil {
		return nil, false
	}
	e := &leaseEntry{id: r.ID, device: r.Device, avd: r.AVD, holder: r.Holder, description: daemon.LeaseDescription(r.Description), durable: true, booted: r.Booted, ttl: time.Duration(r.TTLSec) * time.Second,
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

// A batch includes all revisions assigned before its snapshot. Only one
// worker writes at a time. The table lock is never held during file I/O.
type leaseBatch struct {
	revision uint64
	done     chan struct{}
	err      error
}
type leaseWriter struct {
	mu                  sync.Mutex
	revision, committed uint64
	pending, writing    *leaseBatch
	write               func(string, leaseFile) error
}

func (s *Server) queueLeases(changed bool) *leaseBatch {
	w := &s.writer
	w.mu.Lock()
	defer w.mu.Unlock()
	if changed {
		w.revision++
	}
	if w.committed == w.revision {
		return nil
	}
	if w.pending != nil {
		w.pending.revision = w.revision
		return w.pending
	}
	if w.writing != nil && w.writing.revision == w.revision {
		return w.writing
	}
	w.pending = &leaseBatch{revision: w.revision, done: make(chan struct{})}
	if w.writing == nil {
		go s.writeBatches()
	}
	return w.pending
}

func (s *Server) writeBatches() {
	w := &s.writer
	for {
		time.Sleep(60 * time.Millisecond)
		w.mu.Lock()
		b := w.pending
		w.pending, w.writing = nil, b
		w.mu.Unlock()
		b.err = w.write(LeasesPath(s.cfg.Name), s.leases.snapshot())
		w.mu.Lock()
		if b.err == nil {
			w.committed = b.revision
		}
		w.writing = nil
		more := w.pending != nil
		close(b.done)
		w.mu.Unlock()
		if !more {
			return
		}
	}
}
func (b *leaseBatch) wait() error {
	if b == nil {
		return nil
	}
	<-b.done
	return b.err
}

func (s *Server) saveLeases() error { return s.queueLeases(true).wait() }

func (s *Server) loadLeases() error {
	path := LeasesPath(s.cfg.Name)
	f, err := readLeases(path)
	if err != nil {
		// A failed migration write must leave the source available to retry.
		var pe *os.PathError
		if !errors.As(err, &pe) || pe.Op != "read lease state" {
			return err
		}
		path = pe.Path
		if renameErr := os.Rename(path, path+".bad"); renameErr != nil {
			return fmt.Errorf("moving unreadable state: %w (read: %v)", renameErr, err)
		}
		logger.Warn("leases: moved unreadable %s to .bad: %v", path, err)
		return nil
	}
	t := s.leases
	t.mu.Lock()
	for _, r := range f.AcquisitionReplays {
		t.replays[r.RequestID] = r
	}
	for _, a := range f.SelectorAttempts {
		t.attempts[a.RequestID] = a
	}
	changed := t.pruneJournal(time.Now())
	t.mu.Unlock()
	for _, r := range f.Leases {
		e, ok := t.restore(r)
		if !ok {
			changed = true
			continue
		}
		changed = changed || r.Description != e.description
		if !e.pending() && r.State == daemon.LeaseScheduled {
			changed = true
			s.Events().Publish(daemon.Event{Type: "lease.granted", Device: e.device, Message: daemon.HolderName(e.holder)})
		}
	}
	if changed {
		return s.saveLeases()
	}
	return nil
}

// Replay records share the acquisition routes' existing empty-device scope.
// Pending dispatches retain their slot until they resolve or age out.
func (t *leaseTable) pruneJournal(now time.Time) bool {
	before := len(t.replays) + len(t.attempts)
	cutoff := now.Add(-daemon.ReplayTTL)
	for key, r := range t.replays {
		if r.CompletedAt.Before(cutoff) {
			delete(t.replays, key)
		}
	}
	for key, a := range t.attempts {
		if a.StartedAt.Before(cutoff) {
			delete(t.attempts, key)
		}
	}
	return before != len(t.replays)+len(t.attempts)
}
func (t *leaseTable) makeJournalRoom(id string) error {
	t.pruneJournal(time.Now())
	if _, ok := t.replays[id]; ok {
		return nil
	}
	if _, ok := t.attempts[id]; ok {
		return nil
	}
	if len(t.replays)+len(t.attempts) < daemon.ReplayLimit {
		return nil
	}
	var oldest daemon.AcquisitionReplay
	for _, r := range t.replays {
		if oldest.RequestID == "" || r.CompletedAt.Before(oldest.CompletedAt) {
			oldest = r
		}
	}
	if oldest.RequestID == "" {
		return daemon.Errorf(daemon.CodeBusy, "hub has %d unresolved acquisition requests", len(t.attempts))
	}
	delete(t.replays, oldest.RequestID)
	return nil
}

func (s *Server) SelectorOutcome(id string) (*daemon.AcquisitionReplay, *daemon.SelectorAttempt, error) {
	t := s.leases
	t.mu.Lock()
	changed := t.pruneJournal(time.Now())
	r, ok := t.replays[id]
	a, pending := t.attempts[id]
	batch := s.queueLeases(changed)
	t.mu.Unlock()
	if err := batch.wait(); err != nil {
		return nil, nil, err
	}
	if ok {
		return &r, nil, nil
	}
	if pending {
		return nil, &a, nil
	}
	return nil, nil, nil
}

func (s *Server) StoreSelectorAttempt(a daemon.SelectorAttempt) error {
	t := s.leases
	t.mu.Lock()
	if err := t.makeJournalRoom(a.RequestID); err != nil {
		t.mu.Unlock()
		return err
	}
	t.attempts[a.RequestID] = a
	b := s.queueLeases(true)
	t.mu.Unlock()
	return b.wait()
}
func (s *Server) FinishSelectorAttempt(id string, r *daemon.AcquisitionReplay) error {
	t := s.leases
	t.mu.Lock()
	if r != nil {
		if err := t.makeJournalRoom(id); err != nil {
			t.mu.Unlock()
			return err
		}
		t.replays[id] = *r
	}
	delete(t.attempts, id)
	b := s.queueLeases(true)
	t.mu.Unlock()
	return b.wait()
}

func (s *Server) acquisition(h http.HandlerFunc) http.HandlerFunc {
	run := s.Idempotent(h)
	return func(w http.ResponseWriter, r *http.Request) {
		if key := r.Header.Get(daemon.HeaderRequestID); key != "" {
			replay, _, err := s.SelectorOutcome(key)
			if err != nil {
				daemon.WriteError(w, daemon.WrapErr(daemon.CodeInternal, err))
				return
			}
			if replay != nil {
				w.Header().Set(daemon.HeaderReplayed, "true")
				daemon.WriteJSON(w, replay.Status, replay.Response)
				return
			}
		}
		run(w, r)
	}
}

// Until the optional attach succeeds, no snapshot may include this grant.
// The replay and the lease become visible to the writer under the same lock.
func (s *Server) commitAcquisition(w http.ResponseWriter, r *http.Request, e *leaseEntry, result daemon.LeaseResult) bool {
	if e.cloud {
		return true
	}
	key := r.Header.Get(daemon.HeaderRequestID)
	t := s.leases
	t.mu.Lock()
	if t.byID[e.id] != e || s.ShuttingDown() {
		t.mu.Unlock()
		daemon.WriteError(w, leaseExpiredErr(e))
		return false
	}
	if key != "" {
		if err := t.makeJournalRoom(key); err != nil {
			t.mu.Unlock()
			s.endLease(e, endReleased)
			daemon.WriteError(w, daemon.WrapErr(daemon.CodeBusy, err))
			return false
		}
		response, err := json.Marshal(result)
		if err != nil {
			t.mu.Unlock()
			s.endLease(e, endReleased)
			daemon.WriteError(w, daemon.WrapErr(daemon.CodeInternal, err))
			return false
		}
		t.replays[key] = daemon.AcquisitionReplay{RequestID: key, LeaseID: e.id, Device: e.device, CompletedAt: time.Now().UTC(), Status: http.StatusOK, Response: response}
	}
	e.durable = true
	batch := s.queueLeases(true)
	t.mu.Unlock()
	if err := batch.wait(); err != nil {
		if key == "" {
			s.endLease(e, endReleased)
		}
		daemon.WriteError(w, daemon.WrapErr(daemon.CodeInternal, err))
		return false
	}
	return true
}
