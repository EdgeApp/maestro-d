package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/driver/mock"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
	"github.com/devicelab-dev/maestro-runner/pkg/netshape"
	"github.com/devicelab-dev/maestro-runner/pkg/report"
)

// stubShaper records what it was asked to send.
type stubShaper struct {
	mu     sync.Mutex
	sets   []netshape.Values
	resets int
	err    error
}

func (s *stubShaper) Set(_ context.Context, v netshape.Values) (netshape.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sets = append(s.sets, v)
	if s.err != nil {
		return netshape.Result{}, s.err
	}
	return netshape.Result{Status: 200}, nil
}

func (s *stubShaper) Reset(context.Context) (netshape.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resets++
	if s.err != nil {
		return netshape.Result{}, s.err
	}
	return netshape.Result{Status: 204}, nil
}

func shapingSession(t *testing.T, shaper TrafficShaper, env map[string]string) *Session {
	t.Helper()
	sc := SessionConfig{Env: env}
	if shaper != nil {
		sc.TrafficShaper = shaper
	}
	s, err := NewSession(context.Background(), mock.New(mock.Config{Platform: "android", DeviceID: "phone-1"}), RunnerConfig{OutputDir: reportDir(t)}, sc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

// reportDir is a session's report directory. The report writer can still be
// flushing as a test ends, so its removal is best effort (t.TempDir would
// fail the test on a file written during RemoveAll).
func reportDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "trafficshape-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestTrafficShape_SendsEveryValueWithDefaults(t *testing.T) {
	sh := &stubShaper{}
	s := shapingSession(t, sh, nil)
	out := s.Execute(context.Background(), mustStep(t, "setTrafficShape:\n  mbit: 1.5\n  latencyMs: 100\n  direction: download"))
	if !out.Success || !strings.Contains(out.Message, "traffic shape set (mbit 1.5, lossPct 0, latencyMs 100, jitterMs 0, download): 200") {
		t.Fatalf("%+v", out)
	}
	if d, ok := out.Data.(map[string]any); !ok || d["status"] != 200 {
		t.Fatalf("data = %#v", out.Data)
	}
	out = s.Execute(context.Background(), mustStep(t, "setTrafficShape:"))
	if !out.Success {
		t.Fatalf("%+v", out)
	}
	want := []netshape.Values{{Mbit: 1.5, LatencyMs: 100, Direction: "download"}, {Direction: "both"}}
	if fmt.Sprint(sh.sets) != fmt.Sprint(want) {
		t.Fatalf("sets = %+v, want %+v", sh.sets, want)
	}
	out = s.Execute(context.Background(), mustStep(t, "resetTrafficShape"))
	if !out.Success || sh.resets != 1 || out.Message != "traffic shape reset: 204" {
		t.Fatalf("%+v resets=%d", out, sh.resets)
	}
}

// Only a daemon session has a shaper; without one (maestro-d test) the
// steps say where they run rather than pointing at a host's config.
func TestTrafficShape_WithoutAShaperIsUnsupported(t *testing.T) {
	s := shapingSession(t, nil, nil)
	for _, c := range []struct{ yaml, step string }{{"setTrafficShape:\n  lossPct: 100", "setTrafficShape"}, {"resetTrafficShape", "resetTrafficShape"}} {
		out := s.Execute(context.Background(), mustStep(t, c.yaml))
		want := c.step + " runs in a maestro-d daemon session (a one-shot command, REST or the JS client); maestro-d test does not shape traffic"
		if out.Success || out.Code != codeUnsupported || out.Error.Message != want {
			t.Fatalf("%q: %+v %+v", c.yaml, out, out.Error)
		}
	}
}

func TestTrafficShape_ErrorCodes(t *testing.T) {
	for _, c := range []struct {
		err  error
		code string
	}{
		{fmt.Errorf("%w for phone-1 on this host", netshape.ErrNotConfigured), codeUnsupported},
		{&netshape.ConfigError{Path: "/h/config.json", Err: errors.New("mode 0644")}, codeUsage},
		{&netshape.FetchError{Command: "setTrafficShape", Method: "POST", URL: "http://s/", Status: 500, Excerpt: "boom"}, ""},
	} {
		sh := &stubShaper{err: c.err}
		s := shapingSession(t, sh, nil)
		for _, y := range []string{"setTrafficShape:", "resetTrafficShape"} {
			out := s.Execute(context.Background(), mustStep(t, y))
			if out.Success || out.Code != c.code || out.Error.Message != c.err.Error() {
				t.Errorf("%T %q: %+v %+v", c.err, y, out, out.Error)
			}
		}
	}
}

func TestTrafficShape_DirectionFromAVariable(t *testing.T) {
	sh := &stubShaper{}
	s := shapingSession(t, sh, map[string]string{"DIR": "upload", "BAD": "sideways"})
	if out := s.Execute(context.Background(), mustStep(t, "setTrafficShape:\n  direction: ${DIR}")); !out.Success {
		t.Fatalf("%+v", out)
	}
	out := s.Execute(context.Background(), mustStep(t, "setTrafficShape:\n  direction: ${BAD}"))
	if out.Success || out.Code != codeUsage || !strings.Contains(out.Error.Message, `got "sideways"`) {
		t.Fatalf("%+v %+v", out, out.Error)
	}
	if len(sh.sets) != 1 || sh.sets[0].Direction != "upload" {
		t.Fatalf("sets = %+v", sh.sets)
	}
}

func TestTrafficShape_InsideARunFlow(t *testing.T) {
	sh := &stubShaper{}
	s := shapingSession(t, sh, nil)
	out := s.Execute(context.Background(), mustStep(t, "runFlow:\n  commands:\n    - setTrafficShape:\n        jitterMs: 5\n    - resetTrafficShape"))
	if !out.Success || len(sh.sets) != 1 || sh.sets[0].JitterMs != 5 || sh.resets != 1 {
		t.Fatalf("%+v sets=%+v resets=%d", out, sh.sets, sh.resets)
	}
}

// `maestro-d test` has no shaper: the steps fail there.
func TestTrafficShape_FlowRunnerRunHasNoShaper(t *testing.T) {
	runner := New(mock.New(mock.Config{}), RunnerConfig{OutputDir: t.TempDir(), Artifacts: ArtifactNever, DriverName: "mock"})
	f := flow.Flow{SourcePath: "shape.yaml", Config: flow.Config{Name: "shape"}, Steps: []flow.Step{mustStep(t, "resetTrafficShape")}}
	res, err := runner.Run(context.Background(), []flow.Flow{f})
	if err != nil || res.Status != report.StatusFailed {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestTrafficShape_NotATrafficStep(t *testing.T) {
	fr := &FlowRunner{shaper: &stubShaper{}, script: NewScriptEngine()}
	if r := fr.executeTrafficShapeStep(&flow.BackStep{}); r.Success {
		t.Fatalf("%+v", r)
	}
}

func TestUsageError(t *testing.T) {
	inner := errors.New("bad")
	e := &UsageError{Err: inner}
	if e.Error() != "bad" || !errors.Is(e, inner) || outcomeCode(e) != codeUsage {
		t.Fatal("UsageError")
	}
}
