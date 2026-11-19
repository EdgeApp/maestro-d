package cli

// maestro-d: Android device listings carry the name, model and Android
// version pkg/device reads over adb.

import (
	"github.com/devicelab-dev/maestro-runner/pkg/device"
	"github.com/devicelab-dev/maestro-runner/pkg/logger"
)

// listAndroidDevices is device.ListDevicesWithProps, swappable in tests.
var listAndroidDevices = device.ListDevicesWithProps

// applyAndroidProps copies d's properties onto entry. A phone takes its
// name, model and Android version. An emulator takes its model and
// version only: its name is its AVD's, which avdEntries sets, and AVD ids,
// --share-device and lease selectors match on that name.
func applyAndroidProps(entry *DeviceEntry, d device.DeviceWithProps) {
	entry.Model = d.Model
	entry.OSVersion = d.OSVersion
	if d.Type != "emulator" {
		entry.Name = d.Name
	}
}

// startAndroidEntries starts listing Android devices (adb, the props reads,
// AVD names, and with includeShutdown the shut-down AVDs) and returns a
// function that waits for them. collectDevices lists iOS meanwhile, so a
// listing costs the slower platform, not the sum. It lists nothing for
// another platform.
func startAndroidEntries(platform string, includeShutdown bool) func() []DeviceEntry {
	if platform != "" && platform != "android" {
		return func() []DeviceEntry { return nil }
	}
	done := make(chan []DeviceEntry, 1)
	go func() {
		// A panic here would end the daemon, not just this listing, as it
		// would in the request's own goroutine.
		defer func() {
			if r := recover(); r != nil {
				logger.Error("android device listing panicked: %v", r)
				done <- nil
			}
		}()
		done <- avdEntries(androidEntries(), includeShutdown)
	}()
	return func() []DeviceEntry { return <-done }
}
