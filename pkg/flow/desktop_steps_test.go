package flow

import (
	"strings"
	"testing"
)

func TestParseMoveMouse(t *testing.T) {
	for _, y := range []string{`moveMouse: "40%, 20%"`, "moveMouse:\n  point: 40%, 20%", `{"moveMouse": {"point": "40%, 20%"}}`} {
		step, err := ParseStep([]byte(y), "t.yaml")
		if err != nil {
			t.Fatalf("%s: %v", y, err)
		}
		s, ok := step.(*MoveMouseStep)
		if !ok || s.Type() != StepMoveMouse || s.Point != "40%, 20%" {
			t.Errorf("%s: %#v", y, step)
		}
	}
	if _, err := ParseStep([]byte("moveMouse: ''"), "t.yaml"); err == nil || !strings.Contains(err.Error(), "needs a point") {
		t.Errorf("empty point: %v", err)
	}
}

func TestParseClickOn(t *testing.T) {
	step, err := ParseStep([]byte(`clickOn: "800, 600"`), "t.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if s := step.(*ClickOnStep); s.Point != "800, 600" || s.Button != "left" || s.Count != 1 || s.Type() != StepClickOn {
		t.Errorf("defaults %#v", s)
	}
	step, err = ParseStep([]byte("clickOn:\n  point: 50%, 50%\n  button: Right\n  count: 2"), "t.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if s := step.(*ClickOnStep); s.Button != "right" || s.Count != 2 {
		t.Errorf("%#v", s)
	}
	// Bare clickOn clicks where the pointer is.
	step, err = ParseStep([]byte("clickOn"), "t.yaml")
	if err != nil || step.(*ClickOnStep).Point != "" {
		t.Errorf("bare: %#v %v", step, err)
	}
	for _, bad := range []string{"clickOn:\n  count: 4", "clickOn:\n  count: -1", "clickOn:\n  count: 0", "clickOn:\n  button: side"} {
		if _, err := ParseStep([]byte(bad), "t.yaml"); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestParseScrollAt(t *testing.T) {
	step, err := ParseStep([]byte("scrollAt:\n  point: 50%, 50%\n  y: 5"), "t.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if s := step.(*ScrollAtStep); s.Point != "50%, 50%" || s.Y != 5 || s.X != 0 || s.Type() != StepScrollAt {
		t.Errorf("%#v", s)
	}
	for bad, want := range map[string]string{
		"scrollAt:\n  y: 5":                  "needs a point",
		"scrollAt:\n  point: 1, 1":           "x or y",
		"scrollAt:\n  point: 1, 1\n  y: 51":  "between -50 and 50",
		"scrollAt:\n  point: 1, 1\n  x: -51": "between -50 and 50",
		"scrollAt: 1, 1":                     "",
	} {
		_, err := ParseStep([]byte(bad), "t.yaml")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestStepTypes_IncludeDesktopSteps(t *testing.T) {
	types := StepTypes()
	idx := map[StepType]int{}
	for i, s := range types {
		idx[s] = i
	}
	for _, s := range []StepType{StepMoveMouse, StepClickOn, StepScrollAt} {
		if _, ok := idx[s]; !ok {
			t.Errorf("%s missing from StepTypes", s)
		}
		if !IsStepType(string(s)) {
			t.Errorf("%s not a step type", s)
		}
	}
	if !(idx[StepMoveMouse] < idx[StepClickOn] && idx[StepClickOn] < idx[StepScrollAt]) {
		t.Error("desktop steps out of order")
	}
}
