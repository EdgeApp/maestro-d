package cli

import (
	"errors"
	"os/exec"
	"runtime"
	"testing"

	goios "github.com/danielpaulus/go-ios/ios"

	wdadriver "github.com/devicelab-dev/maestro-runner/pkg/driver/wda"
)

// withLockdown answers usbmuxd and lockdown for the go-ios seams: list
// names the UDIDs usbmuxd lists, values each one's lockdown answer (a
// missing UDID fails GetDevice), and valuesErr fails GetValues.
func withLockdown(t *testing.T, list []string, values map[string]goios.AllValuesType, valuesErr error) {
	t.Helper()
	prevList, prevGet, prevValues := goiosListDevices, goiosGetDevice, goiosGetValues
	t.Cleanup(func() { goiosListDevices, goiosGetDevice, goiosGetValues = prevList, prevGet, prevValues })
	goiosListDevices = func() (goios.DeviceList, error) {
		var dl goios.DeviceList
		for _, udid := range list {
			dl.DeviceList = append(dl.DeviceList, goios.DeviceEntry{Properties: goios.DeviceProperties{SerialNumber: udid}})
		}
		return dl, nil
	}
	goiosGetDevice = func(udid string) (goios.DeviceEntry, error) {
		if _, ok := values[udid]; !ok {
			return goios.DeviceEntry{}, errors.New("not listed by usbmuxd")
		}
		return goios.DeviceEntry{Properties: goios.DeviceProperties{SerialNumber: udid}}, nil
	}
	goiosGetValues = func(d goios.DeviceEntry) (goios.GetAllValuesResponse, error) {
		if valuesErr != nil {
			return goios.GetAllValuesResponse{}, valuesErr
		}
		return goios.GetAllValuesResponse{Value: values[d.Properties.SerialNumber]}, nil
	}
}

func withDevicectlInfo(t *testing.T, fn func(string) (wdadriver.DevicectlDevice, error)) {
	t.Helper()
	prev := devicectlDeviceInfo
	t.Cleanup(func() { devicectlDeviceInfo = prev })
	devicectlDeviceInfo = fn
}

const phoneUDID = "FAKE-IOS-UDID-0001"

func TestGetPhysicalDeviceInfoReadsProductType(t *testing.T) {
	withLockdown(t, nil, map[string]goios.AllValuesType{
		phoneUDID: {DeviceName: "Test iPhone", ProductType: "iPhone14,2", ProductVersion: "26.1"},
	}, nil)
	withDevicectlInfo(t, func(string) (wdadriver.DevicectlDevice, error) {
		t.Fatal("devicectl asked about a phone usbmuxd lists")
		return wdadriver.DevicectlDevice{}, nil
	})
	info, err := getPhysicalDeviceInfo(phoneUDID, nil)
	if err != nil || info.Name != "Test iPhone" || info.Model != "iPhone14,2" || info.OSVersion != "26.1" {
		t.Fatalf("got %+v, %v", info, err)
	}
}

func TestGetPhysicalDeviceInfoModelFromDevicectl(t *testing.T) {
	withLockdown(t, nil, nil, nil)
	withDevicectlInfo(t, func(string) (wdadriver.DevicectlDevice, error) {
		return wdadriver.DevicectlDevice{Name: "Test iPad", Model: "iPad14,3", OSVersion: "26.6.1"}, nil
	})
	info, err := getPhysicalDeviceInfo(phoneUDID, nil)
	if err != nil || info.Model != "iPad14,3" || info.Name != "Test iPad" {
		t.Fatalf("got %+v, %v", info, err)
	}
}

// A listed phone that lockdown refuses (locked or untrusted) fails as
// before and never waits on devicectl.
func TestGetPhysicalDeviceInfoValuesFailureSkipsDevicectl(t *testing.T) {
	withLockdown(t, nil, map[string]goios.AllValuesType{phoneUDID: {}}, errors.New("pairing refused"))
	withDevicectlInfo(t, func(string) (wdadriver.DevicectlDevice, error) {
		t.Fatal("devicectl asked about a listed phone lockdown refused")
		return wdadriver.DevicectlDevice{}, nil
	})
	if _, err := getPhysicalDeviceInfo(phoneUDID, nil); err == nil || err.Error() != "failed to get device info: pairing refused" {
		t.Fatalf("err = %v", err)
	}
}

func TestPhysicalIOSEntriesCarryModel(t *testing.T) {
	withLockdown(t, []string{phoneUDID, "LOCKED"}, map[string]goios.AllValuesType{
		phoneUDID: {DeviceName: "Test iPhone", ProductType: "iPhone14,2", ProductVersion: "26.1"},
	}, nil)
	withDevicectlInfo(t, func(string) (wdadriver.DevicectlDevice, error) {
		return wdadriver.DevicectlDevice{}, errors.New("timed out")
	})
	m := entriesByID(physicalIOSEntries())
	if p := m[phoneUDID]; p.Model != "iPhone14,2" || p.Name != "Test iPhone" || p.OSVersion != "26.1" || !p.Ready || p.Kind != "device" {
		t.Errorf("phone %+v", p)
	}
	if l := m["LOCKED"]; l.Model != "" || l.Ready {
		t.Errorf("unreadable phone %+v", l)
	}
}

// Simulator entries carry their device type's model identifier. This reads
// the real simctl, as the other listing tests do, and skips without one.
func TestSimulatorEntriesCarryModel(t *testing.T) {
	if _, err := exec.LookPath("xcrun"); err != nil || runtime.GOOS != "darwin" {
		t.Skip("no simctl")
	}
	entries := simulatorEntries(true)
	if len(entries) == 0 {
		t.Skip("no simulators")
	}
	withModel := 0
	for _, e := range entries {
		if e.Model != "" {
			withModel++
		}
	}
	if withModel == 0 {
		t.Errorf("no simulator entry has a model: %+v", entries[0])
	}
}
