package executor

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/driver/mock"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

func deviceSession(t *testing.T, d core.Driver, sc SessionConfig) *Session {
	t.Helper()
	s, err := NewSession(context.Background(), d, RunnerConfig{OutputDir: reportDir(t)}, sc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

// probeDriver is the test mock with a SampleProbe.
type probeDriver struct {
	mockDriver
	probe func(kind, appID string) (core.SampleFunc, error)
}

func (p *probeDriver) SampleProbe(kind, appID string) (core.SampleFunc, error) {
	return p.probe(kind, appID)
}

func platform(p string, sim bool) func() *core.PlatformInfo {
	return func() *core.PlatformInfo { return &core.PlatformInfo{Platform: p, IsSimulator: sim, DeviceID: "dev-1"} }
}

func TestSetStatusBar_PhysicalAppleDeviceIsUnsupported(t *testing.T) {
	for _, p := range []string{"ios", "tvos"} {
		var reached atomic.Int32
		d := &mockDriver{platformFunc: platform(p, false), executeFunc: func(flow.Step) *core.CommandResult {
			reached.Add(1)
			return &core.CommandResult{Success: true}
		}}
		s := deviceSession(t, d, SessionConfig{})
		for _, y := range []string{"setStatusBar: demo", "runFlow:\n  commands:\n    - setStatusBar:\n        mode: live"} {
			out := s.Execute(context.Background(), mustStep(t, y))
			if strings.HasPrefix(y, "runFlow") {
				if out.Success || !strings.Contains(out.Message+fmtSub(out), core.StatusBarPhysicalIOSMessage) {
					t.Fatalf("%s nested: %+v", p, out)
				}
				continue
			}
			if out.Success || out.Code != codeUnsupported || out.Error.Message != "UNSUPPORTED: status bar override on a physical iOS device" {
				t.Fatalf("%s: %+v %+v", p, out, out.Error)
			}
		}
		// runFlow itself reaches the driver for nothing but the gate.
		if reached.Load() != 0 {
			t.Fatalf("%s: driver reached %d times", p, reached.Load())
		}
	}
}

func fmtSub(o *StepOutcome) string {
	var b strings.Builder
	for _, c := range o.SubSteps {
		if c.Error != nil {
			b.WriteString(c.Error.Message)
		}
	}
	if o.Error != nil {
		b.WriteString(o.Error.Message)
	}
	return b.String()
}

func TestSetStatusBar_NoStatusBarPlatforms(t *testing.T) {
	for _, p := range []string{"web", "macos"} {
		s := deviceSession(t, &mockDriver{platformFunc: platform(p, false)}, SessionConfig{})
		out := s.Execute(context.Background(), mustStep(t, "setStatusBar: demo"))
		if out.Success || out.Code != codeUnsupported || !strings.Contains(out.Message, "has no status bar") {
			t.Fatalf("%s: %+v", p, out)
		}
	}
}

func TestSetStatusBar_ReachesTheDriverExpanded(t *testing.T) {
	var got *flow.SetStatusBarStep
	d := &mockDriver{platformFunc: platform("ios", true), executeFunc: func(st flow.Step) *core.CommandResult {
		got, _ = st.(*flow.SetStatusBarStep)
		return &core.CommandResult{Success: true}
	}}
	s := deviceSession(t, d, SessionConfig{Env: map[string]string{"M": "demo", "T": "10:09", "BAD": "sideways"}})
	out := s.Execute(context.Background(), mustStep(t, "setStatusBar:\n  mode: ${M}\n  time: ${T}\n  battery: 50"))
	if !out.Success || got == nil || got.Mode != "demo" || got.EffectiveTime() != "10:09" || got.EffectiveBattery() != 50 {
		t.Fatalf("%+v %+v", out, got)
	}
	out = s.Execute(context.Background(), mustStep(t, "runFlow:\n  commands:\n    - setStatusBar: ${M}"))
	if !out.Success {
		t.Fatalf("nested: %+v", out)
	}
	out = s.Execute(context.Background(), mustStep(t, "setStatusBar: ${BAD}"))
	if out.Success || out.Code != codeUsage {
		t.Fatalf("bad mode: %+v", out)
	}
}

func TestKeyboardVisible_DataFromTheDriver(t *testing.T) {
	s := deviceSession(t, mock.New(mock.Config{Platform: "android"}), SessionConfig{})
	out := s.Execute(context.Background(), mustStep(t, "keyboardVisible"))
	if d, ok := out.Data.(map[string]any); !out.Success || !ok || d["visible"] != false {
		t.Fatalf("%+v", out)
	}
}

func TestSampler_LifecycleOnTheMock(t *testing.T) {
	s := deviceSession(t, mock.New(mock.Config{Platform: "android"}), SessionConfig{AppID: "com.example"})
	out := s.Execute(context.Background(), mustStep(t, "startSampler:\n  kind: memory\n  intervalMs: 100"))
	d, _ := out.Data.(map[string]any)
	if !out.Success || d["kind"] != "memory" || d["appId"] != "com.example" || d["intervalMs"] != int64(100) {
		t.Fatalf("start: %+v", out)
	}
	if out := s.Execute(context.Background(), mustStep(t, "startSampler: memory")); out.Success || out.Code != codeUsage {
		t.Fatalf("second start: %+v", out)
	}
	time.Sleep(350 * time.Millisecond)
	out = s.Execute(context.Background(), mustStep(t, "stopSampler: memory"))
	d, _ = out.Data.(map[string]any)
	if !out.Success || d["unit"] != "bytes" || d["count"].(int) < 2 || d["mean"] != float64(64<<20) || d["min"] != d["max"] {
		t.Fatalf("stop: %+v", out)
	}
	if samples, ok := d["samples"].([]sample); !ok || samples[0].Value != 64<<20 {
		t.Fatalf("samples: %#v", d["samples"])
	}
	for _, k := range []string{"durationMs", "intervalMs", "appId", "kind"} {
		if _, ok := d[k]; !ok {
			t.Fatalf("no %s in %v", k, d)
		}
	}
	if out := s.Execute(context.Background(), mustStep(t, "stopSampler: memory")); out.Success || !strings.Contains(out.Message, "no memory sampler is running") {
		t.Fatalf("stop again: %+v", out)
	}
}

func TestSampler_NestedAndStoppedAtSessionEnd(t *testing.T) {
	var reads atomic.Int32
	d := &probeDriver{mockDriver: mockDriver{platformFunc: platform("android", true)}, probe: func(kind, appID string) (core.SampleFunc, error) {
		return func() (float64, error) {
			if reads.Add(1)%2 == 0 {
				return 0, core.ErrNoSample
			}
			return 7, nil
		}, nil
	}}
	s := deviceSession(t, d, SessionConfig{})
	out := s.Execute(context.Background(), mustStep(t, "runFlow:\n  commands:\n    - startSampler:\n        kind: cpu\n        appId: com.x\n        intervalMs: 100"))
	if !out.Success {
		t.Fatalf("nested start: %+v", out)
	}
	time.Sleep(250 * time.Millisecond)
	s.Cleanup(context.Background())
	n := reads.Load()
	time.Sleep(250 * time.Millisecond)
	if reads.Load() != n || n == 0 {
		t.Fatalf("reads %d then %d: the sampler outlived the session", n, reads.Load())
	}
}

func TestSampler_Refusals(t *testing.T) {
	// No app at all.
	s := deviceSession(t, mock.New(mock.Config{Platform: "android"}), SessionConfig{})
	if out := s.Execute(context.Background(), mustStep(t, "startSampler: cpu")); out.Success || out.Code != codeUsage || !strings.Contains(out.Message, "no appId") {
		t.Fatalf("no app: %+v", out)
	}
	// A driver with no probe.
	s = deviceSession(t, &mockDriver{platformFunc: platform("web", false)}, SessionConfig{AppID: "a"})
	if out := s.Execute(context.Background(), mustStep(t, "startSampler: cpu")); out.Success || out.Code != codeUnsupported || !strings.Contains(out.Message, "web driver cannot measure") {
		t.Fatalf("no probe: %+v", out)
	}
	// The probe's refusal and its error.
	for _, c := range []struct {
		err  error
		code string
		msg  string
	}{
		{&core.UnsupportedError{Msg: "frames sampling is not available"}, codeUnsupported, "startSampler: frames sampling is not available"},
		{errors.New("boom"), "", "startSampler: boom"},
	} {
		d := &probeDriver{probe: func(string, string) (core.SampleFunc, error) { return nil, c.err }}
		s = deviceSession(t, d, SessionConfig{AppID: "a"})
		out := s.Execute(context.Background(), mustStep(t, "startSampler: frames"))
		if out.Success || out.Code != c.code || out.Message != c.msg {
			t.Fatalf("%v: %+v", c.err, out)
		}
	}
	// Kinds that expand to something unknown.
	s = deviceSession(t, mock.New(mock.Config{Platform: "android"}), SessionConfig{AppID: "a", Env: map[string]string{"K": "heat"}})
	for _, y := range []string{"startSampler: ${K}", "stopSampler: ${K}"} {
		if out := s.Execute(context.Background(), mustStep(t, y)); out.Success || out.Code != codeUsage {
			t.Fatalf("%s: %+v", y, out)
		}
	}
}

// A sampler stopped before its first tick still answers one reading, and
// one that never reads answers zeros.
func TestSampler_FinishTakesOneReading(t *testing.T) {
	rs := &runningSampler{kind: "frames", appID: "a", interval: time.Hour, started: time.Now(),
		probe: func() (float64, error) { return 59.94444, nil }, stop: make(chan struct{}), done: make(chan struct{})}
	go rs.loop()
	d := rs.finish()
	if d["count"] != 1 || d["mean"] != 59.944 || d["unit"] != "fps" {
		t.Fatalf("%v", d)
	}
	rs = &runningSampler{kind: "cpu", interval: time.Hour, started: time.Now(),
		probe: func() (float64, error) { return 0, errors.New("adb gone") }, stop: make(chan struct{}), done: make(chan struct{})}
	go rs.loop()
	if d := rs.finish(); d["count"] != 0 || d["mean"] != 0.0 {
		t.Fatalf("%v", d)
	}
}

func TestSampler_NotASamplerStep(t *testing.T) {
	fr := &FlowRunner{script: NewScriptEngine(), driver: &mockDriver{}}
	if r := fr.executeSamplerStep(&flow.BackStep{}); r.Success {
		t.Fatal(r)
	}
	fr.stopSamplers() // nothing running: a no-op
	if got := driverPlatform(&mockDriver{platformFunc: func() *core.PlatformInfo { return nil }}); got != "device's" {
		t.Fatal(got)
	}
}

// wrappedDriver stands for the Flutter driver, which wraps a session's
// driver without forwarding its optional interfaces.
type wrappedDriver struct {
	core.Driver
}

func (w wrappedDriver) Inner() core.Driver { return w.Driver }

// The probe is the wrapped driver's: a wrapper does not hide it.
func TestSampler_ThroughAWrapper(t *testing.T) {
	s := deviceSession(t, wrappedDriver{mock.New(mock.Config{Platform: "android"})}, SessionConfig{AppID: "com.example"})
	if out := s.Execute(context.Background(), mustStep(t, "startSampler: memory")); !out.Success {
		t.Fatalf("start through a wrapper: %+v", out)
	}
	if out := s.Execute(context.Background(), mustStep(t, "stopSampler: memory")); !out.Success {
		t.Fatalf("stop: %+v", out)
	}
}
