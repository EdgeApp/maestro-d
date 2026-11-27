package uiautomator2

import (
	"errors"
	"strings"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

type pathClient struct {
	MockUIA2Client
	start [2]int
	hold  int
	legs  [][3]int
	err   error
}

func (p *pathClient) TouchPath(x, y, holdMs int, legs [][3]int) error {
	p.start, p.hold, p.legs = [2]int{x, y}, holdMs, legs
	return p.err
}

func TestDragPath_TouchPath(t *testing.T) {
	var _ core.PathDragger = (*Driver)(nil)
	c := &pathClient{}
	d := New(c, &core.PlatformInfo{ScreenWidth: 1000, ScreenHeight: 2000}, nil)
	res := d.DragPath(&flow.DragPathStep{Points: []string{"10%, 10%", "50%, 10%", "50%, 50%"}, DurationMs: 1000, HoldDuration: 100})
	if !res.Success || c.start != [2]int{100, 200} || c.hold != 100 || len(c.legs) != 2 || c.legs[1] != [3]int{500, 1000, 667} {
		t.Fatalf("%+v start %v hold %d legs %v", res, c.start, c.hold, c.legs)
	}
	if res := d.DragPath(&flow.DragPathStep{Points: []string{"1, 1", "bad"}}); res.Success {
		t.Fatal("bad point")
	}
	c.err = errors.New("socket hang up")
	if res := d.DragPath(&flow.DragPathStep{Points: []string{"1, 1", "2, 2"}}); res.Success || !strings.Contains(res.Message, "socket hang up") {
		t.Fatalf("error: %+v", res)
	}
	// A client without touch actions, or no screen size.
	if res := New(&MockUIA2Client{}, &core.PlatformInfo{ScreenWidth: 10, ScreenHeight: 10}, nil).DragPath(&flow.DragPathStep{Points: []string{"1, 1", "2, 2"}}); res.Success {
		t.Fatal("no touch actions")
	}
	if res := New(c, &core.PlatformInfo{}, nil).DragPath(&flow.DragPathStep{Points: []string{"1, 1", "2, 2"}}); res.Success {
		t.Fatalf("no screen size: %+v", res)
	}
}
