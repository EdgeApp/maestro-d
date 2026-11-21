package executor

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/driver/mock"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
	"github.com/devicelab-dev/maestro-runner/pkg/netshape"
	"github.com/devicelab-dev/maestro-runner/pkg/report"
)

func airplaneSteps(t *testing.T) []flow.Step {
	return []flow.Step{mustStep(t, "setAirplaneMode: enabled"), mustStep(t, "toggleAirplaneMode")}
}

// The daemon's route: a Session runs each step through executeNestedStep.
func TestAirplane_RefusedOnIOSAndTVOSThroughASession(t *testing.T) {
	for _, platform := range []string{"ios", "tvos", "iOS"} {
		d := &recordingDriver{Driver: mock.New(mock.Config{Platform: platform})}
		s, err := NewSession(context.Background(), d, RunnerConfig{OutputDir: t.TempDir()}, SessionConfig{})
		if err != nil {
			t.Fatal(err)
		}
		for _, step := range airplaneSteps(t) {
			out := s.Execute(context.Background(), step)
			if out.Success || out.Code != codeUnsupported || out.Error == nil || out.Error.Message != iosAirplaneMessage {
				t.Errorf("%s %s: %+v (error %+v)", platform, step.Type(), out, out.Error)
			}
		}
		s.Close()
		if len(d.got) != 0 {
			t.Errorf("%s: the driver saw %v", platform, d.got)
		}
	}
}

func TestAirplane_ReachesTheDriverElsewhere(t *testing.T) {
	d := &recordingDriver{Driver: mock.New(mock.Config{Platform: "android"})}
	s, err := NewSession(context.Background(), d, RunnerConfig{OutputDir: t.TempDir()}, SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, step := range airplaneSteps(t) {
		if out := s.Execute(context.Background(), step); !out.Success || out.Code != "" {
			t.Errorf("%s: %+v", step.Type(), out)
		}
	}
	if len(d.got) != 2 || d.got[0] != flow.StepSetAirplaneMode || d.got[1] != flow.StepToggleAirplaneMode {
		t.Errorf("the driver saw %v", d.got)
	}
}

// Inside runFlow the step is nested, and the refusal still carries its code.
func TestAirplane_RefusedInsideARunFlow(t *testing.T) {
	d := &recordingDriver{Driver: mock.New(mock.Config{Platform: "ios"})}
	s, err := NewSession(context.Background(), d, RunnerConfig{OutputDir: t.TempDir()}, SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	out := s.Execute(context.Background(), mustStep(t, "runFlow:\n  commands:\n    - setAirplaneMode: disabled"))
	if out.Success || len(d.got) != 0 {
		t.Fatalf("%+v; the driver saw %v", out, d.got)
	}
}

// The upstream runner's route (`maestro-d test`): FlowRunner.Run.
func TestAirplane_RefusedOnIOSThroughFlowRunnerRun(t *testing.T) {
	for _, c := range []struct {
		platform string
		want     report.Status
		seen     int
	}{{"ios", report.StatusFailed, 0}, {"tvos", report.StatusFailed, 0}, {"android", report.StatusPassed, 1}} {
		d := &recordingDriver{Driver: mock.New(mock.Config{Platform: c.platform})}
		runner := New(d, RunnerConfig{OutputDir: t.TempDir(), Artifacts: ArtifactNever, DriverName: "mock"})
		f := flow.Flow{SourcePath: "air.yaml", Config: flow.Config{Name: "air"}, Steps: []flow.Step{mustStep(t, "setAirplaneMode: enabled")}}
		res, err := runner.Run(context.Background(), []flow.Flow{f})
		if err != nil || res.Status != c.want || len(d.got) != c.seen {
			t.Errorf("%s: err %v status %s; the driver saw %v", c.platform, err, res.Status, d.got)
		}
	}
}

func TestAirplane_NoPlatformInfoReachesTheDriver(t *testing.T) {
	d := &recordingDriver{Driver: noInfoDriver{mock.New(mock.Config{})}}
	fr := &FlowRunner{driver: d}
	if r := fr.executeAirplaneStep(mustStep(t, "toggleAirplaneMode")); !r.Success || len(d.got) != 1 {
		t.Fatalf("%+v; the driver saw %v", r, d.got)
	}
}

type noInfoDriver struct{ core.Driver }

func (noInfoDriver) GetPlatformInfo() *core.PlatformInfo { return nil }

func TestOutcomeCode(t *testing.T) {
	for err, want := range map[error]string{
		nil:                         "",
		errors.New("plain"):         "",
		&UnsupportedError{Msg: "x"}: codeUnsupported,
		fmt.Errorf("wrapped: %w", &UnsupportedError{Msg: "x"}):                            codeUnsupported,
		&netshape.ConfigError{Path: "/h/network.json", Err: errors.New("bad")}:            codeUsage,
		fmt.Errorf("wrapped: %w", &netshape.ConfigError{Path: "p", Err: errors.New("b")}): codeUsage,
	} {
		if got := outcomeCode(err); got != want {
			t.Errorf("outcomeCode(%v) = %q, want %q", err, got, want)
		}
	}
	if (&UnsupportedError{Msg: "no"}).Error() != "no" {
		t.Error("UnsupportedError message")
	}
}

// An optional step that fails keeps its code but passes.
func TestAirplane_OptionalRefusalPasses(t *testing.T) {
	d := &recordingDriver{Driver: mock.New(mock.Config{Platform: "ios"})}
	s, err := NewSession(context.Background(), d, RunnerConfig{OutputDir: t.TempDir()}, SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	out := s.Execute(context.Background(), mustStep(t, "setAirplaneMode:\n  value: enabled\n  optional: true"))
	if !out.Success || out.Code != codeUnsupported {
		t.Fatalf("%+v", out)
	}
}
