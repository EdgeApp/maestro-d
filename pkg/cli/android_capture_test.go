package cli

// maestro-d: CreateAndroidDriver's screen capture wiring.

import (
	"errors"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/capture"
	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/device"
	devicelabdriver "github.com/devicelab-dev/maestro-runner/pkg/driver/devicelab"
	"github.com/devicelab-dev/maestro-runner/pkg/driver/mock"
	uia2driver "github.com/devicelab-dev/maestro-runner/pkg/driver/uiautomator2"
)

type captureTestDevice struct{}

func (captureTestDevice) Serial() string               { return "SERIAL" }
func (captureTestDevice) ADBPath() string              { return "adb" }
func (captureTestDevice) Shell(string) (string, error) { return "", errors.New("no device") }
func (captureTestDevice) Push(string, string) error    { return errors.New("no device") }
func (captureTestDevice) ForwardTCPToAbstractSocket(int, string) error {
	return errors.New("no device")
}
func (captureTestDevice) RemoveTCPForward(int) error { return nil }

func TestAttachAndroidCapture(t *testing.T) {
	for name, d := range map[string]core.Driver{
		"uiautomator2": uia2driver.New(nil, &core.PlatformInfo{}, nil),
		"devicelab":    devicelabdriver.New(nil, &core.PlatformInfo{}, nil),
	} {
		t.Run(name, func(t *testing.T) {
			var order []string
			driver, cleanup := attachAndroidCapture(d, func() { order = append(order, "driver") }, captureTestDevice{}, device.DeviceInfo{SDK: "34"})
			sess, err := driver.(core.Capturer).Capture()
			if err != nil || sess == nil || sess.Mode() != capture.OnDemand || !sess.FPSControl() {
				t.Fatalf("Capture = %v, %v", sess, err)
			}
			cleanup()
			if len(order) != 1 {
				t.Errorf("driver cleanup ran %d times", len(order))
			}
			if _, err := sess.Subscribe(capture.Options{}); !errors.Is(err, capture.ErrClosed) {
				t.Errorf("session still open after cleanup: %v", err)
			}
		})
	}
	// No driver cleanup to run first.
	_, cleanup := attachAndroidCapture(uia2driver.New(nil, nil, nil), nil, captureTestDevice{}, device.DeviceInfo{})
	cleanup()
	// A driver without SetCapture is returned as it is.
	m := mock.New(mock.Config{})
	defer m.Close()
	called := false
	driver, cleanup := attachAndroidCapture(m, func() { called = true }, captureTestDevice{}, device.DeviceInfo{})
	cleanup()
	if driver != core.Driver(m) || !called {
		t.Error("a driver without SetCapture was changed")
	}
}
