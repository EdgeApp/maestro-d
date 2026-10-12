package cli

import (
	"errors"
	"strings"
	"testing"

	wdadriver "github.com/devicelab-dev/maestro-runner/pkg/driver/wda"
)

// A device usbmuxd does not list (an iPad on a USB-C network link) is
// described from devicectl instead.
func TestGetPhysicalDeviceInfoFallsBackToDevicectl(t *testing.T) {
	orig := devicectlDeviceInfo
	t.Cleanup(func() { devicectlDeviceInfo = orig })

	devicectlDeviceInfo = func(udid string) (wdadriver.DevicectlDevice, error) {
		return wdadriver.DevicectlDevice{Name: "Test iPad", OSVersion: "26.6.1"}, nil
	}
	info, err := getPhysicalDeviceInfo("00000000-0000000000000000")
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "Test iPad" || info.OSVersion != "26.6.1" || info.IsSimulator {
		t.Errorf("got %+v", info)
	}

	devicectlDeviceInfo = func(string) (wdadriver.DevicectlDevice, error) {
		return wdadriver.DevicectlDevice{OSVersion: "26.6.1"}, nil
	}
	if info, err := getPhysicalDeviceInfo("00000000-0000000000000000"); err != nil || info.Name != "iOS Device" {
		t.Errorf("unnamed device: got %+v, %v", info, err)
	}

	devicectlDeviceInfo = func(string) (wdadriver.DevicectlDevice, error) {
		return wdadriver.DevicectlDevice{}, errors.New("no such device")
	}
	if _, err := getPhysicalDeviceInfo("00000000-0000000000000000"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("unknown device: got %v", err)
	}
}
