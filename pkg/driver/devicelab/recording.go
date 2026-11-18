package devicelab

// maestro-d: screen capture. CreateAndroidDriver hands the driver its
// device's capture session (pkg/capture/scrcpy, on demand) through
// SetCapture. The session gives the driver core.Capturer (live streams),
// and --record (core.ScreenRecorder) records through it with the same
// Recorder the streams use: no three-minute cap, audio where the device
// has it, the file written on the host. The startRecording and
// stopRecording steps run in the executor through Capture(); screenrecord
// is gone. See docs/daemon/UPSTREAM.md.

import (
	"fmt"

	"github.com/devicelab-dev/maestro-runner/pkg/capture"
	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

// SetCapture hands the driver its device's capture session. The caller
// owns the session and closes it after the driver.
func (d *Driver) SetCapture(s *capture.Session) { d.screen.Set(s) }

// Capture implements core.Capturer.
func (d *Driver) Capture() (*capture.Session, error) { return d.screen.Session() }

// StartScreenRecording implements core.ScreenRecorder: it records the
// capture session into a temporary file.
func (d *Driver) StartScreenRecording() error { return d.screen.StartRecording() }

// StopScreenRecording implements core.ScreenRecorder: it finalizes the
// recording at hostPath.
func (d *Driver) StopScreenRecording(hostPath string) error {
	return d.screen.StopRecording(hostPath)
}

// recordingStep answers startRecording and stopRecording when they reach
// the driver. The executor runs them through Capture(), so this happens
// only when the driver was given no capture session.
func (d *Driver) recordingStep(step flow.Step) *core.CommandResult {
	if _, err := d.Capture(); err != nil {
		return errorResult(err, fmt.Sprintf("%s needs screen capture: %v", step.Type(), err))
	}
	err := fmt.Errorf("%s runs in the executor, through the driver's screen capture", step.Type())
	return errorResult(err, err.Error())
}
