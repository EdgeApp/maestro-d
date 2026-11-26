package hub

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

// SelectorJournal is the entry hub's part of the durable lease state.
type SelectorJournal interface {
	SelectorOutcome(requestID string) (*daemon.AcquisitionReplay, *daemon.SelectorAttempt, error)
	StoreSelectorAttempt(daemon.SelectorAttempt) error
	FinishSelectorAttempt(requestID string, replay *daemon.AcquisitionReplay) error
}

type selectorCall struct {
	done   chan struct{}
	result *recorder
}

// A retry arriving over another connection joins the same decision, including
// the first caller's choice of peer. Disk writes never hold the mesh lock.
func (rt *router) keyedAcquisition(w http.ResponseWriter, r *http.Request, body []byte) {
	id := r.Header.Get(daemon.HeaderRequestID)
	m := rt.m
	m.selectorMu.Lock()
	if call := m.selectors[id]; call != nil {
		m.selectorMu.Unlock()
		select {
		case <-call.done:
			if call.result.status == http.StatusOK {
				w.Header().Set(daemon.HeaderReplayed, "true")
			}
			call.result.copyTo(w)
		case <-r.Context().Done():
		}
		return
	}
	if m.selectors == nil {
		m.selectors = make(map[string]*selectorCall)
	}
	call := &selectorCall{done: make(chan struct{}), result: newRecorder()}
	m.selectors[id] = call
	m.selectorMu.Unlock()

	defer func() {
		m.selectorMu.Lock()
		delete(m.selectors, id)
		close(call.done)
		m.selectorMu.Unlock()
	}()
	rec := call.result
	replay, pending, err := m.cfg.SelectorJournal.SelectorOutcome(id)
	switch {
	case err != nil:
		journalFailure(rec, err)
	case replay != nil:
		rt.writeAcquisitionReplay(rec, replay)
	default:
		if pending != nil {
			resumed, finished := rt.dispatchSelector(r, *pending, false)
			if finished {
				resumed.copyTo(rec)
				break
			}
			// A definitive refusal lets the original selector try another hub.
			// A redelivery may supply a different body for the same request ID.
			if len(pending.OriginalBody) > 0 {
				body = pending.OriginalBody
			}
			if pending.Path != "" {
				r = r.Clone(r.Context())
				r.URL.Path = pending.Path
				r.URL.RawPath = ""
			}
		}
		rt.routeAcquisition(rec, r, "", body, true)
	}
	if rec.status == 0 {
		writeError(rec, 0, daemon.Errorf(daemon.CodePeerUnavailable, "acquisition interrupted"))
	}

	rec.copyTo(w)
}

func (rt *router) writeAcquisitionReplay(w http.ResponseWriter, replay *daemon.AcquisitionReplay) {
	w.Header().Set(daemon.HeaderReplayed, "true")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(replay.Status)
	_, _ = w.Write(qualifyJSON(replay.Response, rt.m.cfg.Name, false))
}

func journalFailure(rec http.ResponseWriter, err error) {
	var de *daemon.Error
	if !errors.As(err, &de) {
		de = daemon.WrapErr(daemon.CodeInternal, err)
	}
	writeError(rec, 0, de)
}

// Dispatch the stored bytes, even when fresh discovery would choose another
// device. An incomplete response leaves the attempt pending for the next retry.
// finished is false only when normal candidate selection may continue.
func (rt *router) dispatchSelector(r *http.Request, attempt daemon.SelectorAttempt, fresh bool) (*recorder, bool) {
	journal := rt.m.cfg.SelectorJournal
	failed := func(err error) (*recorder, bool) {
		rec := newRecorder()
		journalFailure(rec, err)
		return rec, true
	}
	if fresh {
		if err := journal.StoreSelectorAttempt(attempt); err != nil {
			return failed(err)
		}
	}
	tr := withBody(r, attempt.ForwardedBody)
	if attempt.Path != "" {
		tr.URL.Path = attempt.Path
		tr.URL.RawPath = ""
	}
	tr.Header.Set(daemon.HeaderRequestID, attempt.AttemptID)
	rec := newRecorder()
	if attempt.Owner == rt.m.cfg.Name {
		rt.serveLocal(rec, tr)
	} else {
		link, e := rt.m.link(attempt.Owner)
		if e != nil {
			return failed(daemon.Errorf(daemon.CodePeerUnavailable, "hub %s is unavailable; its lease outcome is pending", attempt.Owner).WithDetails("hub", attempt.Owner))
		}
		rt.forwardOn(rec, tr, attempt.Owner, link)
	}
	var response daemon.LeaseResult
	decoded := json.Unmarshal(rec.buf.Bytes(), &response) == nil
	if rec.status == http.StatusOK && decoded && response.OK && response.Lease.ID != "" && response.Lease.Device != "" {
		replay := &daemon.AcquisitionReplay{RequestID: attempt.RequestID, LeaseID: response.Lease.ID, Device: response.Lease.Device,
			Status: rec.status, CompletedAt: time.Now().UTC(), Response: rec.buf.Bytes()}
		if err := journal.FinishSelectorAttempt(attempt.RequestID, replay); err != nil {
			return failed(err)
		}
		return rec, true
	}
	if !decoded || response.Error == nil || rec.status >= 500 || response.Error.Code == daemon.CodePeerUnavailable || response.Error.Code == daemon.CodeDaemonUnavailable {
		return failed(daemon.Errorf(daemon.CodePeerUnavailable, "lease outcome at hub %s is unknown; retry with the same request id", attempt.Owner).WithDetails("hub", attempt.Owner))
	}
	if err := journal.FinishSelectorAttempt(attempt.RequestID, nil); err != nil {
		return failed(err)
	}
	return rec, false
}
