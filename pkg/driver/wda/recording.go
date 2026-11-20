package wda

import (
	"fmt"

	"github.com/devicelab-dev/maestro-runner/pkg/simulator"
)

// --record support (core.ScreenRecorder). On a simulator capture happens
// host-side via `simctl io recordVideo`. maestro-d: a physical iPhone or
// iPad records its screen capture session (capture.go), which
// CreateIOSDriver starts through the QuickTime channel on macOS.

// StartScreenRecording implements core.ScreenRecorder.
func (d *Driver) StartScreenRecording() error {
	if d.info == nil || !d.info.IsSimulator {
		return d.startCaptureRecording() // maestro-d
	}
	if d.recording != nil {
		return fmt.Errorf("a recording is already in progress")
	}
	rec, err := simulator.StartRecording(d.udid)
	if err != nil {
		return err
	}
	d.recording = rec
	return nil
}

// StopScreenRecording implements core.ScreenRecorder.
func (d *Driver) StopScreenRecording(hostPath string) error {
	if d.info == nil || !d.info.IsSimulator {
		return d.stopCaptureRecording(hostPath) // maestro-d
	}
	if d.recording == nil {
		return fmt.Errorf("no recording in progress")
	}
	rec := d.recording
	d.recording = nil
	return rec.Stop(hostPath)
}
