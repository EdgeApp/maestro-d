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
	info, err := getPhysicalDeviceInfo("00000000-0000000000000000", nil)
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != "Test iPad" || info.OSVersion != "26.6.1" || info.IsSimulator {
		t.Errorf("got %+v", info)
	}

	devicectlDeviceInfo = func(string) (wdadriver.DevicectlDevice, error) {
		return wdadriver.DevicectlDevice{OSVersion: "26.6.1"}, nil
	}
	if info, err := getPhysicalDeviceInfo("00000000-0000000000000000", nil); err != nil || info.Name != "iOS Device" {
		t.Errorf("unnamed device: got %+v, %v", info, err)
	}

	devicectlDeviceInfo = func(string) (wdadriver.DevicectlDevice, error) {
		return wdadriver.DevicectlDevice{}, errors.New("no such device")
	}
	_, err = getPhysicalDeviceInfo("00000000-0000000000000000", nil)
	if err == nil || !strings.Contains(err.Error(), "not found") || !strings.Contains(err.Error(), "no such device") {
		t.Errorf("unknown device: got %v", err)
	}
}

// What the WDA runner already learned from devicectl is used, not asked for
// again, and its error is the one the person sees.
func TestGetPhysicalDeviceInfoReusesTheRunnersDevicectlReading(t *testing.T) {
	orig := devicectlDeviceInfo
	t.Cleanup(func() { devicectlDeviceInfo = orig })
	devicectlDeviceInfo = func(string) (wdadriver.DevicectlDevice, error) {
		t.Fatal("devicectl asked again")
		return wdadriver.DevicectlDevice{}, nil
	}

	timedOut := errors.New("devicectl device info details timed out after 10s")
	_, err := getPhysicalDeviceInfo("00000000-0000000000000000", &wdadriver.DevicectlLookup{Err: timedOut})
	if err == nil || !errors.Is(err, timedOut) || !strings.Contains(err.Error(), "timed out after 10s") || !strings.Contains(err.Error(), "usbmuxd: ") {
		t.Errorf("locked device: got %v", err)
	}

	info, err := getPhysicalDeviceInfo("00000000-0000000000000000", &wdadriver.DevicectlLookup{
		Device: wdadriver.DevicectlDevice{Name: "Test iPad", OSVersion: "26.6.1"},
	})
	if err != nil || info.Name != "Test iPad" || info.OSVersion != "26.6.1" {
		t.Errorf("known device: got %+v, %v", info, err)
	}
}
