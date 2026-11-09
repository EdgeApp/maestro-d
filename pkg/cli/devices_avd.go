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
			running[name] = true
		}
	}
	if !includeShutdown {
		return entries
	}
	for _, name := range listAVDNames() {
		if running[name] {
			continue
		}
		entries = append(entries, DeviceEntry{
			Platform: "android",
			Kind:     "emulator",
			ID:       daemon.AVDPrefix + name,
			Name:     name,
			State:    "Shutdown",
		})
	}
	return entries
}
