package wda

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

func tempMedia(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "photo.png")
	if err := os.WriteFile(p, []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// A physical iPhone cannot take media, and the hint must not send anyone to
// --driver devicelab, which refuses physical devices in this fork.
func TestAddMedia_PhysicalIPhoneHintIsAccurate(t *testing.T) {
	var calls [][]string
	withFakeExec(t, &calls, false)
	d := &Driver{info: &core.PlatformInfo{Platform: "ios", IsSimulator: false}, udid: "00008110-PHONE"}

	res := d.addMedia(&flow.AddMediaStep{Files: []string{tempMedia(t)}})
	if res.Success {
		t.Fatal("addMedia on a physical iPhone must fail")
	}
	if len(calls) != 0 {
		t.Errorf("no host command may run for a physical iPhone, ran %v", calls)
	}
	for _, want := range []string{"physical iPhone", "runs only on simulators", "iOS simulator", "Photos library before the flow"} {
		if !strings.Contains(res.Message, want) {
			t.Errorf("Message = %q, want it to contain %q", res.Message, want)
		}
	}
	if strings.Contains(res.Message, "--driver devicelab") {
		t.Errorf("Message = %q, must not recommend --driver devicelab", res.Message)
	}
}

func TestAddMedia_SimulatorUsesSimctl(t *testing.T) {
	var calls [][]string
	withFakeExec(t, &calls, false)
	d := &Driver{info: &core.PlatformInfo{Platform: "ios", IsSimulator: true}, udid: "SIM-UDID"}
	file := tempMedia(t)

	res := d.addMedia(&flow.AddMediaStep{Files: []string{file}})
	if !res.Success {
		t.Fatalf("addMedia on a simulator failed: %s", res.Message)
	}
	want := []string{"xcrun", "simctl", "addmedia", "SIM-UDID", file}
	if len(calls) != 1 || strings.Join(calls[0], " ") != strings.Join(want, " ") {
		t.Errorf("calls = %v, want [%v]", calls, want)
	}
}

func TestRequireSimulatorForAppearance_PhysicalHintIsAccurate(t *testing.T) {
	d := &Driver{info: &core.PlatformInfo{Platform: "ios", IsSimulator: false}}
	err := d.requireSimulatorForAppearance("dark mode")
	if err == nil {
		t.Fatal("dark mode on a physical iPhone must fail")
	}
	msg := err.Error()
	if strings.Contains(msg, "devicelab_ios for") || strings.Contains(msg, "--driver devicelab") {
		t.Errorf("error %q must not recommend the devicelab driver for a physical iPhone", msg)
	}
	for _, want := range []string{"physical iPhone", "runs only on simulators", "Settings"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q, want it to contain %q", msg, want)
		}
	}
}
