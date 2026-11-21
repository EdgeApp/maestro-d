package executor

import (
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

// The computer device's mouse steps take their point from a variable like
// any other step's coordinates (maestro-d).
func TestExpandStepExpandsMousePoints(t *testing.T) {
	for _, tc := range []struct {
		step flow.Step
		read func(flow.Step) string
	}{
		{&flow.MoveMouseStep{Point: "${X}, ${Y}"}, func(s flow.Step) string { return s.(*flow.MoveMouseStep).Point }},
		{&flow.ClickOnStep{Point: "${X}, ${Y}"}, func(s flow.Step) string { return s.(*flow.ClickOnStep).Point }},
		{&flow.ScrollAtStep{Point: "${X}, ${Y}"}, func(s flow.Step) string { return s.(*flow.ScrollAtStep).Point }},
	} {
		se := NewScriptEngine()
		se.SetVariable("X", "120")
		se.SetVariable("Y", "40%")
		se.ExpandStep(tc.step)
		if got := tc.read(tc.step); got != "120, 40%" {
			t.Errorf("%T point %q", tc.step, got)
		}
		se.Close()
	}
}
