package cdp

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

// pointerPage logs every mouse event on the page into window.events, and
// has a box far enough down to need scrolling.
func pointerPage() string {
	return `<!DOCTYPE html>
<html><body style="margin:0;height:3000px">
<div id="box" style="position:absolute;left:100px;top:1500px;width:200px;height:100px;background:#ccc">Menu</div>
<script>
window.events = [];
["mousemove", "mousedown", "mouseup", "mouseover"].forEach(function(t) {
  document.addEventListener(t, function(e) {
    window.events.push({t: t, x: e.clientX, y: e.clientY, id: e.target.id || "", at: performance.now()});
  });
});
</script>
</body></html>`
}

type mouseEvent struct {
	T    string
	X, Y float64
	ID   string
	At   float64
}

func pageEvents(t *testing.T, d *Driver) []mouseEvent {
	t.Helper()
	res, err := d.page.Eval(`() => JSON.stringify(window.events)`)
	if err != nil {
		t.Fatal(err)
	}
	var ev []mouseEvent
	if err := json.Unmarshal([]byte(res.Value.Str()), &ev); err != nil {
		t.Fatal(err)
	}
	return ev
}

func pointerDriver(t *testing.T) *Driver {
	t.Helper()
	ts := newIframeTestServer(pointerPage())
	t.Cleanup(ts.Close)
	d := newTestDriver(t, ts.URL)
	t.Cleanup(func() { d.Close() })
	return d
}

// hoverOn moves onto the element (scrolled into view) without pressing;
// a bare point moves there.
func TestHoverOn_MovesWithoutPressing(t *testing.T) {
	var _ core.Hoverer = (*Driver)(nil)
	d := pointerDriver(t)
	start := time.Now()
	res := d.HoverOn(&flow.HoverOnStep{Selector: flow.Selector{ID: "box"}, DurationMs: 100})
	if !res.Success || time.Since(start) < 100*time.Millisecond {
		t.Fatalf("hover: %+v", res)
	}
	ev := pageEvents(t, d)
	over := false
	for _, e := range ev {
		if e.T == "mousedown" || e.T == "mouseup" {
			t.Fatalf("hover pressed: %v", ev)
		}
		over = over || (e.T == "mouseover" && e.ID == "box")
	}
	if !over {
		t.Fatalf("no mouseover on the box: %v", ev)
	}
	if res := d.HoverOn(&flow.HoverOnStep{Selector: flow.Selector{Point: "10, 20"}}); !res.Success {
		t.Fatalf("point: %+v", res)
	}
	if last := pageEvents(t, d); last[len(last)-1].X != 10 || last[len(last)-1].Y != 20 {
		t.Fatalf("point hover at %v", last[len(last)-1])
	}
	if res := d.HoverOn(&flow.HoverOnStep{Selector: flow.Selector{ID: "missing"}, BaseStep: flow.BaseStep{TimeoutMs: 200}}); res.Success || !strings.Contains(res.Message, "not found") {
		t.Fatalf("missing: %+v", res)
	}
	if res := d.HoverOn(&flow.HoverOnStep{Selector: flow.Selector{Point: "nonsense"}}); res.Success {
		t.Fatal("bad point hovered")
	}
}

// dragPath presses at the first point, passes through each later one and
// releases at the last, one press, taking about its duration.
func TestDragPath_StrokesThePath(t *testing.T) {
	var _ core.PathDragger = (*Driver)(nil)
	d := pointerDriver(t)
	start := time.Now()
	res := d.DragPath(&flow.DragPathStep{Points: []string{"100, 100", "300, 100", "300, 300"}, DurationMs: 400, HoldDuration: 50})
	if !res.Success || time.Since(start) < 450*time.Millisecond {
		t.Fatalf("drag: %+v after %v", res, time.Since(start))
	}
	ev := pageEvents(t, d)
	var downs, ups int
	var pressed []mouseEvent
	for _, e := range ev {
		switch e.T {
		case "mousedown":
			downs++
			if e.X != 100 || e.Y != 100 {
				t.Fatalf("pressed at %v", e)
			}
		case "mouseup":
			ups++
			if e.X != 300 || e.Y != 300 {
				t.Fatalf("released at %v", e)
			}
		case "mousemove":
			if downs == 1 && ups == 0 {
				pressed = append(pressed, e)
			}
		}
	}
	corner := false
	for i, e := range pressed {
		corner = corner || (e.X == 300 && e.Y == 100)
		if i > 0 && (abs(e.X-pressed[i-1].X) > dragPathMaxPx+1 || abs(e.Y-pressed[i-1].Y) > dragPathMaxPx+1) {
			t.Fatalf("jumped from %v to %v", pressed[i-1], e)
		}
	}
	if downs != 1 || ups != 1 || !corner || len(pressed) < 40 {
		t.Fatalf("downs %d ups %d corner %v moves %d", downs, ups, corner, len(pressed))
	}
	if res := d.DragPath(&flow.DragPathStep{Points: []string{"1, 2", "x"}}); res.Success {
		t.Fatal("bad point dragged")
	}
}

// tapOn's point with a duration (holdOn on a point) holds the press; a
// plain one clicks; a pixel point is pixels.
func TestTapOnPoint_Holds(t *testing.T) {
	d := pointerDriver(t)
	if res := d.tapOnPoint(&flow.TapOnPointStep{Point: "40, 60", LongPress: true, DurationMs: 300}); !res.Success {
		t.Fatalf("hold: %+v", res)
	}
	ev := pageEvents(t, d)
	var down, up mouseEvent
	for _, e := range ev {
		if e.T == "mousedown" {
			down = e
		}
		if e.T == "mouseup" {
			up = e
		}
	}
	if down.X != 40 || down.Y != 60 || up.At-down.At < 280 {
		t.Fatalf("down %v up %v", down, up)
	}
	start := time.Now()
	if res := d.tapOnPoint(&flow.TapOnPointStep{Point: "40, 60", LongPress: true}); !res.Success || time.Since(start) < time.Duration(flow.DefaultHoldMs)*time.Millisecond {
		t.Fatalf("default hold: %+v", res)
	}
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
