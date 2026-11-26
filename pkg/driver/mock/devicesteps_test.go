package mock

import (
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

func TestMockDeviceSteps(t *testing.T) {
	d := New(Config{})
	if r := d.Execute(&flow.KeyboardVisibleStep{BaseStep: flow.BaseStep{StepType: flow.StepKeyboardVisible}}); !r.Success || r.Data.(map[string]any)["visible"] != false {
		t.Fatalf("%+v", r)
	}
	for kind, want := range map[string]float64{core.SampleCPU: 12.5, core.SampleMemory: 64 << 20, core.SampleFrames: 60} {
		p, err := d.SampleProbe(kind, "a")
		if err != nil {
			t.Fatal(err)
		}
		if v, _ := p(); v != want {
			t.Fatalf("%s = %v", kind, v)
		}
	}
	if _, err := d.SampleProbe("heat", "a"); err == nil {
		t.Fatal("unknown kind")
	}
}
