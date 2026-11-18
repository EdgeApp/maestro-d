// Package quicktime captures the screen and audio of physical iPhones and
// iPads connected over USB to a Mac, through Apple's QuickTime
// screen-capture channel (CoreMediaIO's kCMIOHardwarePropertyAllowScreenCaptureDevices
// and an AVCaptureSession on the device's muxed capture device).
//
// The capture runs in MaestroDCapture.app, a small signed helper app
// (drivers/ios/MaestroDCapture) that maestro-d builds on first use and
// launches through LaunchServices, so macOS asks for Camera access on the
// helper's behalf. Each maestro-d process runs at most one helper, owned
// by its Host: switching the property on re-enumerates every USB iOS
// device on the Mac, so the switch happens once per process, before any
// of its WebDriverAgent connections exist.
//
// The helper talks protocol 1 over a unix socket the Host listens on: a
// control connection, then one connection per device; each frame is a
// 16-byte little-endian header (type u8, flags u8, reserved u16, pts_us
// i64, len u32) and its payload (protocol.go).
//
// On anything but darwin, Open answers capture.Unavailable{not-macos}.
package quicktime

import "time"

// Result is how a device's capture start ended, as Device.Wait reports it.
type Result struct {
	// Found is true once the helper discovered the device's capture
	// device (status device-found or capturing): the device has then
	// re-enumerated on USB.
	Found bool
	// At is when the latest of those statuses arrived.
	At time.Time
	// Match is how the helper matched the device once capturing: "serial"
	// (the capture device's unique ID is the UDID) or "name".
	Match string
	// Err is nil when the device is capturing, else the
	// *capture.Unavailable that says why it is not.
	Err error
}
