package cli

// devices_avd.go (maestro-d): Android emulators in device listings carry
// their AVD names, and shut-down AVDs are listed as avd:<name>, so they can
// be booted, shared and deleted by name.

import (
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/emulator"
)

// Seams over the emulator tools, for tests.
var (
	runningAVDsBySerial = emulator.RunningAVDsBySerial
	listAVDNames        = func() []string {
		avds, err := emulator.ListAVDs()
		if err != nil {
			return nil
		}
		names := make([]string, 0, len(avds))
		for _, a := range avds {
			names = append(names, a.Name)
		}
		return names
	}
)

// avdEntries names each running emulator after its AVD and, with
// includeShutdown, appends every AVD that is not running as avd:<name>. A
// running AVD is listed once, under its serial.
func avdEntries(entries []DeviceEntry, includeShutdown bool) []DeviceEntry {
	running := map[string]bool{}
	// androidEntries returns nil when adb discovery failed, and an empty
	// non-nil slice when adb confirmed there are no transports.
	complete := entries != nil
	var bySerial map[string]string
	for i := range entries {
		e := &entries[i]
		if e.Platform != "android" || !emulator.IsEmulator(e.ID) {
			continue
		}
		if bySerial == nil {
			bySerial = runningAVDsBySerial()
		}
		if name := bySerial[e.ID]; name != "" {
			e.Name = name
			e.DeviceType = avdDeviceType(name)
			running[name] = true
		} else {
			complete = false // This serial could be any installed AVD.
		}
	}
	if !includeShutdown {
		return entries
	}
	state := "Shutdown"
	if !complete {
		state = "Unknown"
	}
	for _, name := range listAVDNames() {
		if running[name] {
			continue
		}
		entries = append(entries, DeviceEntry{
			Platform:   "android",
			Kind:       "emulator",
			ID:         daemon.AVDPrefix + name,
			Name:       name,
			DeviceType: avdDeviceType(name),
			State:      state,
		})
	}
	return entries
}

// avdDeviceType distinguishes the configured hardware profile from both the
// user's AVD name and the generic model returned by the emulator's adb.
func avdDeviceType(name string) string {
	if !emulator.ValidAVDName(name) {
		return ""
	}
	return readAVDValue(emulator.AVDConfigPath(name), "hw.device.name")
}
