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

// A cached positive answer for another id and an empty --share-device never
// share the computer, nor does a registry record of kind computer under
// another id.
func TestShared_ComputerBeforeCache(t *testing.T) {
	s := &Server{cfg: Config{}, sharedIDs: map[string]bool{"mac": true}, shareSeen: map[string]shareAnswer{}}
	s.registry = NewRegistry("plain", func(string) []DeviceInfo { return nil })
	if s.shared("mac") {
		t.Fatal("shared(mac) with an empty --share-device")
	}
	s.registry.MarkAttached(DeviceInfo{ID: "other-mac", Platform: "macos", Kind: "computer"})
	if s.shared("other-mac") {
		t.Fatal("a computer record shared")
	}
	if s.sharedInfo(macDevice) {
		t.Fatal("sharedInfo(mac)")
	}
	if !s.shared("mock-1") {
		t.Fatal("an ordinary device stopped being shared")
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

func TestCheckRemoteAttach_Computer(t *testing.T) {
	if e := CheckRemoteAttach(&AttachConfig{Platform: "macOS"}); e == nil || e.Code != CodeForbidden || !strings.Contains(e.Message, "local only") {
		t.Fatalf("%v", e)
	}
	if e := CheckRemoteAttach(&AttachConfig{Platform: "android"}); e != nil {
		t.Fatalf("android refused: %v", e)
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
