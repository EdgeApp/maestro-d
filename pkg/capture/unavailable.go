package capture

import "errors"

// Reasons an Unavailable error names. The daemon reports them as
// CAPTURE_UNAVAILABLE's details.reason.
const (
	ReasonSimulator           = "simulator"
	ReasonUnsupportedPlatform = "unsupported-platform" // web, tvOS, a mock without capture
	ReasonNotMacOS            = "not-macos"
	ReasonCameraPermission    = "camera-permission"
	ReasonNoCaptureDevice     = "no-capture-device"
	ReasonAmbiguousDeviceName = "ambiguous-device-name"
	ReasonHelperFailed        = "helper-failed"
	ReasonAndroidTooOld       = "android-too-old" // SDK < 21
	ReasonScrcpyFailed        = "scrcpy-failed"
	ReasonCaptureLost         = "capture-lost"
	ReasonNoFrames            = "no-frames" // the stream route: no config packet within 10 s
)

// Unavailable says why a device has no capture, or why its capture ended.
type Unavailable struct {
	Reason  string
	Message string
}

func (e *Unavailable) Error() string {
	if e.Message == "" {
		return "screen capture unavailable: " + e.Reason
	}
	return "screen capture unavailable (" + e.Reason + "): " + e.Message
}

// ReasonOf returns the reason of the *Unavailable in err's chain, or
// fallback when there is none.
func ReasonOf(err error, fallback string) string {
	var u *Unavailable
	if errors.As(err, &u) {
		return u.Reason
	}
	return fallback
}
