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
	mu      sync.Mutex
	pending *daemon.SelectorAttempt
	replay  *daemon.AcquisitionReplay
	fail    string
}

func (j *testSelectorJournal) SelectorOutcome(string) (*daemon.AcquisitionReplay, *daemon.SelectorAttempt, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.fail == "lookup" {
		return nil, nil, errors.New("read failed")
	}
	return j.replay, j.pending, nil
}
func (j *testSelectorJournal) StoreSelectorAttempt(a daemon.SelectorAttempt) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.fail == "store" {
		return errors.New("write failed")
	}
	j.pending = &a
	return nil
}
func (j *testSelectorJournal) FinishSelectorAttempt(id string, r *daemon.AcquisitionReplay) error {
	j.mu.Lock()
	defer j.mu.Unlock()
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

func TestBarePeerAcquisitionReplaysBeforeLocalDiscovery(t *testing.T) {
	journal := &testSelectorJournal{}
	owner, entry := twoHubs(t, func(c *Config, _ *daemon.Config) { c.SelectorJournal = journal })
	for _, route := range []string{"/v1/leases", "/v1/reservations"} {
		_ = journal.FinishSelectorAttempt("", nil)
		body := `{"device":"a1","ttlSec":60}`
		if route == "/v1/reservations" {
			body = `{"device":"a1","ttlSec":60,"clientTime":"` + time.Now().UTC().Format(time.RFC3339) + `","startAt":"` + time.Now().Add(10*time.Minute).UTC().Format(time.RFC3339) + `"}`
		}
		owner.setHook(func(_ http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path == route && r.Method == http.MethodPost {
				_, a, _ := journal.SelectorOutcome(route)
				if a == nil || a.Owner != owner.name || a.AttemptID != r.Header.Get(daemon.HeaderRequestID) || a.Path != route {
					t.Errorf("owner received request before journaled dispatch: %+v", a)
				}
			}
			return false
		})
		code, data := entry.do(t, http.MethodPost, route, body, daemon.HeaderRequestID, route)
		replay, _, _ := journal.SelectorOutcome(route)
		if code != http.StatusOK || replay == nil || replay.Device != "a1@hub-a" {
			t.Fatalf("first grant: %d %s", code, data)
		}
		fresh := New(Config{Name: entry.name, SelectorJournal: journal})
		req := httptest.NewRequest(http.MethodPost, route, strings.NewReader(body))
		req.Header.Set(daemon.HeaderRequestID, route)
		rec := httptest.NewRecorder()
		fresh.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Fatal("retried grant reached local discovery")
		}), false).ServeHTTP(rec, req)
		var result daemon.LeaseResult
		if rec.Header().Get(daemon.HeaderReplayed) != "true" || json.Unmarshal(rec.Body.Bytes(), &result) != nil || result.Lease.ID != replay.LeaseID || result.Lease.Device != "a1@hub-a" {
			t.Fatalf("retry: %d %s", rec.Code, rec.Body)
		}
	}
}
