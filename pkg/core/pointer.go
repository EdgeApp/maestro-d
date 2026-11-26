package core

// maestro-d: the pointer gestures hoverOn and dragPath, which only some
// drivers can do: the executor asks for these interfaces and answers
// UNSUPPORTED for a driver that has neither.

import (
	"math"

	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

// Hoverer is a driver with a pointer that moves without pressing (the
// browser's mouse).
type Hoverer interface {
	HoverOn(step *flow.HoverOnStep) *CommandResult
}

// PathDragger is a driver that can press, move through a list of points
// and release as one stroke.
type PathDragger interface {
	DragPath(step *flow.DragPathStep) *CommandResult
}

// PathLeg is one leg of a dragPath: a move to (X, Y) taking Ms milliseconds.
type PathLeg struct{ X, Y, Ms int }

// ParsePath resolves dragPath's points ("x, y" pixels or "x%, y%") on a
// screen of w by h.
func ParsePath(points []string, w, h int) ([][2]int, error) {
	out := make([][2]int, 0, len(points))
	for _, p := range points {
		x, y, err := ParsePointCoords(p, w, h)
		if err != nil {
			return nil, err
		}
		out = append(out, [2]int{x, y})
	}
	return out, nil
}

// PathLegs is the moves from path[0] through each later point, sharing
// durationMs among them by length (evenly when the path has no length).
func PathLegs(path [][2]int, durationMs int) []PathLeg {
	if len(path) < 2 {
		return nil
	}
	lengths := make([]float64, len(path)-1)
	total := 0.0
	for i := 1; i < len(path); i++ {
		lengths[i-1] = math.Hypot(float64(path[i][0]-path[i-1][0]), float64(path[i][1]-path[i-1][1]))
		total += lengths[i-1]
	}
	legs := make([]PathLeg, 0, len(lengths))
	spent := 0
	for i, l := range lengths {
		share := 1.0 / float64(len(lengths))
		if total > 0 {
			share = l / total
		}
		ms := int(math.Round(float64(durationMs) * share))
		if i == len(lengths)-1 {
			ms = durationMs - spent // the rounding's remainder goes last
		}
		spent += ms
		legs = append(legs, PathLeg{X: path[i+1][0], Y: path[i+1][1], Ms: ms})
	}
	return legs
}

// SplitLegs cuts each leg from start into moves of at most maxPx pixels,
// sharing its time evenly, for a driver whose pointer jumps from move to
// move (the browser's mouse) and whose page should see the path between.
func SplitLegs(start [2]int, legs []PathLeg, maxPx int) []PathLeg {
	var out []PathLeg
	from := start
	for _, leg := range legs {
		n := int(math.Ceil(math.Hypot(float64(leg.X-from[0]), float64(leg.Y-from[1])) / float64(maxPx)))
		if n < 1 {
			n = 1
		}
		spent := 0
		for k := 1; k <= n; k++ {
			ms := leg.Ms * k / n
			x := from[0] + (leg.X-from[0])*k/n
			y := from[1] + (leg.Y-from[1])*k/n
			out = append(out, PathLeg{X: x, Y: y, Ms: ms - spent})
			spent = ms
		}
		from = [2]int{leg.X, leg.Y}
	}
	return out
}
