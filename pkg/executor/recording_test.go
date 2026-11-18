package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/capture"
	"github.com/devicelab-dev/maestro-runner/pkg/capture/fake"
	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/driver/mock"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
	"github.com/devicelab-dev/maestro-runner/pkg/report"
)

// checkMP4 checks a finished recording: an MP4 that ends with its mfra,
// and, when ffprobe is installed (required in CI), plays as H.264.
func checkMP4(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 16 || string(data[4:8]) != "ftyp" || !bytes.Contains(data[len(data)-64:], []byte("mfro")) {
		t.Fatalf("%s is not a finished fragmented MP4 (%d bytes)", path, len(data))
	}
	probe, err := exec.LookPath("ffprobe")
	if err != nil {
		if os.Getenv("MAESTRO_D_REQUIRE_FFPROBE") == "1" {
			t.Fatal("ffprobe is required")
		}
		return
	}
	out, err := exec.Command(probe, "-v", "error", "-show_entries", "stream=codec_name", "-of", "csv=p=0", path).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "h264") {
		t.Fatalf("ffprobe %s: %v\n%s", path, err, out)
	}
}

func TestRecordingName(t *testing.T) {
	for in, want := range map[string]string{
		"": "recording.mp4", "  ": "recording.mp4", "clip": "clip.mp4", "clip.mp4": "clip.mp4", "clip.MP4": "clip.MP4",
		"/sdcard/x/demo": "demo.mp4", "a/b/c.mp4": "c.mp4", `C:\tmp\win`: "win.mp4", "dir/": "dir.mp4", "/": "recording.mp4",
		"take.1": "take.1.mp4",
	} {
		if got := recordingName(in); got != want {
			t.Errorf("recordingName(%q) = %q, want %q", in, got, want)
		}
	}
}

// U26, daemon: a session records into <report>/recordings/, Data relative
// to the report directory; a second start is refused; Close finalizes.
func TestRecording_SessionSteps(t *testing.T) {
	s, dir := newTestSession(t, SessionConfig{}, mock.Config{})
	ctx := context.Background()
	out := s.Execute(ctx, mustStep(t, "startRecording: some/dir/clip"))
	if !out.Success || out.Data != "recordings/clip.mp4" {
		t.Fatalf("start: %+v %v", out.Error, out.Data)
	}
	out = s.Execute(ctx, mustStep(t, "startRecording: other"))
	if out.Success || out.Error == nil || out.Error.Message != "a recording is already running (clip.mp4)" {
		t.Fatalf("second start: %+v", out.Error)
	}
	time.Sleep(1200 * time.Millisecond)
	out = s.Execute(ctx, mustStep(t, "stopRecording: ignored.mp4"))
	if !out.Success || out.Data != "recordings/clip.mp4" {
		t.Fatalf("stop: %+v %v", out.Error, out.Data)
	}
	checkMP4(t, filepath.Join(dir, "recordings", "clip.mp4"))
	if out = s.Execute(ctx, mustStep(t, "stopRecording")); out.Success || !strings.Contains(out.Error.Message, "no recording is running") {
		t.Fatalf("stop again: %+v", out.Error)
	}
	// A recording left running ends with the session.
	if out = s.Execute(ctx, mustStep(t, `startRecording: ""`)); !out.Success || out.Data != "recordings/recording.mp4" {
		t.Fatalf("start unnamed: %+v %v", out.Error, out.Data)
	}
	time.Sleep(500 * time.Millisecond)
	s.Close()
	checkMP4(t, filepath.Join(dir, "recordings", "recording.mp4"))
}

// Review 2.harness-review.7: two recordings with the same file name in a
// session (no name twice, or two paths whose directories are dropped) do
// not overwrite each other: the second gets a numeric suffix, and each
// step's data names its own file.
func TestRecording_SameNameGetsASuffix(t *testing.T) {
	s, dir := newTestSession(t, SessionConfig{}, mock.Config{})
	ctx := context.Background()
	var got []string
	for _, step := range []string{"startRecording: /sdcard/a/x.mp4", "startRecording: /sdcard/b/x.mp4", "startRecording", "startRecording", "startRecording: X.MP4"} {
		out := s.Execute(ctx, mustStep(t, step))
		if !out.Success {
			t.Fatalf("%s: %+v", step, out.Error)
		}
		time.Sleep(300 * time.Millisecond)
		stop := s.Execute(ctx, mustStep(t, "stopRecording"))
		if !stop.Success || stop.Data != out.Data {
			t.Fatalf("stop after %s: %+v %v", step, stop.Error, stop.Data)
		}
		got = append(got, fmt.Sprint(out.Data))
	}
	want := []string{"recordings/x.mp4", "recordings/x-2.mp4", "recordings/recording.mp4", "recordings/recording-2.mp4", "recordings/X-3.MP4"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("data = %v, want %v", got, want)
	}
	for _, rel := range want {
		checkMP4(t, filepath.Join(dir, filepath.FromSlash(rel)))
	}
}

func TestUniqueRecordingName(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "old.mp4"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		used map[string]bool
		want string
	}{
		{"new.mp4", nil, "new.mp4"},
		{"old.mp4", nil, "old-2.mp4"}, // a file already there
		{"a.mp4", map[string]bool{"a.mp4": true, "a-2.mp4": true}, "a-3.mp4"},
		{"take.1.mp4", map[string]bool{"take.1.mp4": true}, "take.1-2.mp4"},
	} {
		if got := uniqueRecordingName(dir, c.name, c.used); got != c.want {
			t.Errorf("uniqueRecordingName(%q, %v) = %q, want %q", c.name, c.used, got, c.want)
		}
	}
}

// The recording steps inside a repeat (the nested dispatcher) and a
// runFlow of the session go the same way.
func TestRecording_NestedSteps(t *testing.T) {
	s, dir := newTestSession(t, SessionConfig{}, mock.Config{StepDelay: time.Second})
	out := s.Execute(context.Background(), mustStep(t, "repeat:\n  times: 1\n  commands:\n    - startRecording: nested\n    - tapOn: x\n    - stopRecording"))
	if !out.Success {
		t.Fatalf("repeat: %+v", out.Error)
	}
	checkMP4(t, filepath.Join(dir, "recordings", "nested.mp4"))
}

// U26, `maestro-d test`: the flow's assets directory, and the end of the
// flow finalizes a recording left running.
func TestRecording_FlowRunner(t *testing.T) {
	dir := t.TempDir()
	drv := mock.New(mock.Config{StepDelay: time.Second})
	defer drv.Close()
	runner := New(drv, RunnerConfig{OutputDir: dir, Artifacts: ArtifactNever, DriverName: "mock"})
	f := flow.Flow{SourcePath: "rec.yaml", Config: flow.Config{Name: "rec"}, Steps: []flow.Step{
		&flow.StartRecordingStep{BaseStep: flow.BaseStep{StepType: flow.StepStartRecording}, Path: "first"},
		&flow.WaitForAnimationToEndStep{BaseStep: flow.BaseStep{StepType: flow.StepWaitForAnimationToEnd}},
		&flow.StopRecordingStep{BaseStep: flow.BaseStep{StepType: flow.StepStopRecording}},
		&flow.StartRecordingStep{BaseStep: flow.BaseStep{StepType: flow.StepStartRecording}, Path: "left"},
	}}
	res, err := runner.Run(context.Background(), []flow.Flow{f})
	if err != nil || res.Status != report.StatusPassed {
		t.Fatalf("run: %v %+v", err, res)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "assets", "*", "recordings", "*.mp4"))
	if len(matches) != 2 {
		t.Fatalf("recordings: %v", matches)
	}
	for _, m := range matches {
		checkMP4(t, m)
	}

	// Data is relative to the report directory: assets/<flow>/recordings/.
	detail := &report.FlowDetail{ID: "flow-x"}
	fr := &FlowRunner{driver: drv, config: RunnerConfig{OutputDir: dir}, flowWriter: report.NewFlowWriter(detail, dir, nil)}
	r := fr.executeRecordingStep(&flow.StartRecordingStep{Path: "x"})
	if !r.Success || r.Data != "assets/flow-x/recordings/x.mp4" {
		t.Fatalf("start: %+v", r)
	}
	if r = fr.executeRecordingStep(&flow.StopRecordingStep{}); !r.Success {
		t.Fatalf("stop: %+v", r)
	}
	checkMP4(t, filepath.Join(dir, "assets", "flow-x", "recordings", "x.mp4"))
}

// recordingDriver is a driver without capture that counts the recording
// steps reaching it: upstream's behaviour.
type recordingDriver struct {
	core.Driver
	got []flow.StepType
}

func (d *recordingDriver) Execute(step flow.Step) *core.CommandResult {
	d.got = append(d.got, step.Type())
	return d.Driver.Execute(step)
}

// capturelessDriver answers that it has no capture.
type capturelessDriver struct{ *recordingDriver }

func (capturelessDriver) Capture() (*capture.Session, error) {
	return nil, &capture.Unavailable{Reason: capture.ReasonSimulator}
}

func TestRecording_DriversWithoutCaptureKeepTheirSteps(t *testing.T) {
	for _, d := range []core.Driver{
		&recordingDriver{Driver: mock.New(mock.Config{})},
		capturelessDriver{&recordingDriver{Driver: mock.New(mock.Config{})}},
	} {
		dir := t.TempDir()
		s, err := NewSession(context.Background(), d, RunnerConfig{OutputDir: dir}, SessionConfig{})
		if err != nil {
			t.Fatal(err)
		}
		s.Execute(context.Background(), mustStep(t, "startRecording: x"))
		s.Execute(context.Background(), mustStep(t, "stopRecording"))
		s.Close()
		var rd *recordingDriver
		switch v := d.(type) {
		case *recordingDriver:
			rd = v
		case capturelessDriver:
			rd = v.recordingDriver
		}
		if len(rd.got) != 2 || rd.got[0] != flow.StepStartRecording || rd.got[1] != flow.StepStopRecording {
			t.Fatalf("%T: the driver saw %v", d, rd.got)
		}
		if _, err := os.Stat(filepath.Join(dir, "recordings")); !os.IsNotExist(err) {
			t.Fatalf("%T: a recordings directory was made", d)
		}
	}
}

// shortSource streams the fake clip for d, then fails, so a recording of
// it ends before its stop.
type shortSource struct{ d time.Duration }

func (shortSource) Mode() capture.Mode { return capture.OnDemand }
func (shortSource) FPSControl() bool   { return true }

func (s shortSource) Run(ctx context.Context, opts capture.Options, emit func(capture.Packet)) error {
	run, cancel := context.WithTimeout(ctx, s.d)
	defer cancel()
	_ = fake.New().Run(run, opts, emit)
	if ctx.Err() != nil {
		return nil
	}
	return &capture.Unavailable{Reason: capture.ReasonScrcpyFailed, Message: "the server stopped"}
}

// earlyEndDriver is the mock with a capture session whose source fails
// after half a second, for the recording steps and for --record.
type earlyEndDriver struct {
	*mock.Driver
	screen capture.DriverCapture
}

func newEarlyEndDriver(cfg mock.Config) *earlyEndDriver {
	d := &earlyEndDriver{Driver: mock.New(cfg)}
	d.screen.Set(capture.NewSession(shortSource{500 * time.Millisecond}))
	return d
}

func (d *earlyEndDriver) Capture() (*capture.Session, error)   { return d.screen.Session() }
func (d *earlyEndDriver) StartScreenRecording() error          { return d.screen.StartRecording() }
func (d *earlyEndDriver) StopScreenRecording(dst string) error { return d.screen.StopRecording(dst) }

// Review 1.harness-review.2: a recording that ended early is saved, so
// stopRecording succeeds with the warning in its message.
func TestRecording_EndedEarlyStepSucceedsWithAWarning(t *testing.T) {
	dir := t.TempDir()
	s, err := NewSession(context.Background(), newEarlyEndDriver(mock.Config{}), RunnerConfig{OutputDir: dir}, SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if out := s.Execute(context.Background(), mustStep(t, "startRecording: early")); !out.Success {
		t.Fatalf("start: %+v", out.Error)
	}
	time.Sleep(time.Second)
	out := s.Execute(context.Background(), mustStep(t, "stopRecording"))
	if !out.Success || out.Data != "recordings/early.mp4" {
		t.Fatalf("stop: %+v %v", out.Error, out.Data)
	}
	if !strings.HasPrefix(out.Message, "Saved recording recordings/early.mp4 (warning: capture: the recording ended early: ") ||
		!strings.Contains(out.Message, "the server stopped") {
		t.Errorf("message %q", out.Message)
	}
	checkMP4(t, filepath.Join(dir, "recordings", "early.mp4"))
}

// Review 1.harness-review.2: --record links a recording that ended early,
// and --video on-failure still discards it when the flow passed.
func TestRecording_EndedEarlyRecordIsLinkedAndRetained(t *testing.T) {
	run := func(mode string) (dir string, detail report.FlowDetail) {
		dir = t.TempDir()
		drv := newEarlyEndDriver(mock.Config{StepDelay: 1500 * time.Millisecond})
		runner := New(drv, RunnerConfig{OutputDir: dir, Artifacts: ArtifactNever, DriverName: "mock", Record: true, RecordMode: mode})
		f := flow.Flow{SourcePath: "early.yaml", Config: flow.Config{Name: "early"}, Steps: []flow.Step{
			&flow.WaitForAnimationToEndStep{BaseStep: flow.BaseStep{StepType: flow.StepWaitForAnimationToEnd}},
		}}
		res, err := runner.Run(context.Background(), []flow.Flow{f})
		if err != nil || res.Status != report.StatusPassed {
			t.Fatalf("run: %v %+v", err, res)
		}
		flows, _ := filepath.Glob(filepath.Join(dir, "flows", "*.json"))
		if len(flows) != 1 {
			t.Fatalf("flow details %v", flows)
		}
		data, err := os.ReadFile(flows[0])
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &detail); err != nil {
			t.Fatal(err)
		}
		return dir, detail
	}

	dir, detail := run("")
	if detail.Artifacts.Video == "" {
		t.Fatal("the recording that ended early is not linked")
	}
	checkMP4(t, filepath.Join(dir, detail.Artifacts.Video))

	dir, detail = run("on-failure")
	if detail.Artifacts.Video != "" {
		t.Errorf("on-failure linked %s for a passing flow", detail.Artifacts.Video)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "assets", "*", "recording.mp4")); len(left) != 0 {
		t.Errorf("on-failure kept %v for a passing flow", left)
	}
}

func TestRecordingSaved(t *testing.T) {
	early := fmt.Errorf("%w: %w", capture.ErrEndedEarly, capture.ErrLagged)
	if !recordingSaved("r", nil) || !recordingSaved("r", early) || recordingSaved("r", errors.New("disk full")) {
		t.Error("recordingSaved: want nil and ErrEndedEarly saved, any other error not")
	}
}
