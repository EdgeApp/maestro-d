package capture

import (
	"errors"
	"os"
	"sync"
)

// DriverCapture is the capture state a device driver keeps: the session
// its creator handed it, and the recording a core.ScreenRecorder runs on
// that session. Drivers embed it as a field and forward Capture,
// StartScreenRecording and StopScreenRecording to it. The zero value has
// no session. The session belongs to whoever created it, who closes it.
type DriverCapture struct {
	mu      sync.Mutex
	session *Session
	rec     *Recorder
}

// Set hands over the device's capture session.
func (c *DriverCapture) Set(s *Session) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.session = s
}

// Session returns the capture session, or an *Unavailable error
// (unsupported-platform) when the driver was given none.
func (c *DriverCapture) Session() (*Session, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session == nil {
		return nil, &Unavailable{Reason: ReasonUnsupportedPlatform, Message: "this driver was not given a screen capture session"}
	}
	return c.session, nil
}

// StartRecording records the session into a temporary file in
// os.TempDir() (the --record target is only named at the stop), at the
// default frame rate, with audio when the device has it.
func (c *DriverCapture) StartRecording() error {
	s, err := c.Session()
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rec != nil {
		return errors.New("a screen recording is already running")
	}
	r, err := NewRecorder(s, os.TempDir(), Options{FPS: DefaultFPS, Audio: true})
	if err != nil {
		return err
	}
	c.rec = r
	return nil
}

// StopRecording finalizes the running recording at dest.
func (c *DriverCapture) StopRecording(dest string) error {
	c.mu.Lock()
	r := c.rec
	c.rec = nil
	c.mu.Unlock()
	if r == nil {
		return errors.New("no screen recording is running")
	}
	return r.Stop(dest)
}
