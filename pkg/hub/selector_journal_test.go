package hub

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

type testSelectorJournal struct {
	pending *daemon.SelectorAttempt
	replay  *daemon.AcquisitionReplay
	fail    string
}

func (j *testSelectorJournal) SelectorOutcome(string) (*daemon.AcquisitionReplay, *daemon.SelectorAttempt, error) {
	if j.fail == "lookup" {
		return nil, nil, errors.New("read failed")
	}
	return j.replay, j.pending, nil
}
func (j *testSelectorJournal) StoreSelectorAttempt(a daemon.SelectorAttempt) error {
	if j.fail == "store" {
		return errors.New("write failed")
	}
	j.pending = &a
	return nil
}
func (j *testSelectorJournal) FinishSelectorAttempt(id string, r *daemon.AcquisitionReplay) error {
	if j.fail == "finish" {
		return daemon.Errorf(daemon.CodeInternal, "commit failed")
	}
	j.pending = nil
	j.replay = r
	return nil
}
func selectorRequest() *http.Request {
	r := httptest.NewRequest("POST", "/v1/leases", strings.NewReader(`{"select":{"platform":"mock"}}`))
	r.Header.Set(daemon.HeaderRequestID, "selector-01")
	return r
}
func mockSelectorGrant(w http.ResponseWriter) {
	writeJSON(w, 200, daemon.LeaseResult{Envelope: daemon.Envelope{OK: true}, Lease: daemon.Lease{ID: "one", Device: "mock-1", State: "active", TTLSec: 60, ExpiresAt: "2026-11-25T20:01:00Z"}})
}
func TestKeyedSelectorJoinsAndReplays(t *testing.T) {
	journal := &testSelectorJournal{}
	called := make(chan struct{})
	finish := make(chan struct{})
	var calls atomic.Int32
	mesh := New(Config{Name: "alpha", SelectorJournal: journal})
	h := mesh.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(called)
		}
		if r.Header.Get(daemon.HeaderRequestID) != "selector-01/alpha/ready" {
			t.Errorf("attempt key: %v", r.Header)
		}
		<-finish
		mockSelectorGrant(w)
	}), false)
	var wg sync.WaitGroup
	const copies = 12
	recs := make([]*httptest.ResponseRecorder, copies)
	for i := range recs {
		recs[i] = httptest.NewRecorder()
		wg.Add(1)
		go func() { defer wg.Done(); h.ServeHTTP(recs[i], selectorRequest()) }()
		if i == 0 {
			<-called
		}
	}
	close(finish)
	wg.Wait()
	if calls.Load() != 1 || journal.pending != nil || journal.replay == nil || journal.replay.Device != "mock-1@alpha" {
		t.Fatalf("calls %d journal %+v", calls.Load(), journal)
	}
	replayed := 0
	for _, rec := range recs {
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "one@alpha") {
			t.Fatal(rec.Body.String())
		}
		if rec.Header().Get(daemon.HeaderReplayed) == "true" {
			replayed++
		}
	}
	if replayed != copies-1 {
		t.Fatalf("replays %d", replayed)
	}
}
func TestKeyedSelectorResumesExactAttempt(t *testing.T) {
	j := &testSelectorJournal{}
	pending := daemon.SelectorAttempt{RequestID: "selector-01", Owner: "alpha", Pass: "boot", AttemptID: "persisted-key", Path: "/v1/leases", ForwardedBody: json.RawMessage(`{"select":{"name":"saved","boot":true}}`), StartedAt: time.Now()}
	j.pending = &pending
	mesh := New(Config{Name: "alpha", SelectorJournal: j})
	var calls int
	h := mesh.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		data, _ := io.ReadAll(r.Body)
		if string(data) != string(pending.ForwardedBody) || r.Header.Get(daemon.HeaderRequestID) != pending.AttemptID || r.URL.Path != pending.Path {
			t.Fatalf("resumed request %s %s, %s", r.URL.Path, data, r.Header.Get(daemon.HeaderRequestID))
		}
		if calls == 1 {
			w.WriteHeader(200)
			io.WriteString(w, "{")
			return
		}
		mockSelectorGrant(w)
	}), false)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, selectorRequest())
	if envelopeCode(rec.Body.Bytes()) != daemon.CodePeerUnavailable || j.pending != &pending || calls != 1 {
		t.Fatal(rec.Body.String(), calls)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, selectorRequest())
	if calls != 2 || rec.Code != 200 || j.replay == nil || j.pending != nil {
		t.Fatal(rec.Body.String(), calls)
	}
	// A fresh mesh can answer an original caller after losing every owner connection.
	fresh := New(Config{Name: "alpha", SelectorJournal: j})
	rec = httptest.NewRecorder()
	fresh.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("contacted owner on replay") }), false).ServeHTTP(rec, selectorRequest())
	if rec.Code != 200 || rec.Header().Get(daemon.HeaderReplayed) != "true" {
		t.Fatal(rec.Body.String())
	}
}
func TestKeyedSelectorJournalFailures(t *testing.T) {
	for _, stage := range []string{"lookup", "store", "finish", "refusal"} {
		t.Run(stage, func(t *testing.T) {
			j := &testSelectorJournal{fail: stage}
			mesh := New(Config{Name: "alpha", SelectorJournal: j})
			var calls int
			h := mesh.Handler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				if stage == "refusal" {
					writeError(w, 0, daemon.Errorf(daemon.CodeDeviceInUse, "in use"))
					return
				}
				mockSelectorGrant(w)
			}), false)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, selectorRequest())
			want := daemon.CodeInternal
			if stage == "refusal" {
				want = daemon.CodeDeviceInUse
			}
			if envelopeCode(rec.Body.Bytes()) != want {
				t.Fatal(rec.Body.String())
			}
			if (stage == "finish") != (j.pending != nil) || (calls > 0) != (stage == "finish" || stage == "refusal") {
				t.Fatalf("stage %s calls %d pending %+v", stage, calls, j.pending)
			}
		})
	}
}

func TestPendingSelectorWithUnknownOwnerDoesNotSearchLocally(t *testing.T) {
	pending := &daemon.SelectorAttempt{RequestID: "selector-01", Owner: "absent", AttemptID: "owner-key", ForwardedBody: json.RawMessage(`{"select":{"platform":"mock"}}`)}
	j := &testSelectorJournal{pending: pending}
	m := New(Config{Name: "alpha", SelectorJournal: j})
	h := m.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("allocated locally with an unresolved peer grant") }), false)
	rec := httptest.NewRecorder()
	r := selectorRequest()
	r.Body = io.NopCloser(strings.NewReader(`{"device":"mock-1@alpha"}`))
	h.ServeHTTP(rec, r)
	if envelopeCode(rec.Body.Bytes()) != daemon.CodePeerUnavailable || j.pending != pending {
		t.Fatal(rec.Body.String())
	}
}
