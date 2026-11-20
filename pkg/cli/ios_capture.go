package cli

// maestro-d addition. This file is not part of upstream maestro-runner.
// Step 0 of CreateIOSDriver: a physical iPhone's or iPad's screen capture
// through the QuickTime channel (pkg/capture/quicktime) starts before
// anything else touches the device, because starting it re-enumerates the
// device on USB and drops every usbmux connection (WebDriverAgent's
// included). Capture runs only when RunConfig.ScreenCapture asks for it:
// the daemon's attach always does, `maestro-d test` only when it records
// (--record/--video, or a startRecording step anywhere in its flows). A
// capture that fails never fails the attach: the driver answers why
// through core.Capturer. ios.go reaches this through three hooks; see
// docs/daemon/UPSTREAM.md.

import (
	"context"
	"runtime"
	"time"

	goios "github.com/danielpaulus/go-ios/ios"
	"github.com/devicelab-dev/maestro-runner/pkg/capture"
	"github.com/devicelab-dev/maestro-runner/pkg/capture/quicktime"
	"github.com/devicelab-dev/maestro-runner/pkg/core"
	wdadriver "github.com/devicelab-dev/maestro-runner/pkg/driver/wda"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
	"github.com/devicelab-dev/maestro-runner/pkg/logger"
)

// iosCaptureDevice is a device's capture on the process's helper
// (*quicktime.Device).
type iosCaptureDevice interface {
	Session() *capture.Session
	Wait(ctx context.Context) quicktime.Result
	Close()
}

// Seams the tests replace.
var (
	iosCaptureGOOS = runtime.GOOS
	iosListDevices = goios.ListDevices
	iosGetValues   = goios.GetValues
	iosCaptureOpen = func(udid, name string) iosCaptureDevice { return quicktime.Open(udid, name) }
	iosCaptureNow  = time.Now
	iosCaptureWait = time.Sleep
)

// The re-listing wait after a capture start re-enumerated the device.
var (
	iosRelistPoll    = 250 * time.Millisecond
	iosRelistTimeout = 15 * time.Second
	iosRelistSettle  = 1500 * time.Millisecond
)

// iosCapture is step 0's outcome for one device.
type iosCapture struct {
	udid string
	dev  iosCaptureDevice // nil when capture never started
	err  error            // why the device has no capture, or nil
	kept bool
}

// startIOSCapture is CreateIOSDriver's step 0. It returns nil when the
// run does not capture (the driver then answers that capture is off).
func startIOSCapture(cfg *RunConfig, udid string, isSimulator bool) *iosCapture {
	if !cfg.ScreenCapture || isSimulator {
		return nil
	}
	c := &iosCapture{udid: udid}
	if iosCaptureGOOS != "darwin" {
		c.err = &capture.Unavailable{Reason: capture.ReasonNotMacOS, Message: "iOS screen capture needs a macOS host"}
		return c
	}
	entry, ok, err := usbEntry(udid)
	switch {
	case err != nil:
		c.err = &capture.Unavailable{Reason: capture.ReasonNoCaptureDevice, Message: "usbmuxd: " + err.Error()}
		return c
	case !ok:
		c.err = &capture.Unavailable{Reason: capture.ReasonNoCaptureDevice, Message: "not connected over USB"}
		logger.Info("capture %s: skipped, not connected over USB", udid)
		return c
	}
	values, err := iosGetValues(entry)
	if err != nil {
		c.err = &capture.Unavailable{Reason: capture.ReasonNoCaptureDevice, Message: "lockdown: " + err.Error()}
		return c
	}
	switch class := values.Value.DeviceClass; class {
	case "iPhone", "iPad":
	default:
		c.err = &capture.Unavailable{Reason: capture.ReasonUnsupportedPlatform, Message: "screen capture supports iPhones and iPads, not " + class}
		logger.Info("capture %s: skipped, device class %q", udid, class)
		return c
	}

	printSetupStep("Starting screen capture...")
	c.dev = iosCaptureOpen(udid, values.Value.DeviceName)
	res := c.dev.Wait(context.Background())
	if res.Err != nil {
		c.err = res.Err
		c.dev.Close()
		c.dev = nil
		logger.Warn("capture %s: %v; the device attaches without screen capture", udid, res.Err)
		printSetupWarning("Screen capture unavailable: " + res.Err.Error())
	} else {
		printSetupSuccess("Screen capture started (match: " + res.Match + ")")
	}
	if res.Found {
		waitForRelisting(udid, res.At)
	}
	return c
}

// usbEntry finds the device's usbmux entry over USB.
func usbEntry(udid string) (goios.DeviceEntry, bool, error) {
	list, err := iosListDevices()
	if err != nil {
		return goios.DeviceEntry{}, false, err
	}
	for _, d := range list.DeviceList {
		if d.Properties.SerialNumber == udid && d.Properties.ConnectionType == "USB" {
			return d, true, nil
		}
	}
	return goios.DeviceEntry{}, false, nil
}

// waitForRelisting waits until usbmux lists the device again after the
// capture start re-enumerated it, and at least iosRelistSettle has passed
// since the status that said so, before anything opens a connection.
func waitForRelisting(udid string, at time.Time) {
	deadline := iosCaptureNow().Add(iosRelistTimeout)
	for {
		if _, ok, _ := usbEntry(udid); ok {
			break
		}
		if !iosCaptureNow().Before(deadline) {
			logger.Warn("capture %s: usbmuxd did not list the device again within %s", udid, iosRelistTimeout)
			break
		}
		iosCaptureWait(iosRelistPoll)
	}
	if rest := iosRelistSettle - iosCaptureNow().Sub(at); rest > 0 {
		iosCaptureWait(rest)
	}
}

// session is the capture session, or nil.
func (c *iosCapture) session() *capture.Session {
	if c == nil || c.dev == nil {
		return nil
	}
	return c.dev.Session()
}

// close ends the device's capture session unless the driver kept it.
func (c *iosCapture) closeUnlessKept() {
	if c != nil && !c.kept {
		c.close()
	}
}

func (c *iosCapture) close() {
	if c != nil && c.dev != nil {
		c.dev.Close()
	}
}

// bind hands the capture to the WebDriverAgent driver and orders cleanup:
// WebDriverAgent first, then the capture session (stopping it can
// re-enumerate the device, and nothing is connected by then).
func (c *iosCapture) bind(driver core.Driver, cleanup func()) func() {
	if c == nil {
		return cleanup
	}
	if wd, ok := core.Unwrap(driver).(*wdadriver.Driver); ok {
		wd.SetCapture(c.session(), c.err)
	}
	c.kept = true
	return func() {
		cleanup()
		c.close()
	}
}

// flowsRecord reports whether any flow can record: a startRecording step
// anywhere in its step trees (the steps, the inline commands of runFlow,
// repeat and retry, and the onFlowStart/onFlowComplete hooks), or a
// runFlow or retry that names a file, which is parsed only when it runs.
func flowsRecord(flows []flow.Flow) bool {
	for _, f := range flows {
		if stepsRecord(f.Steps) || stepsRecord(f.Config.OnFlowStart) || stepsRecord(f.Config.OnFlowComplete) {
			return true
		}
	}
	return false
}

func stepsRecord(steps []flow.Step) bool {
	for _, s := range steps {
		switch st := s.(type) {
		case *flow.StartRecordingStep:
			return true
		case *flow.RunFlowStep:
			if st.File != "" || st.ElseFile != "" || stepsRecord(st.Steps) || stepsRecord(st.ElseSteps) {
				return true
			}
		case *flow.RepeatStep:
			if stepsRecord(st.Steps) {
				return true
			}
		case *flow.RetryStep:
			if st.File != "" || stepsRecord(st.Steps) {
				return true
			}
		}
	}
	return false
}
