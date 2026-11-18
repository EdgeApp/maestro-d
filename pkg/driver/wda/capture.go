package wda

// maestro-d addition. This file is not part of upstream maestro-runner.
// Screen capture of a physical iPhone or iPad through the QuickTime
// channel (pkg/capture/quicktime): CreateIOSDriver starts the device's
// always-on capture before WebDriverAgent and hands it over with
// SetCapture; the driver gives it out through core.Capturer and records
// --record through it (recording.go). When the capture is lost the device
// has re-enumerated on USB, which also drops WebDriverAgent's connection,
// so steps that then fail with a network error say so. driver.go reaches
// this through its capture fields and one hook in Execute; see
// docs/daemon/UPSTREAM.md.

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"syscall"

	"github.com/devicelab-dev/maestro-runner/pkg/capture"
	"github.com/devicelab-dev/maestro-runner/pkg/core"
)

// captureLostHint is added to a step that fails with a network error once
// the device's capture was lost.
const captureLostHint = "the iPhone re-enumerated when screen capture stopped; detach and attach again"

// SetCapture gives the driver its device's always-on capture session, or
// the *capture.Unavailable error saying why it has none. A lost capture
// marks the driver captureLost.
func (d *Driver) SetCapture(s *capture.Session, err error) {
	d.capMu.Lock()
	defer d.capMu.Unlock()
	if d.capStop != nil {
		d.capStop()
		d.capStop = nil
	}
	d.capErr = err
	d.screen.Set(s)
	if s != nil {
		d.capStop = s.OnState(func(st capture.State, _ string) {
			if st == capture.StateLost {
				d.captureLost.Store(true)
			}
		})
	}
}

// Capture implements core.Capturer: a physical iPhone's or iPad's
// always-on capture session. A simulator answers simulator (it records
// through simctl instead), a TV unsupported-platform.
func (d *Driver) Capture() (*capture.Session, error) {
	switch {
	case d.isTV:
		return nil, &capture.Unavailable{Reason: capture.ReasonUnsupportedPlatform, Message: "tvOS has no screen capture"}
	case d.info != nil && d.info.IsSimulator:
		return nil, &capture.Unavailable{Reason: capture.ReasonSimulator, Message: "iOS simulators have no live screen capture; --record uses simctl"}
	}
	d.capMu.Lock()
	defer d.capMu.Unlock()
	if d.capErr != nil {
		return nil, d.capErr
	}
	s, err := d.screen.Session()
	if err != nil {
		return nil, &capture.Unavailable{Reason: capture.ReasonUnsupportedPlatform, Message: "screen capture is not enabled for this device"}
	}
	if st, reason := s.State(); st == capture.StateLost || st == capture.StateUnavailable {
		return nil, &capture.Unavailable{Reason: reason, Message: fmt.Sprintf("the capture of %s ended", d.udid)}
	}
	return s, nil
}

// startCaptureRecording records a physical device's capture session into
// a temporary file until stopCaptureRecording (capture.DriverCapture).
func (d *Driver) startCaptureRecording() error {
	if _, err := d.Capture(); err != nil {
		return fmt.Errorf("screen recording: %w", err)
	}
	return d.screen.StartRecording()
}

// stopCaptureRecording finalizes the running capture recording at
// hostPath.
func (d *Driver) stopCaptureRecording(hostPath string) error {
	return d.screen.StopRecording(hostPath)
}

// addCaptureLostHint tells a step that failed with a network error after
// the capture was lost why WebDriverAgent is gone.
func (d *Driver) addCaptureLostHint(result *core.CommandResult) {
	if result == nil || result.Success || !d.captureLost.Load() || !isNetworkError(result) {
		return
	}
	if strings.Contains(result.Message, captureLostHint) {
		return
	}
	if result.Message == "" {
		result.Message = captureLostHint
	} else {
		result.Message += " (" + captureLostHint + ")"
	}
	if result.Error != nil {
		result.Error = fmt.Errorf("%w (%s)", result.Error, captureLostHint)
	} else {
		result.Error = errors.New(captureLostHint)
	}
}

// networkSignals are the texts of the errors a dead WebDriverAgent
// connection gives.
var networkSignals = []string{
	"connection refused",
	"connection reset",
	"broken pipe",
	"no route to host",
	"network is unreachable",
	"i/o timeout",
	"dial tcp",
}

// eofWord matches "EOF" as a word ("unexpected EOF", "read: EOF").
var eofWord = regexp.MustCompile(`\beof\b`)

// isNetworkError reports whether a step failed because WebDriverAgent
// could not be reached.
func isNetworkError(result *core.CommandResult) bool {
	if result.Error != nil {
		var ne net.Error
		var op *net.OpError
		if errors.As(result.Error, &ne) || errors.As(result.Error, &op) ||
			errors.Is(result.Error, syscall.ECONNREFUSED) || errors.Is(result.Error, syscall.ECONNRESET) ||
			errors.Is(result.Error, syscall.EPIPE) {
			return true
		}
	}
	text := result.Message
	if result.Error != nil {
		text += " " + result.Error.Error()
	}
	text = strings.ToLower(text)
	for _, s := range networkSignals {
		if strings.Contains(text, s) {
			return true
		}
	}
	return eofWord.MatchString(text)
}
