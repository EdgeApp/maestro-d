package devicelab_ios

import (
	"errors"
	"strings"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

func TestSetStatusBar(t *testing.T) {
	d := &Driver{info: &core.PlatformInfo{Platform: "ios"}, udid: "DEV-1"}
	if r := d.setStatusBar(&flow.SetStatusBarStep{Mode: "demo"}); r.Success || r.Message != core.StatusBarPhysicalIOSMessage {
		t.Fatalf("%+v", r)
	}
	var got []string
	prev := core.RunXcrun
	core.RunXcrun = func(args ...string) ([]byte, error) { got = args; return nil, nil }
	t.Cleanup(func() { core.RunXcrun = prev })
	d = &Driver{info: &core.PlatformInfo{Platform: "ios", IsSimulator: true}, udid: "SIM-1"}
	if r := d.Execute(&flow.SetStatusBarStep{Mode: "live"}); !r.Success || len(got) != 4 || got[3] != "clear" {
		t.Fatalf("%+v %q", r, got)
	}
	if _, err := d.SampleProbe(core.SampleFrames, "com.x"); err == nil {
		t.Fatal("frames on iOS")
	}
}

// A key the driver cannot send answers UNSUPPORTED, naming the driver that
// can.
func TestHandlePressKey_UnknownKeyUnsupported(t *testing.T) {
	res := (&Driver{}).handlePressKey(&flow.PressKeyStep{Key: "up"})
	var unsupported *core.UnsupportedError
	if res.Success || !errors.As(res.Error, &unsupported) || !strings.Contains(res.Message, "WebDriverAgent") {
		t.Fatalf("got %+v", res)
	}
}
