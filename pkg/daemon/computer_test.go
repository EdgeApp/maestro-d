package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
)

var macDevice = DeviceInfo{ID: "mac", Platform: "macos", Kind: "computer", Name: "Demo Mac", OSVersion: "26.6.1", State: "ready", Ready: true}

func TestLeaseSelector_ComputerOnlyWhenAsked(t *testing.T) {
	for _, tc := range []struct {
		sel  LeaseSelector
		want bool
	}{
		{LeaseSelector{}, false},
		{LeaseSelector{OSVersion: "26"}, false},
		{LeaseSelector{Platform: "ios"}, false},
		{LeaseSelector{Platform: "macos"}, true},
		{LeaseSelector{Platform: "macOS", OSVersion: "26"}, true},
		{LeaseSelector{Kind: "computer"}, true},
		{LeaseSelector{Kind: "Computer", OSVersion: "26"}, true},
		{LeaseSelector{Name: "Demo Mac"}, true},
		{LeaseSelector{Name: "mac"}, true},
	} {
		if got := tc.sel.Matches(macDevice); got != tc.want {
			t.Errorf("%s: %v", tc.sel, got)
		}
	}
}

func TestShutdownDevice_ComputerIsANoOp(t *testing.T) {
	if err := shutdownDevice(macDevice); err != nil {
		t.Fatalf("stop mac: %v", err)
	}
	if err := shutdownDevice(DeviceInfo{ID: "phone", Platform: "ios", Kind: "device"}); err == nil || !strings.Contains(err.Error(), "physical device") {
		t.Fatalf("a phone: %v", err)
	}
}

func hasID(list []DeviceInfo, id string) bool {
	for _, d := range list {
		if d.ID == id {
			return true
		}
	}
	return false
}

// A driver factory's own code survives attach: a bad computer.json is
// USAGE, not DEVICE_ERROR.
func TestAttach_KeepsTheFactorysCode(t *testing.T) {
	env := newTestEnv(t, 0)
	env.s.deps.NewDriver = func(context.Context, string, AttachConfig) (core.Driver, DriverInfo, func(), error) {
		return nil, DriverInfo{}, nil, Errorf(CodeUsage, "computer.json is readable by others (mode 0644)")
	}
	_, err := env.c.Attach(context.Background(), "mac", AttachConfig{})
	if codeOf(t, err) != CodeUsage || !strings.Contains(err.Error(), "attach mac: computer.json is readable by others") {
		t.Fatalf("%v", err)
	}
	env.s.deps.NewDriver = func(context.Context, string, AttachConfig) (core.Driver, DriverInfo, func(), error) {
		return nil, DriverInfo{}, nil, errors.New("no such device")
	}
	if _, err := env.c.Attach(context.Background(), "mock-1", AttachConfig{}); codeOf(t, err) != CodeDeviceError {
		t.Fatalf("a plain error: %v", err)
	}
}
