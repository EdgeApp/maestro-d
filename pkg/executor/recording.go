package executor

// maestro-d addition. This file is not part of upstream maestro-runner.
// The startRecording and stopRecording steps on a driver with screen
// capture (core.Capturer): the executor records the device's capture
// session into the report itself, so every such driver records the same
// way and the file lands where the report's other artifacts are. A driver
// that records its own frames (core.FrameRecorder: the browser) records
// into the same files. Other drivers without capture keep upstream's
// behaviour (the step reaches the driver).
// flow_runner.go reaches this through one case in each step dispatcher and
// a deferred finishRecording; see docs/daemon/UPSTREAM.md.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/capture"
	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
	"github.com/devicelab-dev/maestro-runner/pkg/logger"
)

// recordingsDir is the directory recordings go to, under the output
// directory (the daemon's session report) or the flow's assets directory
// (`maestro-d test`).
const recordingsDir = "recordings"

// stepRecording is a FlowRunner's recording state.
type stepRecording struct {
	// inReport puts recordings under the report directory itself (a
	// daemon session) instead of the flow's assets directory.
	inReport bool
	active   *capture.Recorder
	frames   core.FrameRecorder // the running recording, on a FrameRecorder
	used     map[string]bool    // the file names this runner's recordings took
	name     string             // the running recording's file name
	dest     string             // its absolute path
	rel      string             // its path relative to the report directory
}

// captureSession returns the session of a driver with screen capture. It
// returns nil and a nil error when the driver has none: it is no
// core.Capturer, or it answers that it has no capture (a simulator, an
// unsupported platform, or an error that is no *capture.Unavailable). It
// returns the *capture.Unavailable of a Capturer whose capture failed for
// any other reason (camera-permission, helper-failed, capture-lost, …).
func captureSession(d core.Driver) (*capture.Session, error) {
	c, ok := core.Unwrap(d).(core.Capturer)
	if !ok {
		return nil, nil
	}
	s, err := c.Capture()
	if err == nil && s != nil {
		return s, nil
	}
	var u *capture.Unavailable
	if !errors.As(err, &u) || u.Reason == capture.ReasonSimulator || u.Reason == capture.ReasonUnsupportedPlatform {
		return nil, nil
	}
	return nil, err
}

// recordingName turns a step's path into a recording's file name: the
// directory is dropped, .mp4 is added when missing, and empty means
// "recording".
func recordingName(path string) string {
	name := filepath.Base(strings.ReplaceAll(strings.TrimSpace(path), "\\", "/"))
	if name == "." || name == "/" || name == "" {
		name = "recording"
	}
	if !strings.EqualFold(filepath.Ext(name), ".mp4") {
		name += ".mp4"
	}
	return name
}

// uniqueRecordingName is name, or name with a numeric suffix
// (clip-2.mp4, clip-3.mp4, …) when a recording of this runner already took
// it or a file of that name is already in dir: a second recording never
// replaces the first, whose step's data still names it.
func uniqueRecordingName(dir, name string, used map[string]bool) string {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; ; i++ {
		cand := name
		if i > 1 {
			cand = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		if used[strings.ToLower(cand)] {
			continue
		}
		if _, err := os.Lstat(filepath.Join(dir, cand)); err == nil {
			continue
		}
		return cand
	}
}

// recordingBase is the directory recordings/ is made in, and its path
// relative to the report directory.
func (fr *FlowRunner) recordingBase() (abs, rel string) {
	if fr.rec.inReport || fr.flowWriter == nil {
		return fr.config.OutputDir, ""
	}
	assets := filepath.Dir(fr.flowWriter.RecordingTarget())
	r, err := filepath.Rel(fr.config.OutputDir, assets)
	if err != nil {
		return assets, ""
	}
	return assets, r
}

// executeRecordingStep runs startRecording or stopRecording: through the
// driver's capture session when it has one, else through the driver's
// core.FrameRecorder when it is one, else on the driver. A
// stopRecording while a recording runs always stops it, even when the
// capture has since been lost (the file is saved with the
// capture.ErrEndedEarly warning). A driver whose capture failed (not a
// simulator or an unsupported platform) fails the step with the reason.
func (fr *FlowRunner) executeRecordingStep(step flow.Step) *core.CommandResult {
	start := time.Now()
	st, isStart := step.(*flow.StartRecordingStep)
	name := "stopRecording"
	if isStart {
		name = "startRecording"
	}
	s, err := captureSession(fr.driver)
	var res *core.CommandResult
	switch {
	case !isStart && fr.rec.running():
		res = fr.stopRecording()
	case err != nil:
		res = &core.CommandResult{Success: false, Error: err, Message: fmt.Sprintf("%s needs screen capture: %v", name, err)}
	case s == nil:
		fr2, ok := core.Unwrap(fr.driver).(core.FrameRecorder)
		switch {
		case !ok:
			return fr.driver.Execute(step)
		case isStart:
			res = fr.startFrameRecording(fr2, st.Path)
		default:
			res = fr.stopRecording()
		}
	case isStart:
		res = fr.startRecording(s, st.Path)
	default:
		res = fr.stopRecording()
	}
	res.Duration = time.Since(start)
	return res
}

// running reports whether a recording runs, on either path.
func (r *stepRecording) running() bool { return r.active != nil || r.frames != nil }

func (fr *FlowRunner) startRecording(s *capture.Session, path string) *core.CommandResult {
	return fr.beginRecording(path, func(dir string) error {
		r, err := capture.NewRecorder(s, dir, capture.Options{FPS: capture.DefaultFPS, Audio: true})
		if err == nil {
			fr.rec.active = r
		}
		return err
	})
}

// startFrameRecording starts a FrameRecorder's recording, named and placed
// as a capture session's recording is.
func (fr *FlowRunner) startFrameRecording(d core.FrameRecorder, path string) *core.CommandResult {
	return fr.beginRecording(path, func(string) error {
		if err := d.StartFrameRecording(); err != nil {
			return err
		}
		fr.rec.frames = d
		return nil
	})
}

// beginRecording names a new recording from path, makes its directory and
// calls start with it; start sets the running recorder.
func (fr *FlowRunner) beginRecording(path string, start func(dir string) error) *core.CommandResult {
	if fr.rec.running() {
		err := fmt.Errorf("a recording is already running (%s)", fr.rec.name)
		return &core.CommandResult{Success: false, Error: err, Message: err.Error()}
	}
	base, rel := fr.recordingBase()
	dir := filepath.Join(base, recordingsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return &core.CommandResult{Success: false, Error: err, Message: fmt.Sprintf("startRecording: %v", err)}
	}
	name := uniqueRecordingName(dir, recordingName(path), fr.rec.used)
	if err := start(dir); err != nil {
		return &core.CommandResult{Success: false, Error: err, Message: fmt.Sprintf("startRecording: %v", err)}
	}
	if fr.rec.used == nil {
		fr.rec.used = map[string]bool{}
	}
	fr.rec.used[strings.ToLower(name)] = true
	fr.rec.name = name
	fr.rec.dest = filepath.Join(dir, name)
	fr.rec.rel = filepath.ToSlash(filepath.Join(rel, recordingsDir, name))
	return &core.CommandResult{Success: true, Message: "Recording to " + fr.rec.rel, Data: fr.rec.rel}
}

// FrameRecordingData is stopRecording's data on a core.FrameRecorder (the
// browser): the file, as the other drivers' data names it, with the
// video's length and the frames it holds.
type FrameRecordingData struct {
	Path       string `json:"path"`
	DurationMs int64  `json:"durationMs"`
	Frames     int    `json:"frames"`
}

func (fr *FlowRunner) stopRecording() *core.CommandResult {
	if !fr.rec.running() {
		err := fmt.Errorf("no recording is running")
		return &core.CommandResult{Success: false, Error: err, Message: err.Error()}
	}
	rel := fr.rec.rel
	if fr.rec.frames != nil {
		stats, err := fr.finishFrameRecording()
		if err != nil {
			return &core.CommandResult{Success: false, Error: err, Message: fmt.Sprintf("stopRecording: %v", err), Data: rel}
		}
		data := FrameRecordingData{Path: rel, DurationMs: stats.Duration.Milliseconds(), Frames: stats.Frames}
		msg := fmt.Sprintf("Saved recording %s (%.1f s, %d frames)", rel, stats.Duration.Seconds(), stats.Frames)
		return &core.CommandResult{Success: true, Message: msg, Data: data}
	}
	err := fr.finishRecording()
	if errors.Is(err, capture.ErrEndedEarly) {
		return &core.CommandResult{Success: true, Message: fmt.Sprintf("Saved recording %s (warning: %v)", rel, err), Data: rel}
	}
	if err != nil {
		return &core.CommandResult{Success: false, Error: err, Message: fmt.Sprintf("stopRecording: %v", err), Data: rel}
	}
	return &core.CommandResult{Success: true, Message: "Saved recording " + rel, Data: rel}
}

// finishRecording finalizes a recording left running (the flow, session or
// attachment ended). It is a no-op when none runs. An error wrapping
// capture.ErrEndedEarly means the recording was saved but ended before
// the step or the flow did.
func (fr *FlowRunner) finishRecording() error {
	if fr.rec.frames != nil {
		_, err := fr.finishFrameRecording()
		return err
	}
	r, dest, rel := fr.rec.active, fr.rec.dest, fr.rec.rel
	if r == nil {
		return nil
	}
	fr.rec.active = nil
	err := r.Stop(dest)
	recordingSaved("recording "+rel, err)
	if err == nil {
		logger.Info("Saved recording %s", dest)
	}
	return err
}

// finishFrameRecording ends a FrameRecorder's running recording and
// encodes it at its destination.
func (fr *FlowRunner) finishFrameRecording() (core.RecordingStats, error) {
	d, dest, rel := fr.rec.frames, fr.rec.dest, fr.rec.rel
	fr.rec.frames = nil
	stats, err := d.StopFrameRecording(dest)
	recordingSaved("recording "+rel, err)
	if err == nil {
		logger.Info("Saved recording %s (%d frames, %s)", dest, stats.Frames, stats.Duration.Round(time.Millisecond))
	}
	return stats, err
}

// recordingSaved reports whether a recording's Stop error still means the
// file was saved: nil, or capture.ErrEndedEarly (logged as a warning). Any
// other error is logged as a failed save.
func recordingSaved(what string, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, capture.ErrEndedEarly):
		logger.Warn("%s: saved, but %v", what, err)
		return true
	}
	logger.Warn("%s: failed to save the recording: %v", what, err)
	return false
}
