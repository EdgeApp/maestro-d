package rfb

import "time"

// SetCaptureTimeoutForTest shortens Capture's wait; call the result to
// restore it.
func SetCaptureTimeoutForTest(d time.Duration) func() {
	old := captureTimeout
	captureTimeout = d
	return func() { captureTimeout = old }
}

// SetHandshakeTimeoutForTest shortens Dial's handshake deadline.
func SetHandshakeTimeoutForTest(d time.Duration) func() {
	old := handshakeTimeout
	handshakeTimeout = d
	return func() { handshakeTimeout = old }
}
