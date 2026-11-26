package devicelab

import (
	"strings"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

// recShell answers dumpsys with out and records every command.
type recShell struct {
	cmds []string
	out  string
}

func (r *recShell) Shell(cmd string) (string, error) {
	r.cmds = append(r.cmds, cmd)
	return r.out, nil
}

func TestEscapeKeyCode(t *testing.T) {
	for _, k := range []string{"escape", "esc", "ESCAPE"} {
		if got := mapKeyCode(k); got != 111 {
			t.Fatalf("%s = %d", k, got)
		}
	}
}

func TestSetStatusBarThroughDemoMode(t *testing.T) {
	sh := &recShell{}
	d := &Driver{device: sh}
	r := d.Execute(&flow.SetStatusBarStep{Mode: "demo"})
	if !r.Success || len(sh.cmds) != 6 || sh.cmds[2] != "am broadcast -a com.android.systemui.demo -e command clock -e hhmm 0941" ||
		!strings.Contains(sh.cmds[3], "battery -e level 100 ") {
		t.Fatalf("%+v %q", r, sh.cmds)
	}
	if r := (&Driver{}).Execute(&flow.SetStatusBarStep{Mode: "live"}); r.Success {
		t.Fatalf("no shell: %+v", r)
	}
}

func TestKeyboardVisible(t *testing.T) {
	sh := &recShell{out: "mFrame=[0,1584][1080,2400]"}
	d := &Driver{device: sh}
	r := d.Execute(&flow.KeyboardVisibleStep{})
	if !r.Success || r.Data.(map[string]any)["visible"] != true {
		t.Fatalf("%+v", r)
	}
	sh.out = "mInputShown=false"
	if r := d.Execute(&flow.KeyboardVisibleStep{}); r.Data.(map[string]any)["visible"] != false {
		t.Fatalf("%+v", r)
	}
	if r := (&Driver{}).Execute(&flow.KeyboardVisibleStep{}); r.Success {
		t.Fatalf("no shell: %+v", r)
	}
}

func TestSampleProbeUsesTheShell(t *testing.T) {
	sh := &recShell{out: "TOTAL PSS: 10"}
	d := &Driver{device: sh}
	probe, err := d.SampleProbe(core.SampleMemory, "com.x")
	if err != nil {
		t.Fatal(err)
	}
	if v, err := probe(); err != nil || v != 10240 || sh.cmds[0] != "dumpsys meminfo 'com.x'" {
		t.Fatalf("%v %v %q", v, err, sh.cmds)
	}
	if _, err := (&Driver{}).SampleProbe(core.SampleCPU, "com.x"); err == nil {
		t.Fatal("no shell")
	}
}
