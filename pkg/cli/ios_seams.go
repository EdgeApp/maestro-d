package cli

// maestro-d: physical iOS listings and getPhysicalDeviceInfo reach
// usbmuxd and lockdown through these, so tests can answer for them.
// GetDevice and GetValues stay two calls: a GetDevice failure (usbmuxd
// does not list the device) falls back to devicectl, and a GetValues
// failure (a listed phone that is locked or untrusted) does not.

import goios "github.com/danielpaulus/go-ios/ios"

var (
	goiosListDevices = goios.ListDevices
	goiosGetDevice   = goios.GetDevice
	goiosGetValues   = goios.GetValues
)
