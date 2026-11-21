package core

// maestro-d: optional driver capabilities for the computer device.

import (
	"context"
	"errors"
	"image"
	"time"
)

// ErrScreenResized is a FrameGrabber's answer when the screen changed size
// while it captured; the caller re-reads the size and asks again.
var ErrScreenResized = errors.New("the screen changed size during the capture")

// FrameGrabber is a driver that can capture any rectangle of its screen as
// pixels, so the daemon can encode screenshots (JPEG or PNG, whole screen or
// a region) without decoding a PNG first. A capture cut short by a change of
// screen size fails with ErrScreenResized.
type FrameGrabber interface {
	Frame(ctx context.Context, rect image.Rectangle) (*image.RGBA, error)
}

// ConnectionEnder is a driver whose connection to its device can end on its
// own (the computer device when Screen Sharing drops it). Done is closed
// once it has, and the daemon then detaches the device.
type ConnectionEnder interface {
	Done() <-chan struct{}
}

// IdleTimer is a driver whose attachment ends after this long without a
// call. Zero means never.
type IdleTimer interface {
	IdleTimeout() time.Duration
}

// DefaultImager is a driver whose screenshots default to an image type other
// than PNG ("jpeg").
type DefaultImager interface {
	DefaultImage() string
}
