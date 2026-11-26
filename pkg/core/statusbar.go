package core

// maestro-d: setStatusBar. Android emulators and phones show System UI's
// demo mode; iOS simulators take simctl's status_bar override. A physical
// iPhone or iPad has no override; the executor refuses it before a driver
// is asked.

import (
	"fmt"
	"os/exec"
	"strings"
)

// Status bar modes, as setStatusBar names them (flow.StatusBarDemo,
// flow.StatusBarLive).
const (
	statusBarDemo = "demo"
	statusBarLive = "live"
)

// StatusBarPhysicalIOSMessage is setStatusBar's answer on a physical
// iPhone, iPad or Apple TV, whose status bar iOS lets nothing override.
const StatusBarPhysicalIOSMessage = "UNSUPPORTED: status bar override on a physical iOS device"

// RunXcrun runs xcrun with args on this host and returns its combined
// output. A variable so tests see the command line instead.
var RunXcrun = func(args ...string) ([]byte, error) {
	return exec.Command("xcrun", args...).CombinedOutput()
}

// AndroidStatusBarCommands are the device shell commands that put System
// UI into demo mode with clock (H:MM) and battery (0–100), full wifi and
// no notification icons, or, for live, take it out again.
func AndroidStatusBarCommands(mode, clock string, battery int) ([]string, error) {
	const demo = "am broadcast -a com.android.systemui.demo -e command "
	switch mode {
	case statusBarLive:
		return []string{demo + "exit"}, nil
	case statusBarDemo:
	default:
		return nil, fmt.Errorf("setStatusBar: unknown mode %q", mode)
	}
	h, m, _ := strings.Cut(clock, ":")
	if len(h) == 1 {
		h = "0" + h
	}
	return []string{
		"settings put global sysui_demo_allowed 1",
		demo + "enter",
		demo + "clock -e hhmm " + h + m,
		fmt.Sprintf("%sbattery -e level %d -e plugged false", demo, battery),
		demo + "network -e wifi show -e level 4 -e mobile show -e datatype none -e level 4",
		demo + "notifications -e visible false",
	}, nil
}

// RunAndroidStatusBar sends AndroidStatusBarCommands through shell, one
// at a time, stopping at the first that fails.
func RunAndroidStatusBar(shell func(string) (string, error), mode, clock string, battery int) *CommandResult {
	cmds, err := AndroidStatusBarCommands(mode, clock, battery)
	if err != nil {
		return &CommandResult{Success: false, Error: err, Message: err.Error()}
	}
	if shell == nil {
		err := fmt.Errorf("setStatusBar: no adb shell to the device")
		return &CommandResult{Success: false, Error: err, Message: err.Error()}
	}
	for _, c := range cmds {
		if _, err := shell(c); err != nil {
			err = fmt.Errorf("setStatusBar: %s: %w", c, err)
			return &CommandResult{Success: false, Error: err, Message: err.Error()}
		}
	}
	return statusBarResult(mode, clock, battery)
}

// IOSStatusBarArgs are xcrun's arguments for a simulator's status bar:
// simctl status_bar override for demo, clear for live.
func IOSStatusBarArgs(udid, mode, clock string, battery int) ([]string, error) {
	switch mode {
	case statusBarLive:
		return []string{"simctl", "status_bar", udid, "clear"}, nil
	case statusBarDemo:
		return []string{"simctl", "status_bar", udid, "override",
			"--time", clock, "--batteryState", "charged", "--batteryLevel", fmt.Sprint(battery),
			"--wifiBars", "3", "--cellularBars", "4", "--dataNetwork", "wifi"}, nil
	}
	return nil, fmt.Errorf("setStatusBar: unknown mode %q", mode)
}

// RunIOSSimulatorStatusBar sets a simulator's status bar through simctl.
func RunIOSSimulatorStatusBar(udid, mode, clock string, battery int) *CommandResult {
	args, err := IOSStatusBarArgs(udid, mode, clock, battery)
	if err == nil && udid == "" {
		err = fmt.Errorf("setStatusBar: the simulator's UDID is unknown")
	}
	if err != nil {
		return &CommandResult{Success: false, Error: err, Message: err.Error()}
	}
	if out, err := RunXcrun(args...); err != nil {
		err = fmt.Errorf("setStatusBar: simctl status_bar: %w: %s", err, strings.TrimSpace(string(out)))
		return &CommandResult{Success: false, Error: err, Message: err.Error()}
	}
	return statusBarResult(mode, clock, battery)
}

func statusBarResult(mode, clock string, battery int) *CommandResult {
	if mode == statusBarLive {
		return &CommandResult{Success: true, Message: "Status bar is live", Data: map[string]any{"mode": mode}}
	}
	return &CommandResult{Success: true, Message: fmt.Sprintf("Status bar in demo mode (%s, battery %d%%)", clock, battery),
		Data: map[string]any{"mode": mode, "time": clock, "battery": battery}}
}

// KeyboardVisibleResult answers keyboardVisible: data {"visible": v}.
func KeyboardVisibleResult(visible bool) *CommandResult {
	msg := "Keyboard is hidden"
	if visible {
		msg = "Keyboard is visible"
	}
	return &CommandResult{Success: true, Message: msg, Data: map[string]any{"visible": visible}}
}
