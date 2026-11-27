package uiautomator2

// maestro-d: dragPath as one W3C touch stroke (core.PathDragger).

import (
	"fmt"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

// touchPather is a client that strokes a path (the UIAutomator2 server's
// W3C actions).
type touchPather interface {
	TouchPath(startX, startY, holdMs int, legs [][3]int) error
}

// DragPath presses at the first point, holds, moves through every later
// point (each leg's time shared by length) and lifts, as one touch.
func (d *Driver) DragPath(step *flow.DragPathStep) *core.CommandResult {
	tp, ok := d.client.(touchPather)
	if !ok {
		return core.UnsupportedResult("dragPath: this UIAutomator2 client cannot send touch actions")
	}
	w, h, err := d.screenSize()
	if err != nil {
		return errorResult(err, fmt.Sprintf("Failed to get screen size: %v", err))
	}
	return touchPathResult(tp, step, w, h)
}

// touchPathResult strokes step's path on a w by h screen through tp.
func touchPathResult(tp touchPather, step *flow.DragPathStep, w, h int) *core.CommandResult {
	path, err := core.ParsePath(step.Points, w, h)
	if err != nil {
		return errorResult(err, fmt.Sprintf("dragPath: %v", err))
	}
	legs := core.PathLegs(path, step.EffectiveDurationMs())
	raw := make([][3]int, len(legs))
	for i, l := range legs {
		raw[i] = [3]int{l.X, l.Y, l.Ms}
	}
	if err := tp.TouchPath(path[0][0], path[0][1], step.HoldDuration, raw); err != nil {
		return errorResult(err, fmt.Sprintf("dragPath failed: %v", err))
	}
	end := path[len(path)-1]
	return successResult(fmt.Sprintf("Dragged through %d points to (%d, %d)", len(path), end[0], end[1]), nil)
}
