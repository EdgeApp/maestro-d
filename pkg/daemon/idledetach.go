package daemon

// maestro-d: the per-device automatic detaches. A driver that asks for it
// (core.IdleTimer: the computer device, 5 minutes by default) is detached
// once no call has held it for that long, which closes its connection; one
// whose connection ends on its own (core.ConnectionEnder) is detached then.
// This is separate from the daemon's own idle shutdown (idle.go).

import (
	"fmt"
	"sync/atomic"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/logger"
)

// Variables so tests can drive the clock.
var (
	idleSweep = 10 * time.Second
	idleNow   = time.Now
)

// touch records a call that holds the device.
func (a *attached) touch() { a.lastUsed.Store(idleNow().UnixNano()) }

// watchIdle starts the sweep for an attachment whose driver has an idle
// timeout; it ends with the attachment (a.base).
func (s *Server) watchIdle(a *attached, driver core.Driver) {
	it, ok := core.Unwrap(driver).(core.IdleTimer)
	if !ok || it.IdleTimeout() <= 0 {
		return
	}
	timeout := it.IdleTimeout()
	a.touch()
	go func() {
		t := time.NewTicker(idleSweep)
		defer t.Stop()
		for {
			select {
			case <-a.base.Done():
				return
			case <-t.C:
			}
			if atomic.LoadInt32(&a.inFlite) > 0 {
				continue
			}
			if idleNow().Sub(time.Unix(0, a.lastUsed.Load())) > timeout && s.idleDetach(a, timeout) {
				return
			}
		}
	}()
}

// idleDetach detaches a through the normal path. It reports false when a
// call holds the device now, so the sweep looks again; true when the
// attachment is gone, by this detach or another. It holds the device's busy
// lock until the connection is closed, so a call that arrives meanwhile
// waits and then finds the session closed instead of losing its connection
// mid-step.
func (s *Server) idleDetach(a *attached, timeout time.Duration) bool {
	if !a.busy.TryLock() {
		return false
	}
	defer a.busy.Unlock()
	s.autoDetach(a, fmt.Sprintf("no call for %s", timeout), "idle")
	return true
}

// watchConnection detaches an attachment whose driver's connection ends on
// its own, so the next call answers DEVICE_NOT_ATTACHED (and the one-shot
// CLI attaches again) instead of every call failing on a dead connection.
func (s *Server) watchConnection(a *attached, driver core.Driver) {
	ce, ok := core.Unwrap(driver).(core.ConnectionEnder)
	if !ok {
		return
	}
	go func() {
		select {
		case <-a.base.Done():
			return
		case <-ce.Done():
		}
		// A step in flight fails at once on the closed connection; let it
		// answer first.
		a.busy.Lock()
		defer a.busy.Unlock()
		s.autoDetach(a, "the connection closed", "connection closed")
	}()
}

// autoDetach detaches a, when it is still attached, through the normal path,
// with message on the device.detached event. Caller holds a.busy.
func (s *Server) autoDetach(a *attached, why, message string) {
	b := s.takeAttachedIf(a)
	if b == nil {
		return
	}
	defer s.finishDetach(b)
	logger.Info("Detaching %s: %s", a.id, why)
	b.close()
	s.registry.MarkDetached(a.id)
	s.events.Publish(Event{Type: "device.detached", Device: a.id, Message: message})
}

// takeAttachedIf is takeAttached for a only: nil when the id is no longer
// attached as a.
func (s *Server) takeAttachedIf(a *attached) *attached {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.attached[a.id] != a {
		return nil
	}
	return s.takeAttachedLocked(a.id)
}
