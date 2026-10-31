package wda

import (
	"fmt"
	"strings"

	"github.com/devicelab-dev/maestro-runner/pkg/simulator"
)

// configureRuntime keeps explicit tvOS sessions separate from iOS build caches.
// The generic Apple driver still defaults to iOS for physical devices.
func (r *Runner) configureRuntime() error {
	if r.runtimeChecked {
		return r.runtimeError
	}
	r.runtimeChecked = true
	list := r.listSimulators
	if list == nil {
		list = simulator.ListSimulators
	}
	devices, err := list()
	if err != nil {
		return nil
	}
	for _, device := range devices {
		if device.UDID != r.deviceUDID || !device.IsAvailable {
			continue
		}
		if device.Platform != "iOS" && device.Platform != "tvOS" {
			r.runtimeError = fmt.Errorf("WDA does not support the %s simulator", device.Platform)
			return r.runtimeError
		}
		r.simulatorPlatform = device.Platform
		r.simulatorVersion = device.OSVersion
		break
	}
	return nil
}

// IsTV reports the explicitly selected runtime after Build has validated it.
func (r *Runner) IsTV() bool { return r.simulatorPlatform == "tvOS" }
func (r *Runner) runnerScheme() string {
	if r.IsTV() {
		return "WebDriverAgentRunner_tvOS"
	}
	return "WebDriverAgentRunner"
}
func tvRemoteButton(key string) string {
	switch strings.ToLower(key) {
	case "up", "dpad_up":
		return "up"
	case "down", "dpad_down":
		return "down"
	case "left", "dpad_left":
		return "left"
	case "right", "dpad_right":
		return "right"
	case "select", "center", "dpad_center", "enter":
		return "select"
	case "back", "menu":
		return "menu"
	case "playpause", "play_pause":
		return "playPause"
	default:
		return ""
	}
}

// SetTVMode enables remote-only keys without changing iOS keyboard behavior.
func (d *Driver) SetTVMode(enabled bool) { d.isTV = enabled }
