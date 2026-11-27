package executor

// maestro-d: hoverOn, holdOn and dragPath. holdOn is each driver's press
// for a duration: longPressOn on an element, tapOn's point with a duration
// on a bare point. hoverOn and dragPath need a driver that has them
// (core.Hoverer, core.PathDragger); the computer device hovers with its
// moveMouse. Anything else answers UNSUPPORTED.

import (
	"fmt"
	"strings"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

// computerPlatform is the computer device's platform (pkg/driver/computer).
const computerPlatform = "macos"

func (fr *FlowRunner) executePointerStep(step flow.Step) *core.CommandResult {
	if err := flow.ValidatePointerStep(step); err != nil {
		return errorResult(&UsageError{Err: err})
	}
	platform := strings.ToLower(driverPlatform(fr.driver))
	switch s := step.(type) {
	case *flow.HoldOnStep:
		base := s.BaseStep
		if s.Selector.IsEmpty() {
			base.StepType = flow.StepTapOnPoint
			return fr.driver.Execute(&flow.TapOnPointStep{BaseStep: base, Point: s.Selector.Point, LongPress: true, DurationMs: s.EffectiveDurationMs()})
		}
		base.StepType = flow.StepLongPressOn
		return fr.driver.Execute(&flow.LongPressOnStep{BaseStep: base, Selector: s.Selector, DurationMs: s.EffectiveDurationMs()})
	case *flow.HoverOnStep:
		if h, ok := core.Unwrap(fr.driver).(core.Hoverer); ok {
			return h.HoverOn(s)
		}
		switch {
		case platform == computerPlatform && s.Selector.IsEmpty():
			base := s.BaseStep
			base.StepType = flow.StepMoveMouse
			res := fr.driver.Execute(&flow.MoveMouseStep{BaseStep: base, Point: s.Selector.Point})
			if res.Success {
				fr.pause(time.Duration(s.DurationMs) * time.Millisecond)
			}
			return res
		case platform == computerPlatform:
			return unsupportedResult("hoverOn: the computer device hovers over a point, not an element")
		case platform == "android" || isApplePlatform(platform):
			return unsupportedResult(fmt.Sprintf("hoverOn: a touch screen has no hover (%s)", platform))
		}
		return unsupportedResult(fmt.Sprintf("hoverOn: the %s driver has no pointer to hover", platform))
	case *flow.DragPathStep:
		if d, ok := core.Unwrap(fr.driver).(core.PathDragger); ok {
			return d.DragPath(s)
		}
		return unsupportedResult(fmt.Sprintf("dragPath: the %s driver cannot drag along a path", platform))
	}
	return unsupportedResult(fmt.Sprintf("%s is not a pointer step", step.Describe()))
}

// pause waits d, or until the run is cancelled.
func (fr *FlowRunner) pause(d time.Duration) {
	if d <= 0 {
		return
	}
	select {
	case <-time.After(d):
	case <-fr.ctx.Done():
	}
}
