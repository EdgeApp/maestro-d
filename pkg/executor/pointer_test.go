package executor

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

// recorder is the test mock recording the steps the driver ran.
func recorder(p string) (*mockDriver, *[]flow.Step) {
	var ran []flow.Step
	d := &mockDriver{platformFunc: platform(p, false), executeFunc: func(s flow.Step) *core.CommandResult {
		ran = append(ran, s)
		return &core.CommandResult{Success: true}
	}}
	return d, &ran
}

// holdOn is longPressOn on an element and tapOn's point with a duration on
// a bare point, at the default hold or the step's.
func TestHoldOn_IsTheDriversTimedPress(t *testing.T) {
	d, ran := recorder("ios")
	s := deviceSession(t, d, SessionConfig{})
	for _, y := range []string{
		"holdOn:\n  point: 50%, 40%\n  duration: 2500",
		"holdOn: Play",
		"holdOn:\n  id: video\n  point: 10%, 10%",
		"runFlow:\n  commands:\n    - holdOn:\n        point: 1, 2",
	} {
		if out := s.Execute(context.Background(), mustStep(t, y)); !out.Success {
			t.Fatalf("%s: %+v", y, out)
		}
	}
	if len(*ran) != 4 {
		t.Fatalf("ran %d steps", len(*ran))
	}
	if p, ok := (*ran)[0].(*flow.TapOnPointStep); !ok || p.Point != "50%, 40%" || !p.LongPress || p.DurationMs != 2500 || p.Type() != flow.StepTapOnPoint {
		t.Fatalf("point hold: %#v", (*ran)[0])
	}
	if l, ok := (*ran)[1].(*flow.LongPressOnStep); !ok || l.Selector.Text != "Play" || l.DurationMs != flow.DefaultHoldMs || l.Type() != flow.StepLongPressOn {
		t.Fatalf("element hold: %#v", (*ran)[1])
	}
	if l := (*ran)[2].(*flow.LongPressOnStep); l.Selector.ID != "video" || l.Selector.Point != "10%, 10%" {
		t.Fatalf("element point hold: %#v", l)
	}
	if p := (*ran)[3].(*flow.TapOnPointStep); p.Point != "1, 2" {
		t.Fatalf("nested: %#v", p)
	}
}

type hoverDriver struct {
	mockDriver
	hovered []*flow.HoverOnStep
	dragged []*flow.DragPathStep
}

func (h *hoverDriver) HoverOn(s *flow.HoverOnStep) *core.CommandResult {
	h.hovered = append(h.hovered, s)
	return &core.CommandResult{Success: true}
}

func (h *hoverDriver) DragPath(s *flow.DragPathStep) *core.CommandResult {
	h.dragged = append(h.dragged, s)
	return &core.CommandResult{Success: true}
}

// hoverOn and dragPath reach a driver that has them, through a wrapper,
// with variables expanded; a driver without them answers UNSUPPORTED.
func TestHoverOnAndDragPath_DriverSupport(t *testing.T) {
	h := &hoverDriver{mockDriver: mockDriver{platformFunc: platform("web", false)}}
	s := deviceSession(t, wrappedDriver{h}, SessionConfig{Env: map[string]string{"X": "30%"}})
	for _, y := range []string{"hoverOn: Menu", "dragPath:\n  points: [\"${X}, 50%\", \"70%, 50%\"]", "runFlow:\n  commands:\n    - hoverOn:\n        point: 1, 2"} {
		if out := s.Execute(context.Background(), mustStep(t, y)); !out.Success {
			t.Fatalf("%s: %+v", y, out)
		}
	}
	if len(h.hovered) != 2 || h.hovered[0].Selector.Text != "Menu" || len(h.dragged) != 1 || h.dragged[0].Points[0] != "30%, 50%" {
		t.Fatalf("hovered %v dragged %v", h.hovered, h.dragged)
	}

	for p, want := range map[string]string{
		"android": "hoverOn: a touch screen has no hover (android)",
		"ios":     "hoverOn: a touch screen has no hover (ios)",
		"":        "hoverOn: the device's driver has no pointer to hover",
	} {
		d, ran := recorder(p)
		if p == "" {
			d.platformFunc = func() *core.PlatformInfo { return nil }
		}
		s := deviceSession(t, d, SessionConfig{})
		out := s.Execute(context.Background(), mustStep(t, "hoverOn: Menu"))
		if out.Success || out.Code != codeUnsupported || !strings.Contains(out.Error.Message, want) || len(*ran) != 0 {
			t.Fatalf("%q: %+v %+v", p, out, out.Error)
		}
		out = s.Execute(context.Background(), mustStep(t, "dragPath:\n  points: [\"1, 2\", \"3, 4\"]"))
		if out.Success || out.Code != codeUnsupported || !strings.Contains(out.Error.Message, "cannot drag along a path") {
			t.Fatalf("%q dragPath: %+v", p, out)
		}
	}
}

// The computer device hovers over a point with moveMouse, then waits; an
// element it cannot hover.
func TestHoverOn_ComputerDevice(t *testing.T) {
	d, ran := recorder(computerPlatform)
	s := deviceSession(t, d, SessionConfig{})
	start := time.Now()
	if out := s.Execute(context.Background(), mustStep(t, "hoverOn:\n  point: 10%, 20%\n  duration: 150")); !out.Success {
		t.Fatalf("%+v", out)
	}
	if m, ok := (*ran)[0].(*flow.MoveMouseStep); !ok || m.Point != "10%, 20%" || m.Type() != flow.StepMoveMouse || time.Since(start) < 150*time.Millisecond {
		t.Fatalf("moveMouse: %#v after %v", (*ran)[0], time.Since(start))
	}
	out := s.Execute(context.Background(), mustStep(t, "hoverOn: Menu"))
	if out.Success || out.Code != codeUnsupported || !strings.Contains(out.Error.Message, "not an element") {
		t.Fatalf("element: %+v", out)
	}
}

// A step whose values fail validation once expanded is USAGE, and pause
// ends early when the run is cancelled.
func TestPointerStep_UsageAndPause(t *testing.T) {
	d, _ := recorder("web")
	fr := &FlowRunner{ctx: context.Background(), driver: d}
	if res := fr.executePointerStep(&flow.DragPathStep{Points: []string{"1, 2"}}); res.Success || outcomeCode(res.Error) != codeUsage {
		t.Fatalf("one point: %+v", res)
	}
	if res := fr.executePointerStep(&flow.TapOnStep{}); res.Success || !strings.Contains(res.Message, "not a pointer step") {
		t.Fatalf("other step: %+v", res)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fr.ctx = ctx
	start := time.Now()
	fr.pause(time.Hour)
	fr.pause(0)
	if time.Since(start) > time.Second {
		t.Fatal("pause outlived its run")
	}
}
