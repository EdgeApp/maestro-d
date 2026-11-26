package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/aicheck"
	"github.com/devicelab-dev/maestro-runner/pkg/aicheck/aichecktest"
	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/driver/mock"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
	"github.com/devicelab-dev/maestro-runner/pkg/report"
)

const (
	aiFakeJevKey       = "jev-FAKE-EXEC-KEY"
	aiFakeAnthropicKey = "sk-ant-FAKE-EXEC-KEY"
)

// shotDriver counts Screenshot calls and fails them on demand, and records
// any AI step that reaches Execute (none should).
type shotDriver struct {
	*mock.Driver
	shots    atomic.Int32
	fail     bool
	executed atomic.Int32
}

func (d *shotDriver) Screenshot() ([]byte, error) {
	d.shots.Add(1)
	if d.fail {
		return nil, errors.New("screen capture lost")
	}
	return d.Driver.Screenshot()
}

func (d *shotDriver) Execute(step flow.Step) *core.CommandResult {
	switch step.(type) {
	case *flow.AssertWithAIStep, *flow.AssertNoDefectsWithAIStep:
		d.executed.Add(1)
	}
	return d.Driver.Execute(step)
}

func aiSession(t *testing.T, fake *aichecktest.Server, d *shotDriver, env map[string]string) (*Session, string) {
	t.Helper()
	dir := reportDir(t)
	checker := &aicheck.Checker{
		Keys: func() (aicheck.Keys, error) {
			return aicheck.Keys{Jev: aiFakeJevKey, Anthropic: aiFakeAnthropicKey}, nil
		},
		JevURL:       fake.JevURL(),
		AnthropicURL: fake.AnthropicURL(),
	}
	s, err := NewSession(context.Background(), d, RunnerConfig{OutputDir: dir}, SessionConfig{AI: checker, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, dir
}

func newShotDriver(platform string) *shotDriver {
	return &shotDriver{Driver: mock.New(mock.Config{Platform: platform, DeviceID: "dev-1"})}
}

func TestAICheck_EveryVerdictIsASuccessfulStep(t *testing.T) {
	for _, tc := range []struct {
		noul    float64
		verdict string
	}{{0.9, aicheck.Pass}, {0.1, aicheck.Fail}, {0.5, aicheck.Unsure}} {
		fake := aichecktest.New(t, map[string]aichecktest.Answer{
			aicheck.ModelJev:    {Noul: aichecktest.Noul(tc.noul)},
			aicheck.ModelHaiku:  {Verdict: aicheck.Pass, Confidence: 0.1},
			aicheck.ModelSonnet: {Verdict: aicheck.Pass, Confidence: 0.1},
			aicheck.ModelOpus:   {Verdict: aicheck.Pass, Confidence: 0.1},
		})
		d := newShotDriver("ios")
		s, _ := aiSession(t, fake, d, nil)
		out := s.Execute(context.Background(), mustStep(t, "assertWithAI: A sign-in form is on screen"))
		res, ok := out.Data.(*aicheck.Result)
		if !out.Success || !ok || res.Verdict != tc.verdict || !strings.Contains(out.Message, tc.verdict) {
			t.Fatalf("noul %v: %+v", tc.noul, out)
		}
		if d.shots.Load() != 1 || d.executed.Load() != 0 {
			t.Fatalf("shots %d executed %d", d.shots.Load(), d.executed.Load())
		}
	}
}

func TestAICheck_ScreenshotErrorFailsTheStep(t *testing.T) {
	fake := aichecktest.New(t, map[string]aichecktest.Answer{})
	d := newShotDriver("android")
	d.fail = true
	s, _ := aiSession(t, fake, d, nil)
	out := s.Execute(context.Background(), mustStep(t, "assertNoDefectsWithAI"))
	if out.Success || !strings.Contains(out.Message, "screenshot failed") || len(fake.Requests()) != 0 {
		t.Fatalf("%+v", out)
	}
}

// Both dispatch sites: the top-level step and one nested in runFlow, on
// every platform's driver (the mock stands for each: only Screenshot is used).
func TestAICheck_TopLevelAndNested(t *testing.T) {
	for _, platform := range []string{"android", "ios", "tvos", "web", "macos"} {
		fake := aichecktest.New(t, map[string]aichecktest.Answer{aicheck.ModelJev: {Noul: aichecktest.Noul(0.95)}})
		d := newShotDriver(platform)
		s, _ := aiSession(t, fake, d, map[string]string{"WHAT": "a scoreboard"})
		for _, y := range []string{
			"assertWithAI:\n  assertion: ${WHAT} is on screen\n  budgetUsd: 0.1",
			"runFlow:\n  commands:\n    - assertWithAI: ${WHAT} is on screen\n    - assertNoDefectsWithAI:\n        thresholds:\n          jevPass: 0.9",
		} {
			if out := s.Execute(context.Background(), mustStep(t, y)); !out.Success {
				t.Fatalf("%s %q: %+v", platform, y, out)
			}
		}
		reqs := fake.Requests()
		if len(reqs) != 3 || d.shots.Load() != 3 || d.executed.Load() != 0 {
			t.Fatalf("%s: %d requests, %d shots, %d executed", platform, len(reqs), d.shots.Load(), d.executed.Load())
		}
		instr := func(i int) string {
			return reqs[i].Body["questions"].(map[string]any)["assertion"].(map[string]any)["instructions"].(string)
		}
		if !strings.Contains(instr(0), "a scoreboard is on screen") || !strings.Contains(instr(1), "a scoreboard is on screen") || !strings.Contains(instr(2), aicheck.NoDefectsAssertion) {
			t.Fatalf("%s: %q %q %q", platform, instr(0), instr(1), instr(2))
		}
	}
}

func TestAICheck_NoKeysIsUnsure(t *testing.T) {
	d := newShotDriver("android")
	s, err := NewSession(context.Background(), d, RunnerConfig{OutputDir: reportDir(t)}, SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	out := s.Execute(context.Background(), mustStep(t, "assertNoDefectsWithAI"))
	res, _ := out.Data.(*aicheck.Result)
	if !out.Success || res == nil || res.Verdict != aicheck.Unsure || len(res.Escalations) != 4 {
		t.Fatalf("%+v", out)
	}
	// An assertion that expands to nothing is a usage error.
	out = s.Execute(context.Background(), mustStep(t, "assertWithAI: ${NOTHING}"))
	if out.Success || out.Code != codeUsage {
		t.Fatalf("%+v", out)
	}
	// A key source that fails fails the step.
	s2, _ := NewSession(context.Background(), newShotDriver("ios"), RunnerConfig{OutputDir: reportDir(t)},
		SessionConfig{AI: &aicheck.Checker{Keys: func() (aicheck.Keys, error) { return aicheck.Keys{}, errors.New("key file unreadable") }}})
	t.Cleanup(s2.Close)
	if out := s2.Execute(context.Background(), mustStep(t, "assertWithAI: x")); out.Success || !strings.Contains(out.Message, "key file unreadable") {
		t.Fatalf("%+v", out)
	}
	if _, err := aiRequest(&flow.TapOnStep{}); err == nil {
		t.Fatal("tapOn taken for an AI check")
	}
	if r := (&FlowRunner{driver: d}).executeAICheck(&flow.TapOnStep{}); r.Success {
		t.Fatal(r)
	}
}

// No key reaches the report, the session's variables or the step's answer.
func TestAICheck_NoKeyInReportOrVariables(t *testing.T) {
	fake := aichecktest.New(t, map[string]aichecktest.Answer{
		aicheck.ModelJev:   {Status: 401, Body: "bad key " + aiFakeJevKey},
		aicheck.ModelHaiku: {Verdict: aicheck.Pass, Confidence: 0.9, Observed: "a form"},
	})
	s, dir := aiSession(t, fake, newShotDriver("android"), nil)
	out := s.Execute(context.Background(), mustStep(t, "assertWithAI: A form"))
	if !out.Success {
		t.Fatal(out)
	}
	for name, v := range s.Vars() {
		if strings.Contains(v, aiFakeJevKey) || strings.Contains(v, aiFakeAnthropicKey) {
			t.Fatalf("variable %s holds a key", name)
		}
	}
	s.Close()
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		data, _ := os.ReadFile(p)
		if strings.Contains(string(data), aiFakeJevKey) || strings.Contains(string(data), aiFakeAnthropicKey) {
			t.Errorf("%s holds a key", p)
		}
		return nil
	})
}

// A flow file run (maestro-d test) reaches executeStep's case: with no keys
// the check answers unsure and the step passes; the driver only takes the
// screenshot.
func TestAICheck_FlowRunTopLevel(t *testing.T) {
	d := newShotDriver("android")
	runner := New(d, RunnerConfig{OutputDir: t.TempDir(), Artifacts: ArtifactNever,
		Device: report.Device{ID: "dev-1", Platform: "android"}, DriverName: "mock"})
	steps := []flow.Step{mustStep(t, "assertWithAI: A sign-in form"), mustStep(t, "assertNoDefectsWithAI")}
	res, err := runner.Run(context.Background(), []flow.Flow{{SourcePath: "ai.yaml", Config: flow.Config{Name: "ai"}, Steps: steps}})
	if err != nil || res.Status != report.StatusPassed {
		t.Fatalf("%+v %v", res, err)
	}
	if d.shots.Load() < 2 || d.executed.Load() != 0 {
		t.Fatalf("shots %d executed %d", d.shots.Load(), d.executed.Load())
	}
}
