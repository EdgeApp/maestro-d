package executor

import (
	"context"
	"encoding/json"
	"image"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/driver/mock"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
	"github.com/devicelab-dev/maestro-runner/pkg/report"
)

func newTestSession(t *testing.T, sc SessionConfig, mockCfg mock.Config) (*Session, string) {
	t.Helper()
	dir := t.TempDir()
	drv := mock.New(mockCfg)
	s, err := NewSession(context.Background(), drv, RunnerConfig{
		OutputDir: dir,
		Artifacts: ArtifactOnFailure,
		Env:       map[string]string{"CLI_VAR": "from-cli"},
	}, sc)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	t.Cleanup(s.Close)
	return s, dir
}

func mustStep(t *testing.T, yaml string) flow.Step {
	t.Helper()
	s, err := flow.ParseStep([]byte(yaml), "test")
	if err != nil {
		t.Fatalf("ParseStep(%q): %v", yaml, err)
	}
	return s
}

// readReport returns the session's index and flow detail as written to disk.
func readReport(t *testing.T, dir string) (report.Index, report.FlowDetail) {
	t.Helper()
	var idx report.Index
	data, err := os.ReadFile(filepath.Join(dir, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &idx); err != nil {
		t.Fatalf("decode report.json: %v", err)
	}
	if len(idx.Flows) != 1 {
		t.Fatalf("index has %d flows, want 1", len(idx.Flows))
	}
	data, err = os.ReadFile(filepath.Join(dir, idx.Flows[0].DataFile))
	if err != nil {
		t.Fatal(err)
	}
	var d report.FlowDetail
	if err := json.Unmarshal(data, &d); err != nil {
		t.Fatalf("decode flow detail: %v", err)
	}
	return idx, d
}

func TestSession_ExecuteRecordsSteps(t *testing.T) {
	s, dir := newTestSession(t, SessionConfig{Name: "sess", AppID: "com.example.app"}, mock.Config{})

	out := s.Execute(context.Background(), mustStep(t, `tapOn: Login`))
	if !out.Success {
		t.Fatalf("tapOn failed: %+v", out)
	}
	if out.Index != 0 || out.Type != "tapOn" || out.Element == nil {
		t.Fatalf("unexpected outcome: %+v", out)
	}

	// launchApp with no appId gets the session default.
	la := mustStep(t, `launchApp`).(*flow.LaunchAppStep)
	out = s.Execute(context.Background(), la)
	if !out.Success || la.AppID != "com.example.app" {
		t.Fatalf("launchApp default appId not applied: %+v (appId=%q)", out, la.AppID)
	}
	if s.Vars()["APP_ID"] != "com.example.app" {
		t.Fatalf("APP_ID var missing: %v", s.Vars())
	}

	out = s.Execute(context.Background(), mustStep(t, `assertTrue: ${CLI_VAR == "from-cli"}`))
	if !out.Success {
		t.Fatalf("assertTrue on CLI var failed: %+v", out)
	}

	s.Close()
	idx, d := readReport(t, dir)
	if len(d.Commands) != 3 {
		t.Fatalf("report has %d commands, want 3", len(d.Commands))
	}
	for i, c := range d.Commands {
		if c.Status != report.StatusPassed {
			t.Errorf("command %d status %s", i, c.Status)
		}
		if c.Index != i {
			t.Errorf("command %d index %d", i, c.Index)
		}
	}
	if idx.Flows[0].Status != report.StatusPassed {
		t.Errorf("flow status %s, want passed", idx.Flows[0].Status)
	}
}

func TestSession_FailureAndOptional(t *testing.T) {
	s, dir := newTestSession(t, SessionConfig{}, mock.Config{})

	out := s.Execute(context.Background(), mustStep(t, `assertTrue: "false"`))
	if out.Success || out.Error == nil {
		t.Fatalf("expected failure: %+v", out)
	}
	if out.Artifacts.ScreenshotAfter == "" || out.Artifacts.ViewHierarchy == "" {
		t.Fatalf("expected failure artifacts: %+v", out.Artifacts)
	}
	if _, err := os.Stat(filepath.Join(dir, out.Artifacts.ScreenshotAfter)); err != nil {
		t.Fatalf("screenshot artifact missing: %v", err)
	}
	if !s.Failed() {
		t.Fatal("session should be marked failed")
	}

	// Session stays usable after a failure.
	out = s.Execute(context.Background(), mustStep(t, `{"assertTrue": {"condition": "false", "optional": true}}`))
	if !out.Success || !out.Optional || out.Error == nil {
		t.Fatalf("optional failure should succeed with error preserved: %+v", out)
	}
	out = s.Execute(context.Background(), mustStep(t, `back`))
	if !out.Success {
		t.Fatalf("back after failure: %+v", out)
	}

	s.Close()
	idx, d := readReport(t, dir)
	if idx.Flows[0].Status != report.StatusFailed {
		t.Errorf("flow status %s, want failed", idx.Flows[0].Status)
	}
	if d.Commands[0].Status != report.StatusFailed || d.Commands[0].Error == nil {
		t.Errorf("first command should be failed with error: %+v", d.Commands[0])
	}
}

func TestSession_PlatformGate(t *testing.T) {
	s, _ := newTestSession(t, SessionConfig{}, mock.Config{Platform: "android"})
	out := s.Execute(context.Background(), mustStep(t, `{"tapOn": {"text": "X", "platform": "ios"}}`))
	if !out.Success || !out.Skipped {
		t.Fatalf("expected skipped: %+v", out)
	}
	out = s.Execute(context.Background(), mustStep(t, `{"tapOn": {"text": "X", "platform": "android"}}`))
	if !out.Success || out.Skipped {
		t.Fatalf("expected executed: %+v", out)
	}
}

func TestSession_CompoundAndRunFlow(t *testing.T) {
	s, _ := newTestSession(t, SessionConfig{AppID: "com.example.app"}, mock.Config{})

	out := s.Execute(context.Background(), mustStep(t, `{"repeat": {"times": 3, "commands": [{"tapOn": "X"}]}}`))
	if !out.Success {
		t.Fatalf("repeat failed: %+v", out)
	}
	if len(out.SubSteps) != 3 {
		t.Fatalf("repeat sub-steps = %d, want 3", len(out.SubSteps))
	}

	f, err := flow.Parse([]byte("appId: com.other\nonFlowStart:\n  - evalScript: ${output.started = 'yes'}\n---\n- launchApp\n- tapOn: Y\n- evalScript: ${output.done = 'ok'}\n"), "inline.yaml")
	if err != nil {
		t.Fatal(err)
	}
	out = s.RunFlow(context.Background(), *f)
	if !out.Success {
		t.Fatalf("runFlow failed: %+v", out)
	}
	if len(out.SubSteps) != 4 {
		t.Fatalf("runFlow sub-steps = %d, want 4 (hook + 3): %+v", len(out.SubSteps), out.SubSteps)
	}
	vars := s.Vars()
	if vars["done"] != "ok" || vars["started"] != "yes" {
		t.Fatalf("flow output not shared with session: %v", vars)
	}
	if la, ok := f.Steps[0].(*flow.LaunchAppStep); !ok || la.AppID != "com.other" {
		t.Fatalf("subflow appId not applied: %#v", f.Steps[0])
	}
}

func TestSession_VarsEvalAndScreenshot(t *testing.T) {
	dir := t.TempDir()
	s, err := NewSession(context.Background(), mock.New(mock.Config{}), RunnerConfig{OutputDir: dir},
		SessionConfig{Env: map[string]string{"GREETING": "hi"}, FlowDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetVar("NAME", "paul")
	v, err := s.Eval(`GREETING + " " + NAME`)
	if err != nil || v != "hi paul" {
		t.Fatalf("Eval = %v, %v", v, err)
	}
	if _, err := s.Eval(`output.token = "abc"`); err != nil {
		t.Fatal(err)
	}
	if s.Vars()["token"] != "abc" {
		t.Fatalf("output not synced: %v", s.Vars())
	}
	if _, err := s.Eval(`nope(`); err == nil {
		t.Fatal("expected syntax error")
	}

	png, err := s.Screenshot(context.Background())
	if err != nil || len(png) == 0 {
		t.Fatalf("Screenshot: %v", err)
	}
	h, err := s.Hierarchy(context.Background())
	if err != nil || len(h) == 0 {
		t.Fatalf("Hierarchy: %v", err)
	}
	if st := s.State(context.Background()); st == nil {
		t.Fatal("State nil")
	}
	if pi := s.PlatformInfo(); pi == nil || pi.Platform != "mock" {
		t.Fatalf("PlatformInfo: %+v", pi)
	}

	// Two takeScreenshot steps must produce two distinct artifacts.
	o1 := s.Execute(context.Background(), mustStep(t, `takeScreenshot`))
	o2 := s.Execute(context.Background(), mustStep(t, `takeScreenshot: shot2`))
	if !o1.Success || !o2.Success {
		t.Fatalf("takeScreenshot: %+v %+v", o1, o2)
	}
	if o1.Artifacts.ScreenshotAfter == "" || o1.Artifacts.ScreenshotAfter == o2.Artifacts.ScreenshotAfter {
		t.Fatalf("screenshot artifacts collide: %q %q", o1.Artifacts.ScreenshotAfter, o2.Artifacts.ScreenshotAfter)
	}
	if _, ok := o1.Data.([]byte); !ok {
		t.Fatalf("takeScreenshot data should be PNG bytes, got %T", o1.Data)
	}
	if _, err := os.Stat(filepath.Join(dir, "shot2.png")); err != nil {
		t.Fatalf("requested screenshot path not written under FlowDir: %v", err)
	}
}

func TestSession_ExecuteAllAndCancel(t *testing.T) {
	s, _ := newTestSession(t, SessionConfig{}, mock.Config{})
	steps := []flow.Step{mustStep(t, `back`), mustStep(t, `assertTrue: "false"`), mustStep(t, `back`)}
	outs := s.ExecuteAll(context.Background(), steps, false)
	if len(outs) != 2 || outs[1].Success {
		t.Fatalf("ExecuteAll should stop at first failure: %d outcomes", len(outs))
	}
	outs = s.ExecuteAll(context.Background(), steps, true)
	if len(outs) != 3 {
		t.Fatalf("ExecuteAll continueOnError: %d outcomes", len(outs))
	}

	// A cancelled context fails the step with Cancelled set and leaves the
	// session usable under the base context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := s.Execute(ctx, mustStep(t, `{"repeat": {"times": 2, "commands": [{"tapOn": "X"}]}}`))
	if out.Success || !out.Cancelled {
		t.Fatalf("expected cancelled failure: %+v", out)
	}
	out = s.Execute(context.Background(), mustStep(t, `back`))
	if !out.Success {
		t.Fatalf("session unusable after cancel: %+v", out)
	}
}

type panickyDriver struct{ core.Driver }

func (panickyDriver) Execute(flow.Step) *core.CommandResult { panic("boom") }

func TestSession_DriverPanicIsContained(t *testing.T) {
	dir := t.TempDir()
	drv := panickyDriver{mock.New(mock.Config{})}
	s, err := NewSession(context.Background(), drv, RunnerConfig{OutputDir: dir, Artifacts: ArtifactNever}, SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	out := s.Execute(context.Background(), mustStep(t, `tapOn: X`))
	if out.Success || !out.Panicked || out.Error == nil || out.Error.Type != "device" {
		t.Fatalf("expected contained panic: %+v", out)
	}
}

func TestSession_ClosedRejects(t *testing.T) {
	s, _ := newTestSession(t, SessionConfig{}, mock.Config{})
	s.Close()
	s.Close() // idempotent
	out := s.Execute(context.Background(), mustStep(t, `back`))
	if out.Success {
		t.Fatal("closed session executed a step")
	}
	if _, err := s.Screenshot(context.Background()); err == nil {
		t.Fatal("closed session took a screenshot")
	}
}

// frameMock is the mock driver with a FrameGrabber.
type frameMock struct{ *mock.Driver }

func (frameMock) Frame(_ context.Context, r image.Rectangle) (*image.RGBA, error) {
	return image.NewRGBA(image.Rect(0, 0, r.Dx(), r.Dy())), nil
}

func TestSession_Frame(t *testing.T) {
	r := image.Rect(1, 1, 4, 3)
	s, err := NewSession(context.Background(), frameMock{mock.New(mock.Config{})}, RunnerConfig{OutputDir: t.TempDir()}, SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	img, err := s.Frame(context.Background(), r)
	if err != nil || img.Bounds().Dx() != 3 {
		t.Fatalf("Frame: %v", err)
	}
	s.Close()
	if _, err := s.Frame(context.Background(), r); err == nil || err.Error() != "session is closed" {
		t.Errorf("closed session: %v", err)
	}

	plain, _ := newTestSession(t, SessionConfig{}, mock.Config{})
	if _, err := plain.Frame(context.Background(), r); err == nil {
		t.Error("a driver without frames answered a frame")
	}
}

func TestSession_StepDelay(t *testing.T) {
	dir := t.TempDir()
	s, err := NewSession(context.Background(), mock.New(mock.Config{}), RunnerConfig{OutputDir: dir, StepDelay: 30}, SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	start := time.Now()
	s.ExecuteAll(context.Background(), []flow.Step{mustStep(t, `back`), mustStep(t, `back`), mustStep(t, `back`)}, false)
	if el := time.Since(start); el < 60*time.Millisecond {
		t.Fatalf("StepDelay not applied between steps: %v", el)
	}
}

type flowTimingDriver struct {
	core.Driver
	find, idle, typing int
}

func (d *flowTimingDriver) SetFindTimeout(ms int) { d.find = ms }
func (d *flowTimingDriver) SetWaitForIdleTimeout(ms int) error {
	d.idle = ms
	return nil
}
func (d *flowTimingDriver) SetTypingFrequency(hz int) error {
	d.typing = hz
	return nil
}

func TestHubTestRestoresLeasedDriverAfterCancellation(t *testing.T) {
	driver := &flowTimingDriver{Driver: mock.New(mock.Config{})}
	s, err := NewSession(context.Background(), driver, RunnerConfig{
		OutputDir: t.TempDir(), WaitForIdleTimeout: 12, TypingFrequency: 13,
	}, SessionConfig{CommandTimeout: 11})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	f, err := flow.Parse([]byte("commandTimeout: 21\nwaitForIdleTimeout: 22\ntypingFrequency: 23\n---\n- assertTrue: true\n- assertTrue: true\n"), "paced.yaml")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	out := s.RunTestFlow(ctx, *f, TestFlowOptions{StepDelay: 5000})
	if out.Success || time.Since(start) >= 4*time.Second {
		t.Fatalf("step delay did not respond to cancellation: %+v", out)
	}
	if driver.find != 11 || driver.idle != 12 || driver.typing != 13 {
		t.Fatalf("leased session timing after cancellation: %+v", driver)
	}
}
