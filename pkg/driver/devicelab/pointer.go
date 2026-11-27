package devicelab

// maestro-d: dragPath through Android's own input motionevent
// (core.PathDragger): the agent has no multi-point gesture.

import (
	"fmt"
	"strings"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

// DragPath presses at the first point, holds, moves through every later
// point and lifts, as one shell command of input motionevent DOWN, MOVE
// and UP events (Android 11 or later) with sleeps between. Each event is an
// input call of its own, so the timing is approximate.
func (d *Driver) DragPath(step *flow.DragPathStep) *core.CommandResult {
	if d.device == nil {
		return errorResult(fmt.Errorf("device not configured"), "dragPath requires device access")
	}
	w, h, err := d.screenSize()
	if err != nil {
		return errorResult(err, fmt.Sprintf("Failed to get screen size: %v", err))
	}
	path, err := core.ParsePath(step.Points, w, h)
	if err != nil {
		return errorResult(err, fmt.Sprintf("dragPath: %v", err))
	}
	cmd := []string{fmt.Sprintf("input motionevent DOWN %d %d", path[0][0], path[0][1])}
	pause := func(ms int) {
		if ms > 0 {
			cmd = append(cmd, fmt.Sprintf("sleep %.3f", float64(ms)/1000))
		}
	}
	pause(step.HoldDuration)
	for _, l := range core.PathLegs(path, step.EffectiveDurationMs()) {
		pause(l.Ms)
		cmd = append(cmd, fmt.Sprintf("input motionevent MOVE %d %d", l.X, l.Y))
	}
	end := path[len(path)-1]
	cmd = append(cmd, fmt.Sprintf("input motionevent UP %d %d", end[0], end[1]))
	out, err := d.device.Shell(strings.Join(cmd, "; "))
	if err == nil && (strings.Contains(out, "Error") || strings.Contains(out, "Exception")) {
		err = fmt.Errorf("%s", strings.TrimSpace(out))
	}
	if err != nil {
		return errorResult(err, fmt.Sprintf("dragPath failed (input motionevent needs Android 11 or later): %v", err))
	}
	return successResult(fmt.Sprintf("Dragged through %d points to (%d, %d)", len(path), end[0], end[1]), nil)
}
