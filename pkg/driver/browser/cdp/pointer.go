package cdp

// maestro-d: hoverOn and dragPath with the page's mouse (core.Hoverer,
// core.PathDragger).

import (
	"fmt"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
	"github.com/go-rod/rod/lib/proto"
)

// dragPathMaxPx is the longest jump between two mouse moves of a dragPath,
// so the page's handlers see the path between its points.
const dragPathMaxPx = 10

// pointerTarget is where a hoverOn lands: a bare point on the viewport, or
// an element's point: (default its centre).
func (d *Driver) pointerTarget(sel flow.Selector, timeoutMs int) (float64, float64, *core.ElementInfo, error) {
	if sel.IsEmpty() {
		x, y, err := core.ParsePointCoords(sel.Point, d.viewportW, d.viewportH)
		return float64(x), float64(y), nil, err
	}
	elem, info, err := d.findElement(sel, isOptional(sel.Optional), timeoutMs)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("element not found: %s: %w", sel.Describe(), err)
	}
	// The element's box once scrolled into view, in viewport pixels.
	_ = elem.ScrollIntoView()
	b := info.Bounds
	if res, err := elem.Eval(`() => { var r = this.getBoundingClientRect(); return [r.left, r.top, r.width, r.height]; }`); err == nil {
		if r := res.Value.Arr(); len(r) == 4 {
			b = core.Bounds{X: r[0].Int(), Y: r[1].Int(), Width: r[2].Int(), Height: r[3].Int()}
		}
	}
	x, y, err := core.PointInBounds(sel.Point, b)
	return float64(x), float64(y), info, err
}

// HoverOn moves the mouse onto the target without pressing, so the page
// sees it enter (mouseover, :hover), then waits the step's duration.
func (d *Driver) HoverOn(step *flow.HoverOnStep) *core.CommandResult {
	x, y, info, err := d.pointerTarget(step.Selector, step.TimeoutMs)
	if err != nil {
		return errorResult(err, fmt.Sprintf("hoverOn: %v", err))
	}
	if err := d.page.Mouse.MoveTo(proto.NewPoint(x, y)); err != nil {
		return errorResult(err, fmt.Sprintf("hoverOn: failed to move the mouse: %v", err))
	}
	time.Sleep(time.Duration(step.DurationMs) * time.Millisecond)
	return successResult(fmt.Sprintf("Hovered at (%.0f, %.0f)", x, y), info)
}

// DragPath presses at the first point, holds, moves through every later
// point in moves of at most dragPathMaxPx pixels, paced by the step's
// duration, and releases at the last.
func (d *Driver) DragPath(step *flow.DragPathStep) *core.CommandResult {
	path, err := core.ParsePath(step.Points, d.viewportW, d.viewportH)
	if err != nil {
		return errorResult(err, fmt.Sprintf("dragPath: %v", err))
	}
	moves := core.SplitLegs(path[0], core.PathLegs(path, step.EffectiveDurationMs()), dragPathMaxPx)
	mouse := d.page.Mouse
	if err := mouse.MoveTo(proto.NewPoint(float64(path[0][0]), float64(path[0][1]))); err != nil {
		return errorResult(err, fmt.Sprintf("dragPath: failed to move the mouse: %v", err))
	}
	if err := mouse.Down(proto.InputMouseButtonLeft, 1); err != nil {
		return errorResult(err, fmt.Sprintf("dragPath: failed to press: %v", err))
	}
	time.Sleep(time.Duration(step.HoldDuration) * time.Millisecond)
	for _, m := range moves {
		time.Sleep(time.Duration(m.Ms) * time.Millisecond)
		if err := mouse.MoveTo(proto.NewPoint(float64(m.X), float64(m.Y))); err != nil {
			_ = mouse.Up(proto.InputMouseButtonLeft, 1)
			return errorResult(err, fmt.Sprintf("dragPath: failed to move the mouse: %v", err))
		}
	}
	if err := mouse.Up(proto.InputMouseButtonLeft, 1); err != nil {
		return errorResult(err, fmt.Sprintf("dragPath: failed to release: %v", err))
	}
	end := path[len(path)-1]
	return successResult(fmt.Sprintf("Dragged through %d points to (%d, %d)", len(path), end[0], end[1]), nil)
}
