package wda

// maestro-d: dragPath as one W3C touch stroke (core.PathDragger).

import (
	"fmt"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

// DragPath presses at the first point, holds, moves through every later
// point (each leg's time shared by length) and lifts, as one touch.
func (d *Driver) DragPath(step *flow.DragPathStep) *core.CommandResult {
	w, h, err := d.screenSize()
	if err != nil {
		return errorResult(err, "Failed to get screen size")
	}
	path, err := core.ParsePath(step.Points, w, h)
	if err != nil {
		return errorResult(err, fmt.Sprintf("dragPath: %v", err))
	}
	if err := d.client.PerformTouchPath(path[0], step.HoldDuration, core.PathLegs(path, step.EffectiveDurationMs())); err != nil {
		return errorResult(err, fmt.Sprintf("dragPath failed: %v", err))
	}
	end := path[len(path)-1]
	return successResult(fmt.Sprintf("Dragged through %d points to (%d, %d)", len(path), end[0], end[1]), nil)
}
