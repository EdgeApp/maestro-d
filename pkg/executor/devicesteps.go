package executor

// maestro-d: setStatusBar's platform gate, and startSampler/stopSampler,
// which the executor runs on a ticker with the driver's probe
// (core.SampleProber) and stops at the session's end.

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
	"github.com/devicelab-dev/maestro-runner/pkg/logger"
)

// StatusBarPhysicalIOSMessage is setStatusBar's answer on a physical
// iPhone, iPad or Apple TV, whose status bar iOS lets nothing override.
const StatusBarPhysicalIOSMessage = core.StatusBarPhysicalIOSMessage

// executeStatusBarStep refuses setStatusBar where there is no status bar to
// override (a physical Apple device, a browser, the computer device) and
// otherwise hands it to the driver.
func (fr *FlowRunner) executeStatusBarStep(step *flow.SetStatusBarStep) *core.CommandResult {
	fr.script.ExpandStep(step)
	if err := flow.ValidateStatusBar(step); err != nil {
		return errorResult(&UsageError{Err: err})
	}
	if info := fr.driver.GetPlatformInfo(); info != nil {
		switch p := strings.ToLower(info.Platform); {
		case isApplePlatform(p) && !info.IsSimulator:
			return unsupportedResult(StatusBarPhysicalIOSMessage)
		case p == "web" || p == "macos":
			return unsupportedResult(fmt.Sprintf("setStatusBar: a %s device has no status bar to override", p))
		}
	}
	return fr.driver.Execute(step)
}

// sample is one reading, atMs after the sampler started.
type sample struct {
	AtMs  int64   `json:"atMs"`
	Value float64 `json:"value"`
}

// runningSampler is one kind's sampler in a session.
type runningSampler struct {
	kind, appID string
	interval    time.Duration
	started     time.Time
	probe       core.SampleFunc

	mu      sync.Mutex
	samples []sample

	stop chan struct{}
	done chan struct{}
}

// samplerSet holds a session's running samplers, by kind.
type samplerSet struct {
	mu      sync.Mutex
	running map[string]*runningSampler
}

// samplerNow is time.Now, a variable so tests control the clock.
var samplerNow = time.Now

func (s *runningSampler) read() {
	v, err := s.probe()
	if err != nil {
		if !errors.Is(err, core.ErrNoSample) {
			logger.Debug("sampler %s %s: %v", s.kind, s.appID, err)
		}
		return
	}
	s.mu.Lock()
	s.samples = append(s.samples, sample{AtMs: samplerNow().Sub(s.started).Milliseconds(), Value: v})
	s.mu.Unlock()
}

func (s *runningSampler) loop() {
	defer close(s.done)
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.read()
		}
	}
}

// finish stops the loop, takes a last reading when there was none, and
// returns the summary stopSampler answers.
func (s *runningSampler) finish() map[string]any {
	close(s.stop)
	<-s.done
	s.mu.Lock()
	empty := len(s.samples) == 0
	s.mu.Unlock()
	if empty {
		s.read()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	samples := append([]sample{}, s.samples...)
	var lo, hi, sum float64
	for i, x := range samples {
		if i == 0 || x.Value < lo {
			lo = x.Value
		}
		if i == 0 || x.Value > hi {
			hi = x.Value
		}
		sum += x.Value
	}
	mean := 0.0
	if len(samples) > 0 {
		mean = sum / float64(len(samples))
	}
	return map[string]any{
		"kind": s.kind, "unit": core.SampleUnits[s.kind], "appId": s.appID,
		"intervalMs": s.interval.Milliseconds(), "durationMs": samplerNow().Sub(s.started).Milliseconds(),
		"count": len(samples), "min": round3(lo), "max": round3(hi), "mean": round3(mean), "samples": samples,
	}
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

// executeSamplerStep runs startSampler and stopSampler.
func (fr *FlowRunner) executeSamplerStep(step flow.Step) *core.CommandResult {
	fr.script.ExpandStep(step)
	if fr.samplers == nil {
		fr.samplers = &samplerSet{running: map[string]*runningSampler{}}
	}
	set := fr.samplers
	switch s := step.(type) {
	case *flow.StartSamplerStep:
		if err := flow.ValidateStartSampler(s); err != nil {
			return errorResult(&UsageError{Err: err})
		}
		appID := s.AppID
		if appID == "" {
			appID = fr.flow.Config.EffectiveAppID()
		}
		if appID == "" {
			return errorResult(&UsageError{Err: errors.New("startSampler: no appId, and the session has no app")})
		}
		set.mu.Lock()
		defer set.mu.Unlock()
		if _, ok := set.running[s.Kind]; ok {
			return errorResult(&UsageError{Err: fmt.Errorf("startSampler: a %s sampler is already running; stopSampler it first", s.Kind)})
		}
		// A wrapper (the Flutter driver wraps every Android and iOS session)
		// does not forward the probe: ask the driver it wraps.
		prober, ok := core.Unwrap(fr.driver).(core.SampleProber)
		if !ok {
			return unsupportedResult(fmt.Sprintf("startSampler: the %s driver cannot measure an app", driverPlatform(fr.driver)))
		}
		probe, err := prober.SampleProbe(s.Kind, appID)
		var unsupported *core.UnsupportedError
		if errors.As(err, &unsupported) {
			return unsupportedResult("startSampler: " + unsupported.Msg)
		}
		if err != nil {
			return errorResult(fmt.Errorf("startSampler: %w", err))
		}
		rs := &runningSampler{kind: s.Kind, appID: appID, interval: time.Duration(s.EffectiveIntervalMs()) * time.Millisecond,
			started: samplerNow(), probe: probe, stop: make(chan struct{}), done: make(chan struct{})}
		set.running[s.Kind] = rs
		go rs.loop()
		return &core.CommandResult{Success: true, Message: fmt.Sprintf("%s sampler started for %s", s.Kind, appID),
			Data: map[string]any{"kind": s.Kind, "appId": appID, "intervalMs": rs.interval.Milliseconds()}}
	case *flow.StopSamplerStep:
		if err := flow.ValidateSamplerKind("stopSampler", s.Kind); err != nil {
			return errorResult(&UsageError{Err: err})
		}
		set.mu.Lock()
		rs, ok := set.running[s.Kind]
		delete(set.running, s.Kind)
		set.mu.Unlock()
		if !ok {
			return errorResult(fmt.Errorf("stopSampler: no %s sampler is running; startSampler it first", s.Kind))
		}
		data := rs.finish()
		return &core.CommandResult{Success: true, Message: fmt.Sprintf("%s sampler stopped: %d samples", s.Kind, data["count"]), Data: data}
	}
	return errorResult(fmt.Errorf("not a sampler step: %s", step.Type()))
}

// stopSamplers stops every sampler still running, at the session's end.
func (fr *FlowRunner) stopSamplers() {
	set := fr.samplers
	if set == nil {
		return
	}
	set.mu.Lock()
	running := set.running
	set.running = map[string]*runningSampler{}
	set.mu.Unlock()
	for _, rs := range running {
		close(rs.stop)
		<-rs.done
	}
}

func driverPlatform(d core.Driver) string {
	if info := d.GetPlatformInfo(); info != nil && info.Platform != "" {
		return info.Platform
	}
	return "device's"
}
