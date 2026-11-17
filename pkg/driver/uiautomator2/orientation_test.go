package uiautomator2

import (
	"errors"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

// rotatedDriver is a phone attached while an app was in landscape: PlatformInfo
// holds the 2400×1080 size read then, and the device is now in portrait.
func rotatedDriver() (*Driver, *MockUIA2Client) {
	client := &MockUIA2Client{orientationData: "PORTRAIT", deviceInfoSize: "1080x2400"}
	info := &core.PlatformInfo{Platform: "android", ScreenWidth: 2400, ScreenHeight: 1080}
	return New(client, info, &MockShellExecutor{}), client
}

func TestScreenSizeFollowsRotationSinceAttach(t *testing.T) {
	d, _ := rotatedDriver()

	w, h, err := d.screenSize()
	if err != nil {
		t.Fatalf("screenSize: %v", err)
	}
	if w != 1080 || h != 2400 {
		t.Errorf("screenSize = %dx%d, want the portrait 1080x2400", w, h)
	}
	if info := d.GetPlatformInfo(); info.ScreenWidth != 1080 || info.ScreenHeight != 2400 {
		t.Errorf("PlatformInfo = %dx%d, want 1080x2400", info.ScreenWidth, info.ScreenHeight)
	}
}

func TestTapOnPointUsesTheCurrentOrientation(t *testing.T) {
	d, client := rotatedDriver()

	// 50%, 90% of the portrait screen. Against the size read at attach it
	// would be (1200, 972), which is off a 1080-wide screen.
	if result := d.tapOnPointWithCoords("50%, 90%"); !result.Success {
		t.Fatalf("tap failed: %v", result.Error)
	}
	if len(client.clickCalls) != 1 {
		t.Fatalf("expected 1 click, got %d", len(client.clickCalls))
	}
	if got := client.clickCalls[0]; got.X != 540 || got.Y != 2160 {
		t.Errorf("tapped (%d, %d), want (540, 2160)", got.X, got.Y)
	}
}

func TestScreenSizeIsReadAgainAfterARotation(t *testing.T) {
	d, client := rotatedDriver()
	if _, _, err := d.screenSize(); err != nil {
		t.Fatalf("screenSize: %v", err)
	}

	// The device turns to landscape. Once the cached size is stale, the next
	// read sees the new orientation and reads the size again.
	client.orientationData = "LANDSCAPE"
	client.deviceInfoSize = "2400x1080"
	d.invalidateScreenSize()

	w, h, err := d.screenSize()
	if err != nil {
		t.Fatalf("screenSize: %v", err)
	}
	if w != 2400 || h != 1080 {
		t.Errorf("screenSize = %dx%d, want the landscape 2400x1080", w, h)
	}
	if info := d.GetPlatformInfo(); info.ScreenWidth != 2400 || info.ScreenHeight != 1080 {
		t.Errorf("PlatformInfo = %dx%d, want 2400x1080", info.ScreenWidth, info.ScreenHeight)
	}
}

func TestScreenSizeCostsOneOrientationRequestWhileSteady(t *testing.T) {
	d, client := rotatedDriver()
	if _, _, err := d.screenSize(); err != nil {
		t.Fatalf("screenSize: %v", err)
	}
	if client.orientationCalls != 1 || client.deviceInfoCalls != 1 {
		t.Fatalf("first read: %d orientation and %d device-info requests, want 1 and 1",
			client.orientationCalls, client.deviceInfoCalls)
	}

	// Within the TTL nothing is asked.
	for i := 0; i < 5; i++ {
		_, _, _ = d.screenSize()
	}
	if client.orientationCalls != 1 || client.deviceInfoCalls != 1 {
		t.Errorf("within the TTL: %d orientation and %d device-info requests, want 1 and 1",
			client.orientationCalls, client.deviceInfoCalls)
	}

	// Past it, an unchanged orientation keeps the size without reading it.
	d.invalidateScreenSize()
	if _, _, err := d.screenSize(); err != nil {
		t.Fatalf("screenSize: %v", err)
	}
	if client.orientationCalls != 2 || client.deviceInfoCalls != 1 {
		t.Errorf("same orientation: %d orientation and %d device-info requests, want 2 and 1",
			client.orientationCalls, client.deviceInfoCalls)
	}
}

func TestScreenSizeFallsBackToPlatformInfo(t *testing.T) {
	cases := map[string]*MockUIA2Client{
		"no orientation":       {orientationErr: errors.New("server gone")},
		"blank orientation":    {orientationData: ""},
		"no device info":       {orientationData: "PORTRAIT", deviceInfoErr: errors.New("server gone")},
		"unreadable size":      {orientationData: "PORTRAIT", deviceInfoSize: "unknown"},
		"a zero side":          {orientationData: "PORTRAIT", deviceInfoSize: "0x2400"},
		"three sides, somehow": {orientationData: "PORTRAIT", deviceInfoSize: "1x2x3"},
	}
	for name, client := range cases {
		t.Run(name, func(t *testing.T) {
			d := New(client, &core.PlatformInfo{ScreenWidth: 1080, ScreenHeight: 1920}, &MockShellExecutor{})
			w, h, err := d.screenSize()
			if err != nil {
				t.Fatalf("screenSize: %v", err)
			}
			if w != 1080 || h != 1920 {
				t.Errorf("screenSize = %dx%d, want PlatformInfo's 1080x1920", w, h)
			}
		})
	}
}

func TestSetOrientationMakesTheNextReadCheck(t *testing.T) {
	d, client := rotatedDriver()
	if _, _, err := d.screenSize(); err != nil {
		t.Fatalf("screenSize: %v", err)
	}

	client.orientationData = "LANDSCAPE"
	client.deviceInfoSize = "2400x1080"
	d.setOrientation(&flow.SetOrientationStep{Orientation: "LANDSCAPE"})

	// Straight away, inside what would otherwise be the cached size's TTL.
	w, h, err := d.screenSize()
	if err != nil {
		t.Fatalf("screenSize: %v", err)
	}
	if w != 2400 || h != 1080 {
		t.Errorf("after setOrientation, screenSize = %dx%d, want 2400x1080", w, h)
	}
}

func TestParseDisplaySize(t *testing.T) {
	valid := map[string][2]int{"1080x2400": {1080, 2400}, " 2400 x 1080 ": {2400, 1080}}
	for in, want := range valid {
		w, h, ok := parseDisplaySize(in)
		if !ok || w != want[0] || h != want[1] {
			t.Errorf("parseDisplaySize(%q) = %d, %d, %v; want %d, %d, true", in, w, h, ok, want[0], want[1])
		}
	}
	for _, in := range []string{"", "1080", "1080x", "x2400", "0x2400", "-1x2400", "axb", "1x2x3"} {
		if _, _, ok := parseDisplaySize(in); ok {
			t.Errorf("parseDisplaySize(%q) accepted it", in)
		}
	}
}
