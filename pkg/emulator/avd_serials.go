package emulator

// avd_serials.go (maestro-d): which AVD each running emulator is, so a
// listing can name an emulator by its AVD rather than only its serial.

import "strings"

// RunningAVDsBySerial maps each running emulator's adb serial to its AVD
// name, read from `adb -s <serial> shell getprop ro.boot.qemu.avd_name`.
// Upstream's RunningAVDNames returns only the set of names. An emulator
// that does not answer yet (still booting, offline) is left out.
func RunningAVDsBySerial() map[string]string {
	out, err := execCommand("adb", "devices").Output()
	if err != nil {
		return map[string]string{}
	}
	names := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		parts := strings.Fields(strings.TrimSpace(line))
		if len(parts) < 2 || parts[1] != "device" || !IsEmulator(parts[0]) {
			continue
		}
		serial := parts[0]
		nameOut, err := execCommand("adb", "-s", serial, "shell", "getprop", "ro.boot.qemu.avd_name").Output()
		if err != nil {
			continue
		}
		if name := strings.TrimSpace(string(nameOut)); name != "" {
			names[serial] = name
		}
	}
	return names
}
