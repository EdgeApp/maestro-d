package capture

import (
	"fmt"
	"strconv"
	"strings"
)

// Frame-rate bounds and default, in frames per second.
const (
	MinFPS     = 3
	MaxFPS     = 60
	DefaultFPS = 15
)

// Options are what a subscriber asks of a capture.
type Options struct {
	// FPS is the requested frame rate, MinFPS to MaxFPS; 0 means
	// DefaultFPS. It is a ceiling on sources with FPSControl.
	FPS int
	// Audio asks for the device's audio as well.
	Audio bool
}

func fpsError(v string) error {
	return fmt.Errorf("fps must be an integer from %d to %d, got %q", MinFPS, MaxFPS, v)
}

// Normalize fills the default frame rate and checks the bounds.
func (o Options) Normalize() (Options, error) {
	if o.FPS == 0 {
		o.FPS = DefaultFPS
	}
	if o.FPS < MinFPS || o.FPS > MaxFPS {
		return o, fpsError(strconv.Itoa(o.FPS))
	}
	return o, nil
}

// ParseFPS reads a frame rate as a caller wrote it (a query parameter or a
// flag): empty means DefaultFPS; anything else must be an integer from
// MinFPS to MaxFPS.
func ParseFPS(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return DefaultFPS, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < MinFPS || n > MaxFPS {
		return 0, fpsError(s)
	}
	return n, nil
}
