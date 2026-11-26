package flow

import (
	"strings"
	"testing"
)

func TestDeviceSteps_Parse(t *testing.T) {
	s, err := ParseStep([]byte("setStatusBar: demo"), "t")
	if sb, ok := s.(*SetStatusBarStep); err != nil || !ok || sb.Mode != "demo" || sb.EffectiveTime() != "9:41" || sb.EffectiveBattery() != 100 || sb.Describe() != "setStatusBar: demo" {
		t.Fatalf("%#v %v", s, err)
	}
	s, err = ParseStep([]byte("setStatusBar:\n  mode: live\n  time: '23:59'\n  battery: 0"), "t")
	if sb, ok := s.(*SetStatusBarStep); err != nil || !ok || sb.EffectiveTime() != "23:59" || sb.EffectiveBattery() != 0 {
		t.Fatalf("%#v %v", s, err)
	}
	if s, err := ParseStep([]byte("setStatusBar: ${MODE}"), "t"); err != nil || s.(*SetStatusBarStep).Mode != "${MODE}" {
		t.Fatalf("a variable mode: %v", err)
	}
	if s, err := ParseStep([]byte("keyboardVisible"), "t"); err != nil || s.Type() != StepKeyboardVisible {
		t.Fatalf("%#v %v", s, err)
	}
	s, err = ParseStep([]byte("startSampler:\n  kind: frames\n  appId: com.x\n  intervalMs: 250"), "t")
	if st, ok := s.(*StartSamplerStep); err != nil || !ok || st.EffectiveIntervalMs() != 250 || st.Describe() != "startSampler: frames" {
		t.Fatalf("%#v %v", s, err)
	}
	if s, _ := ParseStep([]byte("startSampler: cpu"), "t"); s.(*StartSamplerStep).EffectiveIntervalMs() != 1000 {
		t.Fatal("default interval")
	}
	s, err = ParseStep([]byte("stopSampler:\n  kind: memory"), "t")
	if st, ok := s.(*StopSamplerStep); err != nil || !ok || st.Describe() != "stopSampler: memory" {
		t.Fatalf("%#v %v", s, err)
	}
	if _, err := ParseStep([]byte("stopSampler: ${K}"), "t"); err != nil {
		t.Fatal(err)
	}
}

func TestDeviceSteps_Refused(t *testing.T) {
	for y, want := range map[string]string{
		"setStatusBar: sideways":                       "mode must be demo or live",
		"setStatusBar:\n  mode: demo\n  battery: 101":  "battery must be 0–100",
		"setStatusBar:\n  mode: demo\n  time: '9:4'":   "time must be H:MM",
		"setStatusBar:\n  mode: demo\n  time: '25:00'": "time must be H:MM",
		"setStatusBar:\n  mode: demo\n  time: 'ab:cd'": "time must be H:MM",
		"setStatusBar:\n  mode: [x]":                   "cannot unmarshal",
		"startSampler: heat":                           "kind must be one of cpu, memory, frames",
		"startSampler:\n  kind: cpu\n  intervalMs: 50": "intervalMs must be at least 100",
		"startSampler:\n  kind: [x]":                   "cannot unmarshal",
		"stopSampler: heat":                            "kind must be one of",
		"stopSampler:\n  kind: [x]":                    "cannot unmarshal",
	} {
		if _, err := ParseStep([]byte(y), "t"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", y, err, want)
		}
	}
}
