package devicelab

import (
	"errors"
	"strings"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

// dragPath is one shell command: DOWN, the hold, a MOVE per leg after its
// time, UP at the last point.
func TestDragPath_MotionEvents(t *testing.T) {
	var _ core.PathDragger = (*Driver)(nil)
	sh := &mockShell{}
	d := New(newTrackingClient(), &core.PlatformInfo{ScreenWidth: 1000, ScreenHeight: 2000}, sh)
	res := d.DragPath(&flow.DragPathStep{Points: []string{"10%, 10%", "50%, 10%", "50%, 50%"}, DurationMs: 300, HoldDuration: 200})
	want := "input motionevent DOWN 100 200; sleep 0.200; sleep 0.100; input motionevent MOVE 500 200; " +
		"sleep 0.200; input motionevent MOVE 500 1000; input motionevent UP 500 1000"
	if !res.Success || len(sh.commands) != 1 || sh.commands[0] != want {
		t.Fatalf("%+v\n%v", res, sh.commands)
	}
	sh.out = "Error: Unknown command: motionevent"
	if res := d.DragPath(&flow.DragPathStep{Points: []string{"1, 1", "2, 2"}}); res.Success || !strings.Contains(res.Message, "Android 11") {
		t.Fatalf("old Android: %+v", res)
	}
	sh.out, sh.err = "", errors.New("device offline")
	if res := d.DragPath(&flow.DragPathStep{Points: []string{"1, 1", "2, 2"}}); res.Success {
		t.Fatal("shell error")
	}
	if res := d.DragPath(&flow.DragPathStep{Points: []string{"1, 1", "bad"}}); res.Success {
		t.Fatal("bad point")
	}
	if res := New(newTrackingClient(), &core.PlatformInfo{}, nil).DragPath(&flow.DragPathStep{Points: []string{"1, 1", "2, 2"}}); res.Success || !strings.Contains(res.Message, "device access") {
		t.Fatalf("no device: %+v", res)
	}
}
