package flow

import (
	"math"
	"strings"
	"testing"
)

func TestParseSetTrafficShape(t *testing.T) {
	step, err := ParseStep([]byte("setTrafficShape:\n  mbit: 1.5\n  lossPct: 2\n  latencyMs: 100\n  jitterMs: 10\n  direction: download"), "t.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s, ok := step.(*SetTrafficShapeStep)
	if !ok || s.Type() != StepSetTrafficShape {
		t.Fatalf("%T %v", step, step.Type())
	}
	if s.Mbit != 1.5 || s.LossPct != 2 || s.LatencyMs != 100 || s.JitterMs != 10 || s.Direction != "download" {
		t.Fatalf("%+v", s)
	}

	some, err := ParseStep([]byte("setTrafficShape:\n  latencyMs: 50"), "t.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if v := some.(*SetTrafficShapeStep); v.LatencyMs != 50 || v.Mbit != 0 || v.Direction != "" {
		t.Fatalf("%+v", v)
	}

	// No value at all: every field at its default.
	for _, y := range []string{"setTrafficShape:", "setTrafficShape: null", "setTrafficShape: ''"} {
		none, err := ParseStep([]byte(y), "t.yaml")
		if err != nil {
			t.Fatalf("%q: %v", y, err)
		}
		if v := none.(*SetTrafficShapeStep); v.Mbit != 0 || v.LossPct != 0 || v.LatencyMs != 0 || v.JitterMs != 0 || v.Direction != "" {
			t.Fatalf("%q: %+v", y, v)
		}
	}

	// A ${VAR} direction is left for the executor.
	vars, err := ParseStep([]byte("setTrafficShape:\n  direction: ${DIR}"), "t.yaml")
	if err != nil || vars.(*SetTrafficShapeStep).Direction != "${DIR}" {
		t.Fatalf("%v %+v", err, vars)
	}
}

func TestParseSetTrafficShape_Rejects(t *testing.T) {
	for y, want := range map[string]string{
		"setTrafficShape:\n  mbit: -1":            "mbit must be a number >= 0",
		"setTrafficShape:\n  lossPct: 101":        "lossPct must be at most 100",
		"setTrafficShape:\n  latencyMs: -5":       "latencyMs must be a number >= 0",
		"setTrafficShape:\n  jitterMs: -0.1":      "jitterMs must be a number >= 0",
		"setTrafficShape:\n  direction: sideways": `direction must be one of download, upload, both, got "sideways"`,
		"setTrafficShape:\n  mbit: fast":          "cannot unmarshal",
		"setTrafficShape: 5":                      "cannot unmarshal",
		"setTrafficShape:\n  lossPc: 100":         `setTrafficShape has no field "lossPc"; it takes direction, jitterMs, label, latencyMs, lossPct, mbit, optional, platform, timeout`,
	} {
		if _, err := ParseStep([]byte(y), "t.yaml"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", y, err, want)
		}
	}
}

func TestValidateTrafficShape_NonFinite(t *testing.T) {
	for _, s := range []*SetTrafficShapeStep{{Mbit: math.NaN()}, {LatencyMs: math.Inf(1)}} {
		if err := ValidateTrafficShape(s); err == nil {
			t.Errorf("%+v accepted", s)
		}
	}
	for _, d := range []string{"", "download", "upload", "both"} {
		if err := ValidateTrafficShapeDirection(d); err != nil {
			t.Errorf("%q: %v", d, err)
		}
	}
}

func TestParseResetTrafficShape_RefusesAValue(t *testing.T) {
	for y, want := range map[string]string{
		"resetTrafficShape:\n  lossPct: 5": `resetTrafficShape has no field "lossPct"; it takes label, optional, platform, timeout; to set a shape use setTrafficShape`,
		"resetTrafficShape: now":           "resetTrafficShape takes no value",
		"resetTrafficShape: [1]":           "resetTrafficShape takes no value",
	} {
		if _, err := ParseStep([]byte(y), "t.yaml"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want %q", y, err, want)
		}
	}
	// The options every step takes are read.
	step, err := ParseStep([]byte("resetTrafficShape:\n  optional: true\n  label: unshape"), "t.yaml")
	if err != nil || !step.IsOptional() || step.Label() != "unshape" || step.Type() != StepResetTrafficShape {
		t.Errorf("options: %v %+v", err, step)
	}
}

func TestParseResetTrafficShape(t *testing.T) {
	for _, y := range []string{"resetTrafficShape", "resetTrafficShape:"} {
		step, err := ParseStep([]byte(y), "t.yaml")
		if err != nil {
			t.Fatalf("%q: %v", y, err)
		}
		if _, ok := step.(*ResetTrafficShapeStep); !ok || step.Type() != StepResetTrafficShape {
			t.Fatalf("%q: %T", y, step)
		}
	}
}

func TestTrafficShapeStepsAreListed(t *testing.T) {
	found := map[StepType]bool{}
	for _, st := range StepTypes() {
		found[st] = true
	}
	if !found[StepSetTrafficShape] || !found[StepResetTrafficShape] || !IsStepType("setTrafficShape") || !IsStepType("resetTrafficShape") {
		t.Fatal("the traffic-shaping steps are not registered")
	}
}
