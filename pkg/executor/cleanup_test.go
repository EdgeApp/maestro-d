package executor

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/driver/mock"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
	"github.com/devicelab-dev/maestro-runner/pkg/netshape"
)

func shortCleanup(t *testing.T, d time.Duration) {
	t.Helper()
	old := cleanupBound
	cleanupBound = d
	t.Cleanup(func() { cleanupBound = old })
}

func TestCleanup_ResetsWhatTheSessionShaped(t *testing.T) {
	for _, c := range []struct {
		name   string
		steps  []string
		err    error
		resets int // by the steps and the cleanup together
	}{
		{"nothing shaped", nil, nil, 0},
		{"shaped", []string{"setTrafficShape:\n  lossPct: 100"}, nil, 1},
		{"shaped twice", []string{"setTrafficShape:", "setTrafficShape:\n  mbit: 1"}, nil, 1},
		{"reset last", []string{"setTrafficShape:", "resetTrafficShape"}, nil, 1},
		{"reset, then shaped again", []string{"setTrafficShape:", "resetTrafficShape", "setTrafficShape:"}, nil, 2},
		{"a failed call may have applied", []string{"setTrafficShape:"}, &netshape.FetchError{Command: "setTrafficShape", Status: 500}, 1},
		{"not configured sends nothing", []string{"setTrafficShape:"}, netshape.ErrNotConfigured, 0},
		{"a bad config sends nothing", []string{"setTrafficShape:"}, &netshape.ConfigError{Path: "p", Err: errors.New("x")}, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			sh := &stubShaper{err: c.err}
			s := shapingSession(t, sh, nil)
			for _, y := range c.steps {
				s.Execute(context.Background(), mustStep(t, y))
			}
			s.Cleanup(context.Background())
			s.Cleanup(context.Background()) // a second cleanup sends nothing more
			// A failed reset still counts as sent.
			if sh.resets != c.resets {
				t.Fatalf("resets = %d, want %d", sh.resets, c.resets)
			}
		})
	}
}

func TestCleanup_ClosedSessionDoesNothing(t *testing.T) {
	sh := &stubShaper{}
	s := shapingSession(t, sh, nil)
	s.Execute(context.Background(), mustStep(t, "setTrafficShape:"))
	s.Close()
	s.Cleanup(context.Background())
	if sh.resets != 0 {
		t.Fatalf("resets = %d", sh.resets)
	}
}

// airplaneDriver records the airplane steps it runs; block, when set,
// makes them hang without looking at their context.
type airplaneDriver struct {
	*mock.Driver
	mu    sync.Mutex
	seen  []string
	block chan struct{}
}

func (d *airplaneDriver) Execute(step flow.Step) *core.CommandResult {
	if s, ok := step.(*flow.SetAirplaneModeStep); ok {
		d.mu.Lock()
		d.seen = append(d.seen, map[bool]string{true: "on", false: "off"}[s.Enabled])
		block := d.block
		d.mu.Unlock()
		if block != nil {
			<-block
		}
	}
	return d.Driver.Execute(step)
}

func (d *airplaneDriver) got() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.seen...)
}

func airplaneSession(t *testing.T, platform string, shaper TrafficShaper) (*Session, *airplaneDriver) {
	t.Helper()
	d := &airplaneDriver{Driver: mock.New(mock.Config{Platform: platform, DeviceID: "dev-1"})}
	s, err := NewSession(context.Background(), d, RunnerConfig{OutputDir: reportDir(t)}, SessionConfig{TrafficShaper: shaper})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, d
}

func TestCleanup_TurnsAndroidAirplaneModeOff(t *testing.T) {
	for _, y := range []string{"setAirplaneMode: enabled", "setAirplaneMode: disabled", "toggleAirplaneMode"} {
		s, d := airplaneSession(t, "android", nil)
		s.Execute(context.Background(), mustStep(t, y))
		before := len(d.got())
		s.Cleanup(context.Background())
		if got := d.got(); len(got) != before+1 || got[len(got)-1] != "off" {
			t.Fatalf("%s: the driver saw %v", y, got)
		}
	}
	// Untouched, or refused on iOS: nothing to undo.
	s, d := airplaneSession(t, "android", nil)
	s.Execute(context.Background(), mustStep(t, "back"))
	s.Cleanup(context.Background())
	if len(d.got()) != 0 {
		t.Fatalf("untouched: %v", d.got())
	}
	s, d = airplaneSession(t, "ios", nil)
	s.Execute(context.Background(), mustStep(t, "setAirplaneMode: enabled"))
	s.Cleanup(context.Background())
	if len(d.got()) != 0 {
		t.Fatalf("ios: %v", d.got())
	}
}

func TestCleanup_AirplaneFailureIsLoggedAndTheResetStillSent(t *testing.T) {
	sh := &stubShaper{}
	d := &failingAirplane{Driver: mock.New(mock.Config{Platform: "android"})}
	s, err := NewSession(context.Background(), d, RunnerConfig{OutputDir: t.TempDir()}, SessionConfig{TrafficShaper: sh})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Execute(context.Background(), mustStep(t, "setTrafficShape:"))
	s.Execute(context.Background(), mustStep(t, "toggleAirplaneMode"))
	s.Cleanup(context.Background())
	if sh.resets != 1 {
		t.Fatalf("resets = %d", sh.resets)
	}
}

type failingAirplane struct{ *mock.Driver }

func (d *failingAirplane) Execute(step flow.Step) *core.CommandResult {
	if _, ok := step.(*flow.SetAirplaneModeStep); ok {
		return &core.CommandResult{Success: false, Error: errors.New("adb: device offline")}
	}
	return d.Driver.Execute(step)
}

// A driver call that ignores its context is abandoned at the bound, and
// the reset is still sent.
func TestCleanup_AHungAirplaneCallIsAbandonedAtTheBound(t *testing.T) {
	shortCleanup(t, 100*time.Millisecond)
	sh := &stubShaper{}
	s, d := airplaneSession(t, "android", sh)
	s.Execute(context.Background(), mustStep(t, "setTrafficShape:"))
	s.Execute(context.Background(), mustStep(t, "setAirplaneMode: enabled"))
	d.mu.Lock()
	d.block = make(chan struct{})
	d.mu.Unlock()
	defer close(d.block)
	started := time.Now()
	s.Cleanup(context.Background())
	if took := time.Since(started); took > 2*time.Second {
		t.Fatalf("cleanup took %s", took)
	}
	if sh.resets != 1 {
		t.Fatalf("resets = %d", sh.resets)
	}
}

// hangingShaper's reset waits for its context.
type hangingShaper struct{ stubShaper }

func (h *hangingShaper) Reset(ctx context.Context) (netshape.Result, error) {
	<-ctx.Done()
	return netshape.Result{}, ctx.Err()
}

func TestCleanup_AHungResetIsBounded(t *testing.T) {
	shortCleanup(t, 100*time.Millisecond)
	sh := &hangingShaper{}
	s := shapingSession(t, sh, nil)
	s.Execute(context.Background(), mustStep(t, "setTrafficShape:"))
	started := time.Now()
	s.Cleanup(context.Background())
	if took := time.Since(started); took > 2*time.Second {
		t.Fatalf("cleanup took %s", took)
	}
}

func TestResultMessage(t *testing.T) {
	for r, want := range map[*core.CommandResult]string{
		nil:                      "no result",
		{Message: "m"}:           "m",
		{Error: errors.New("e")}: "e",
		{Success: false}:         "failed",
	} {
		if got := resultMessage(r); got != want {
			t.Errorf("resultMessage(%+v) = %q, want %q", r, got, want)
		}
	}
}

func TestCleanup_NamesAnUnknownDevice(t *testing.T) {
	sh := &stubShaper{}
	s, err := NewSession(context.Background(), noInfoDriver{mock.New(mock.Config{})}, RunnerConfig{OutputDir: t.TempDir()}, SessionConfig{TrafficShaper: sh})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Execute(context.Background(), mustStep(t, "setTrafficShape:"))
	s.Cleanup(context.Background())
	if sh.resets != 1 {
		t.Fatalf("resets = %d", sh.resets)
	}
}

// countingDriver counts GetPlatformInfo calls.
type countingDriver struct {
	*mock.Driver
	infos int
}

func (d *countingDriver) GetPlatformInfo() *core.PlatformInfo {
	d.infos++
	return d.Driver.GetPlatformInfo()
}

// With nothing to undo, cleanup never calls the driver: on a real device
// GetPlatformInfo can be a slow call at teardown.
func TestCleanup_NothingToUndoMakesNoDriverCall(t *testing.T) {
	d := &countingDriver{Driver: mock.New(mock.Config{Platform: "android"})}
	s, err := NewSession(context.Background(), d, RunnerConfig{OutputDir: reportDir(t)}, SessionConfig{TrafficShaper: &stubShaper{}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	before := d.infos
	s.Cleanup(context.Background())
	if d.infos != before {
		t.Fatalf("GetPlatformInfo called %d times", d.infos-before)
	}
}

// panickyCleanupDriver panics where the cleanup calls the driver.
type panickyCleanupDriver struct {
	*mock.Driver
	inInfo, inStep bool
	armed          bool
}

func (d *panickyCleanupDriver) GetPlatformInfo() *core.PlatformInfo {
	if d.armed && d.inInfo {
		panic("platform info")
	}
	return d.Driver.GetPlatformInfo()
}

func (d *panickyCleanupDriver) Execute(step flow.Step) *core.CommandResult {
	if _, ok := step.(*flow.SetAirplaneModeStep); ok && d.armed && d.inStep {
		panic("adb gone")
	}
	return d.Driver.Execute(step)
}

// A driver that panics during the cleanup never takes the process down,
// and the reset is still sent.
func TestCleanup_ContainsDriverPanics(t *testing.T) {
	for _, c := range []struct{ inInfo, inStep bool }{{true, false}, {false, true}, {true, true}} {
		sh := &stubShaper{}
		d := &panickyCleanupDriver{Driver: mock.New(mock.Config{Platform: "android"}), inInfo: c.inInfo, inStep: c.inStep}
		s, err := NewSession(context.Background(), d, RunnerConfig{OutputDir: reportDir(t)}, SessionConfig{TrafficShaper: sh})
		if err != nil {
			t.Fatal(err)
		}
		s.Execute(context.Background(), mustStep(t, "setTrafficShape:"))
		s.Execute(context.Background(), mustStep(t, "setAirplaneMode: enabled"))
		d.armed = true
		s.Cleanup(context.Background())
		s.Close()
		if sh.resets != 1 {
			t.Fatalf("%+v: resets = %d", c, sh.resets)
		}
	}
}

// panickyShaper panics in its reset.
type panickyShaper struct{ stubShaper }

func (*panickyShaper) Reset(context.Context) (netshape.Result, error) { panic("shaper") }

func TestCleanup_ContainsAShaperPanic(t *testing.T) {
	s := shapingSession(t, &panickyShaper{}, nil)
	s.Execute(context.Background(), mustStep(t, "setTrafficShape:"))
	s.Cleanup(context.Background()) // returns: the panic is logged, not raised
}

// A value the shaper refuses before sending is USAGE, and nothing is left
// to undo.
func TestCleanup_ARefusedValueSendsNothing(t *testing.T) {
	sh := &stubShaper{err: &netshape.ValueError{Msg: "lossPct must be at most 100, got 101"}}
	s := shapingSession(t, sh, nil)
	out := s.Execute(context.Background(), mustStep(t, "setTrafficShape:"))
	if out.Success || out.Code != codeUsage {
		t.Fatalf("%+v", out)
	}
	s.Cleanup(context.Background())
	if sh.resets != 0 {
		t.Fatalf("resets = %d", sh.resets)
	}
}

// After the cleanup no step runs: one queued behind it (an airplane step
// waiting on the session lock during a detach) cannot redo the undo.
func TestCleanup_NoStepRunsAfterIt(t *testing.T) {
	s, d := airplaneSession(t, "android", nil)
	s.Execute(context.Background(), mustStep(t, "setAirplaneMode: enabled"))
	s.Cleanup(context.Background())
	before := len(d.got())
	out := s.Execute(context.Background(), mustStep(t, "setAirplaneMode: enabled"))
	if out.Success || out.Error == nil || out.Error.Message != "session is closed" {
		t.Fatalf("%+v", out)
	}
	if got := d.got(); len(got) != before || got[len(got)-1] != "off" {
		t.Fatalf("the driver saw %v", got)
	}
	if _, err := s.Screenshot(context.Background()); err == nil {
		t.Fatal("a screenshot after the cleanup")
	}
}
