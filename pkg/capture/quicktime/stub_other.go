//go:build !darwin

package quicktime

import (
	"context"

	"github.com/devicelab-dev/maestro-runner/pkg/capture"
)

// Device is one device's capture. Off macOS it never captures.
type Device struct{ err error }

// Open answers capture.Unavailable{not-macos}: the QuickTime channel
// exists only on macOS.
func Open(udid, name string) *Device {
	return &Device{err: &capture.Unavailable{Reason: capture.ReasonNotMacOS,
		Message: "iOS screen capture needs a macOS host with the device connected over USB"}}
}

// Session is nil: there is no capture.
func (d *Device) Session() *capture.Session { return nil }

// Wait answers not-macos at once.
func (d *Device) Wait(context.Context) Result { return Result{Err: d.err} }

// Close does nothing.
func (d *Device) Close() {}
