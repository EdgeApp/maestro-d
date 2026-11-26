package uiautomator2

// maestro-d: setStatusBar, keyboardVisible and the samplers' probe, through
// the device's adb shell.

import "github.com/devicelab-dev/maestro-runner/pkg/core"

// shellFunc is the device's adb shell, nil when the driver has none.
func (d *Driver) shellFunc() func(string) (string, error) {
	if d.device == nil {
		return nil
	}
	return d.device.Shell
}

// keyboardVisibleResult answers keyboardVisible from dumpsys's input
// method state.
func (d *Driver) keyboardVisibleResult() *core.CommandResult {
	if d.device == nil {
		return core.UnsupportedResult("keyboardVisible needs an adb shell to the device, which this driver has not got")
	}
	return core.KeyboardVisibleResult(d.isKeyboardVisible())
}

// SampleProbe measures an app for startSampler (core.SampleProber).
func (d *Driver) SampleProbe(kind, appID string) (core.SampleFunc, error) {
	return core.AndroidSampleProbe(d.shellFunc(), kind, appID)
}
