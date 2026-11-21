package executor

// maestro-d: undoing, when a session ends, what it set on the device's
// network: a traffic shape has no expiry, and airplane mode outlives the
// session that turned it on.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
	"github.com/devicelab-dev/maestro-runner/pkg/logger"
	"github.com/devicelab-dev/maestro-runner/pkg/netshape"
)

// cleanupBound bounds each undo, so the whole cleanup stays inside a hub's
// lease-shutdown wait (10 s) and well under `stop`'s 30 s.
var cleanupBound = 4 * time.Second

// sessionUndo is what a session set that its end must undo.
type sessionUndo struct {
	shaped   bool // a setTrafficShape call was sent, and no reset since
	airplane bool // setAirplaneMode or toggleAirplaneMode reached the driver
}

// noteSet records a setTrafficShape that may have reached the shaper: any
// outcome but "nothing was sent" counts, since a failed call may still
// have applied.
func (u *sessionUndo) noteSet(err error) {
	var config *netshape.ConfigError
	var value *netshape.ValueError
	if errors.Is(err, netshape.ErrNotConfigured) || errors.As(err, &config) || errors.As(err, &value) {
		return // nothing was sent
	}
	u.shaped = true
}

// Cleanup undoes what the session set: Android airplane mode off, then the
// device's resetTrafficShape call, each bounded at cleanupBound and run on
// ctx rather than the session's (by now cancelled) base context. A failure
// is logged and never stops the rest of the teardown. The daemon calls it
// after cancelling the attach and before Close.
func (s *Session) Cleanup(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	// No step runs after this, so none queued behind the lock can redo what
	// the cleanup undoes before the session closes.
	s.ending = true
	fr := s.fr
	if !fr.undo.airplane && !(fr.undo.shaped && fr.shaper != nil) {
		return // nothing to undo: the common case, with no driver call
	}
	// The teardown runs outside any step, so nothing else would contain a
	// driver panic here: each part recovers its own, logs it, and the rest
	// of the teardown goes on.
	id := s.cleanupDeviceID()
	if fr.undo.airplane {
		fr.undo.airplane = false
		guardCleanup(id, "airplane mode off", func() { s.cleanupAirplane(ctx, id) })
	}
	if fr.undo.shaped && fr.shaper != nil {
		fr.undo.shaped = false
		guardCleanup(id, "resetTrafficShape", func() {
			rctx, cancel := context.WithTimeout(ctx, cleanupBound)
			defer cancel()
			if _, err := fr.shaper.Reset(rctx); err != nil {
				logger.Warn("session end: %s: resetTrafficShape failed: %v", id, err)
			} else {
				logger.Info("session end: %s: traffic shape reset", id)
			}
		})
	}
}

// cleanupDeviceID names the device in the cleanup's log lines.
func (s *Session) cleanupDeviceID() (id string) {
	id = "device"
	defer func() {
		if r := recover(); r != nil {
			logger.Error("session end: GetPlatformInfo panicked: %v", r)
		}
	}()
	if info := s.fr.driver.GetPlatformInfo(); info != nil && info.DeviceID != "" {
		id = info.DeviceID
	}
	return id
}

// guardCleanup runs one part of the cleanup, containing a panic.
func guardCleanup(id, what string, f func()) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error("session end: %s: %s panicked: %v", id, what, r)
		}
	}()
	f()
}

// cleanupAirplane turns airplane mode off. The Android drivers run it
// through adb with no context, so the bound is enforced here: a call still
// running when it ends is abandoned, and ends when adb returns.
func (s *Session) cleanupAirplane(ctx context.Context, id string) {
	fr := s.fr
	actx, cancel := context.WithTimeout(ctx, cleanupBound)
	defer cancel()
	fr.driver.SetContext(actx)
	defer fr.driver.SetContext(s.baseCtx)
	done := make(chan *core.CommandResult, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- &core.CommandResult{Success: false, Message: fmt.Sprintf("driver panic: %v", r)}
			}
		}()
		done <- fr.driver.Execute(&flow.SetAirplaneModeStep{BaseStep: flow.BaseStep{StepType: flow.StepSetAirplaneMode}, Enabled: false})
	}()
	select {
	case r := <-done:
		if r == nil || !r.Success {
			logger.Warn("session end: %s: airplane mode off failed: %v", id, resultMessage(r))
			return
		}
		logger.Info("session end: %s: airplane mode off", id)
	case <-actx.Done():
		logger.Warn("session end: %s: airplane mode off did not answer within %s; abandoned", id, cleanupBound)
	}
}

func resultMessage(r *core.CommandResult) string {
	if r == nil {
		return "no result"
	}
	if r.Message != "" {
		return r.Message
	}
	if r.Error != nil {
		return r.Error.Error()
	}
	return "failed"
}
