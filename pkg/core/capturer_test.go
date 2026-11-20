package core

import (
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/capture"
	"github.com/devicelab-dev/maestro-runner/pkg/capture/fake"
)

// capturingDriver is a driver with screen capture.
type capturingDriver struct {
	stubDriver
	sess *capture.Session
}

func (d *capturingDriver) Capture() (*capture.Session, error) { return d.sess, nil }

// U27: a wrapper (FlutterDriver) does not forward Capturer; Unwrap reaches
// it through any number of layers.
func TestUnwrap_ReachesCapturer(t *testing.T) {
	inner := &capturingDriver{sess: capture.NewSession(fake.New())}
	defer inner.sess.Close()
	wrapped := &wrappingDriver{inner: &wrappingDriver{inner: inner}}
	if _, ok := Driver(wrapped).(Capturer); ok {
		t.Fatal("the wrapper itself must not look like a Capturer")
	}
	c, ok := Unwrap(wrapped).(Capturer)
	if !ok {
		t.Fatal("Unwrap does not reach the Capturer")
	}
	if s, err := c.Capture(); err != nil || s != inner.sess {
		t.Fatalf("Capture: %v %v", s, err)
	}
	if _, ok := Unwrap(&stubDriver{}).(Capturer); ok {
		t.Fatal("a driver without capture")
	}
}
