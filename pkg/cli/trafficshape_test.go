package cli

import (
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

// setTrafficShape with no flags sends the all-defaults shape.
func TestOneShot_SetTrafficShapeNeedsNoFlags(t *testing.T) {
	_, p := oneShotParse(t, "setTrafficShape", "--device", "d")
	v, err := buildStepValue(daemon.CommandSpecs["setTrafficShape"], p)
	if err != nil || v != nil {
		t.Fatalf("value %v err %v", v, err)
	}
	_, p = oneShotParse(t, "setTrafficShape", "--mbit", "1.5", "--direction", "download", "--device", "d")
	v, err = buildStepValue(daemon.CommandSpecs["setTrafficShape"], p)
	m, ok := v.(map[string]any)
	if err != nil || !ok || m["mbit"] != 1.5 || m["direction"] != "download" {
		t.Fatalf("value %#v err %v", v, err)
	}
	_, p = oneShotParse(t, "resetTrafficShape", "--device", "d")
	if v, err := buildStepValue(daemon.CommandSpecs["resetTrafficShape"], p); err != nil || v != nil {
		t.Fatalf("reset: %v %v", v, err)
	}
}
