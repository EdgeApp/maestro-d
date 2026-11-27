package flow

import (
	"strings"
	"testing"
)

func TestParsePointerSteps(t *testing.T) {
	s, err := ParseStep([]byte("hoverOn: Menu"), "t")
	if h, ok := s.(*HoverOnStep); err != nil || !ok || h.Selector.Text != "Menu" || h.Describe() != `hoverOn: text="Menu"` {
		t.Fatalf("hoverOn scalar: %#v %v", s, err)
	}
	s, err = ParseStep([]byte("hoverOn:\n  point: 10%, 20%\n  duration: 300"), "t")
	if h := s.(*HoverOnStep); err != nil || h.DurationMs != 300 || h.Describe() != "hoverOn: point 10%, 20%" {
		t.Fatalf("hoverOn point: %#v %v", s, err)
	}
	s, err = ParseStep([]byte("holdOn: Play"), "t")
	if h := s.(*HoldOnStep); err != nil || h.EffectiveDurationMs() != DefaultHoldMs || h.Describe() != `holdOn: text="Play" for 1000ms` {
		t.Fatalf("holdOn scalar: %#v %v", s, err)
	}
	s, err = ParseStep([]byte("holdOn:\n  id: v\n  point: 5%, 5%\n  duration: 2500"), "t")
	if h := s.(*HoldOnStep); err != nil || h.EffectiveDurationMs() != 2500 || h.Describe() != `holdOn: id="v" at 5%, 5% for 2500ms` {
		t.Fatalf("holdOn map: %#v %v", s, err)
	}
	s, err = ParseStep([]byte("dragPath:\n  points: [\"10%, 50%\", \"50%, 20%\", \"90%, 50%\"]\n  holdDuration: 200"), "t")
	d := s.(*DragPathStep)
	if err != nil || len(d.Points) != 3 || d.HoldDuration != 200 || d.EffectiveDurationMs() != DefaultDragPathMs ||
		d.Describe() != "dragPath: 10%, 50% → 90%, 50% (3 points)" {
		t.Fatalf("dragPath: %#v %v", s, err)
	}
	if (&DragPathStep{DurationMs: 40}).EffectiveDurationMs() != 40 || (&DragPathStep{}).Describe() != "dragPath" {
		t.Fatal("dragPath defaults")
	}
}

func TestParsePointerSteps_Invalid(t *testing.T) {
	for y, want := range map[string]string{
		"hoverOn:\n  duration: 5":                                   "hoverOn needs an element or a point",
		"holdOn:\n  point: 1, 2\n  duration: -1":                    "holdOn: duration must not be negative",
		"dragPath:\n  points: [\"1, 2\"]":                           "at least two points",
		"dragPath:\n  points: [\"1, 2\", \" \"]":                    "point 2 is empty",
		"dragPath:\n  points: [\"1, 2\", \"3, 4\"]\n  duration: -1": "must not be negative",
		"dragPath: nonsense":                                        "cannot unmarshal",
		"hoverOn:\n  duration: [1]":                                 "cannot unmarshal",
		"holdOn:\n  duration: [1]":                                  "cannot unmarshal",
	} {
		if _, err := ParseStep([]byte(y), "t"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", y, err, want)
		}
	}
	if ValidatePointerStep(&TapOnStep{}) != nil {
		t.Error("another step is not a pointer step's to refuse")
	}
}
