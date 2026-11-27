package flow

// maestro-d: pointer gestures upstream Maestro has no command for: a hover,
// a press held for a set time, and a drag along a path of points.

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// HoverOnStep moves the pointer onto an element (at its point:, default the centre) or a point on the screen without pressing, then waits duration: the browser and the computer device. A touch screen has no hover: Android and iOS answer UNSUPPORTED.
type HoverOnStep struct {
	BaseStep   `yaml:",inline"`
	Selector   Selector `yaml:",inline"`
	DurationMs int      `yaml:"duration"` // How long to stay before the step ends, in milliseconds (default 0)
}

// Describe names the target.
func (s *HoverOnStep) Describe() string { return "hoverOn: " + describeTarget(s.Selector) }

// HoldOnStep presses an element (at its point:, default the centre) or a point on the screen, holds it for duration, then releases: on the browser, Android and iOS.
type HoldOnStep struct {
	BaseStep   `yaml:",inline"`
	Selector   Selector `yaml:",inline"`
	DurationMs int      `yaml:"duration"` // How long to hold, in milliseconds (default 1000)
}

// Describe names the target and the hold.
func (s *HoldOnStep) Describe() string {
	return fmt.Sprintf("holdOn: %s for %dms", describeTarget(s.Selector), s.EffectiveDurationMs())
}

// DefaultHoldMs is holdOn's hold when the step names none.
const DefaultHoldMs = 1000

// EffectiveDurationMs is how long holdOn holds.
func (s *HoldOnStep) EffectiveDurationMs() int {
	if s.DurationMs > 0 {
		return s.DurationMs
	}
	return DefaultHoldMs
}

// DragPathStep presses at the first point, moves through each later point in turn and releases at the last, one unbroken stroke: on the browser (the mouse), iOS (WebDriverAgent) and Android (uiautomator2, and devicelab through input motionevent on Android 11 or later); other drivers answer UNSUPPORTED.
type DragPathStep struct {
	BaseStep     `yaml:",inline"`
	Points       []string `yaml:"points"`       // The path, at least two points, each "x, y" in screen pixels or "x%, y%"
	DurationMs   int      `yaml:"duration"`     // How long the movement takes, in milliseconds, shared among the legs by length (default 1000)
	HoldDuration int      `yaml:"holdDuration"` // How long to press at the first point before moving, in milliseconds (default 0)
}

// Describe names the path's ends.
func (s *DragPathStep) Describe() string {
	if len(s.Points) == 0 {
		return "dragPath"
	}
	return fmt.Sprintf("dragPath: %s → %s (%d points)", s.Points[0], s.Points[len(s.Points)-1], len(s.Points))
}

// DefaultDragPathMs is dragPath's movement time when the step names none.
const DefaultDragPathMs = 1000

// EffectiveDurationMs is how long dragPath's movement takes.
func (s *DragPathStep) EffectiveDurationMs() int {
	if s.DurationMs > 0 {
		return s.DurationMs
	}
	return DefaultDragPathMs
}

// describeTarget is a selector's description, or its point when it names
// only a point.
func describeTarget(sel Selector) string {
	if sel.IsEmpty() {
		return "point " + sel.Point
	}
	if sel.Point != "" {
		return sel.DescribeQuoted() + " at " + sel.Point
	}
	return sel.DescribeQuoted()
}

// ValidatePointerStep checks hoverOn, holdOn and dragPath after decoding.
func ValidatePointerStep(step Step) error {
	switch s := step.(type) {
	case *HoverOnStep:
		return validatePointerTarget("hoverOn", s.Selector, s.DurationMs)
	case *HoldOnStep:
		return validatePointerTarget("holdOn", s.Selector, s.DurationMs)
	case *DragPathStep:
		switch {
		case len(s.Points) < 2:
			return fmt.Errorf("dragPath needs at least two points, got %d", len(s.Points))
		case s.DurationMs < 0 || s.HoldDuration < 0:
			return fmt.Errorf("dragPath: duration and holdDuration must not be negative")
		}
		for i, p := range s.Points {
			if strings.TrimSpace(p) == "" {
				return fmt.Errorf("dragPath: point %d is empty", i+1)
			}
		}
	}
	return nil
}

func validatePointerTarget(name string, sel Selector, durationMs int) error {
	if sel.IsEmpty() && strings.TrimSpace(sel.Point) == "" {
		return fmt.Errorf("%s needs an element or a point", name)
	}
	if durationMs < 0 {
		return fmt.Errorf("%s: duration must not be negative, got %d", name, durationMs)
	}
	return nil
}

// validatedPointerStep is a decoded hoverOn, holdOn or dragPath, or the
// parse error its values give.
func validatedPointerStep(step Step, node *yaml.Node, sourcePath string) (Step, error) {
	if err := ValidatePointerStep(step); err != nil {
		return nil, wrapParseError(sourcePath, node.Line, err)
	}
	return step, nil
}
