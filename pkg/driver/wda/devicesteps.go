package wda

// maestro-d: setStatusBar (simulators only, through simctl),
// keyboardVisible and the samplers' probe.

import (
	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

func (d *Driver) isSimulator() bool { return d.info != nil && d.info.IsSimulator }

func (d *Driver) setStatusBar(s *flow.SetStatusBarStep) *core.CommandResult {
	if !d.isSimulator() {
		return core.UnsupportedResult(core.StatusBarPhysicalIOSMessage)
	}
	return core.RunIOSSimulatorStatusBar(d.udid, s.Mode, s.EffectiveTime(), s.EffectiveBattery())
}

func (d *Driver) keyboardVisibleResult() *core.CommandResult {
	visible, err := d.keyboardVisible()
	if err != nil {
		return errorResult(err, "keyboardVisible: "+err.Error())
	}
	return core.KeyboardVisibleResult(visible)
}

// SampleProbe measures an app for startSampler (core.SampleProber).
func (d *Driver) SampleProbe(kind, appID string) (core.SampleFunc, error) {
	return core.IOSSampleProbe(d.udid, d.isSimulator(), kind, appID)
}
