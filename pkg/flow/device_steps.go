package flow

// maestro-d: device steps a test needs beyond upstream's: the status bar's
// demo mode, whether the software keyboard is up, and the performance
// samplers.

import (
	"fmt"
	"strings"
)

// Status bar modes, as setStatusBar names them.
const (
	StatusBarDemo = "demo"
	StatusBarLive = "live"
)

// Sampler kinds, as startSampler and stopSampler name them.
const (
	SamplerCPU    = "cpu"
	SamplerMemory = "memory"
	SamplerFrames = "frames"
)

// SamplerKinds lists the accepted sampler kinds.
var SamplerKinds = []string{SamplerCPU, SamplerMemory, SamplerFrames}

// DefaultStatusBarTime and DefaultStatusBarBattery are what demo mode shows
// when the step names neither.
const (
	DefaultStatusBarTime    = "9:41"
	DefaultStatusBarBattery = 100
)

// SetStatusBarStep puts the status bar into demo mode (a fixed clock, a full battery, full signal, no notifications) or back to live: simctl status_bar on an iOS simulator, System UI demo mode on Android. A physical iPhone, iPad or Apple TV has no override and answers UNSUPPORTED, as do the browser and the computer device; supportsStatusBar(device) in the JavaScript client tells ahead.
type SetStatusBarStep struct {
	BaseStep `yaml:",inline"`
	Mode     string `yaml:"mode"`    // demo or live
	Time     string `yaml:"time"`    // The clock demo mode shows, H:MM (default 9:41)
	Battery  *int   `yaml:"battery"` // The battery level demo mode shows, 0–100 (default 100)
}

// Describe names the mode.
func (s *SetStatusBarStep) Describe() string {
	return fmt.Sprintf("setStatusBar: %s", s.Mode)
}

// EffectiveTime is the clock demo mode shows.
func (s *SetStatusBarStep) EffectiveTime() string {
	if t := strings.TrimSpace(s.Time); t != "" {
		return t
	}
	return DefaultStatusBarTime
}

// EffectiveBattery is the battery level demo mode shows.
func (s *SetStatusBarStep) EffectiveBattery() int {
	if s.Battery != nil {
		return *s.Battery
	}
	return DefaultStatusBarBattery
}

// ValidateStatusBar checks a setStatusBar step's values.
//
// A mode or time that still holds a ${VAR} is checked once the executor
// has expanded it.
func ValidateStatusBar(s *SetStatusBarStep) error {
	switch {
	case s.Mode == StatusBarDemo, s.Mode == StatusBarLive, strings.Contains(s.Mode, "${"):
	default:
		return fmt.Errorf("setStatusBar: mode must be %s or %s, got %q", StatusBarDemo, StatusBarLive, s.Mode)
	}
	if s.Battery != nil && (*s.Battery < 0 || *s.Battery > 100) {
		return fmt.Errorf("setStatusBar: battery must be 0–100, got %d", *s.Battery)
	}
	if t := strings.TrimSpace(s.Time); t != "" && !strings.Contains(t, "${") && !validClock(t) {
		return fmt.Errorf("setStatusBar: time must be H:MM or HH:MM, got %q", s.Time)
	}
	return nil
}

func validClock(t string) bool {
	h, m, ok := strings.Cut(t, ":")
	if !ok || len(h) < 1 || len(h) > 2 || len(m) != 2 {
		return false
	}
	var hh, mm int
	if _, err := fmt.Sscanf(h+" "+m, "%d %d", &hh, &mm); err != nil {
		return false
	}
	return hh >= 0 && hh <= 23 && mm >= 0 && mm <= 59
}

// KeyboardVisibleStep reports whether the software keyboard is on screen, as data {visible}, on Android (uiautomator2 and devicelab drivers) and iOS (WebDriverAgent).
type KeyboardVisibleStep struct {
	BaseStep `yaml:",inline"`
}

// StartSamplerStep starts sampling the app's cpu (percent), memory (bytes) or frames (fps) in the background until stopSampler, answering {kind, appId, intervalMs}: all three on Android, cpu and memory on an iOS simulator; anything else answers UNSUPPORTED.
type StartSamplerStep struct {
	BaseStep   `yaml:",inline"`
	Kind       string `yaml:"kind"`       // cpu, memory or frames
	AppID      string `yaml:"appId"`      // The app to sample (default: the session's app)
	IntervalMs int    `yaml:"intervalMs"` // Time between samples in milliseconds (default 1000, at least 100)
}

// Describe names the kind.
func (s *StartSamplerStep) Describe() string { return fmt.Sprintf("startSampler: %s", s.Kind) }

// StopSamplerStep stops a sampler startSampler started and answers {kind, unit, appId, intervalMs, durationMs, count, min, max, mean, samples: [{atMs, value}]}, the same fields on every platform.
type StopSamplerStep struct {
	BaseStep `yaml:",inline"`
	Kind     string `yaml:"kind"` // cpu, memory or frames
}

// Describe names the kind.
func (s *StopSamplerStep) Describe() string { return fmt.Sprintf("stopSampler: %s", s.Kind) }

// DefaultSamplerIntervalMs is a sampler's interval when the step names none.
const DefaultSamplerIntervalMs = 1000

// ValidateSamplerKind checks a sampler step's kind. A kind that still
// holds a ${VAR} is checked once the executor has expanded it.
func ValidateSamplerKind(step, kind string) error {
	if strings.Contains(kind, "${") {
		return nil
	}
	for _, k := range SamplerKinds {
		if kind == k {
			return nil
		}
	}
	return fmt.Errorf("%s: kind must be one of %s, got %q", step, strings.Join(SamplerKinds, ", "), kind)
}

// ValidateStartSampler checks a startSampler step's values.
func ValidateStartSampler(s *StartSamplerStep) error {
	if err := ValidateSamplerKind("startSampler", s.Kind); err != nil {
		return err
	}
	if s.IntervalMs != 0 && s.IntervalMs < 100 {
		return fmt.Errorf("startSampler: intervalMs must be at least 100, got %d", s.IntervalMs)
	}
	return nil
}

// EffectiveIntervalMs is the sampler's interval.
func (s *StartSamplerStep) EffectiveIntervalMs() int {
	if s.IntervalMs > 0 {
		return s.IntervalMs
	}
	return DefaultSamplerIntervalMs
}

// AIThresholds overrides the AI checks' stopping points. Each is optional.
type AIThresholds struct {
	// JevPass is the noul at or above which Jev's answer passes (default 0.7).
	JevPass *float64 `yaml:"jevPass" json:"jevPass,omitempty"`
	// JevFail is the noul at or below which Jev's answer fails (default 0.3).
	JevFail *float64 `yaml:"jevFail" json:"jevFail,omitempty"`
	// Haiku, Sonnet and Opus are the confidence each model's pass or fail
	// needs to stop the waterfall (default 0.7 each). A value above 1
	// never stops there.
	Haiku  *float64 `yaml:"haiku" json:"haiku,omitempty"`
	Sonnet *float64 `yaml:"sonnet" json:"sonnet,omitempty"`
	Opus   *float64 `yaml:"opus" json:"opus,omitempty"`
}

// AIOptions are the options both AI checks take.
type AIOptions struct {
	// The most the check may spend, in US dollars: the waterfall stops
	// before a call that would pass it and answers unsure (0 = no limit of
	// the caller's own; a daemon spending its own keys caps it at its
	// maxBudgetUsdPerCall).
	BudgetUSD float64 `yaml:"budgetUsd"`
	// Stopping points: jevPass (default 0.7), jevFail (0.3), haiku, sonnet
	// and opus (0.7 each); a value above 1 never stops there.
	Thresholds *AIThresholds `yaml:"thresholds"`
}

// ValidateAIOptions checks an AI check's options.
func ValidateAIOptions(step string, o AIOptions) error {
	if o.BudgetUSD < 0 {
		return fmt.Errorf("%s: budgetUsd must not be negative, got %v", step, o.BudgetUSD)
	}
	if t := o.Thresholds; t != nil {
		for name, v := range map[string]*float64{"jevPass": t.JevPass, "jevFail": t.JevFail, "haiku": t.Haiku, "sonnet": t.Sonnet, "opus": t.Opus} {
			if v != nil && *v < 0 {
				return fmt.Errorf("%s: thresholds.%s must not be negative, got %v", step, name, *v)
			}
		}
	}
	return nil
}
