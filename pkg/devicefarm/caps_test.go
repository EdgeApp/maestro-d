package devicefarm

import (
	"reflect"
	"testing"
)

func TestCapabilitiesBase(t *testing.T) {
	android := Capabilities("android", nil)
	want := map[string]any{
		"platformName": "Android", "appium:automationName": "UiAutomator2",
		"appium:newCommandTimeout": 300, "appium:autoLaunch": false, "appium:autoGrantPermissions": true,
	}
	if !reflect.DeepEqual(android, want) {
		t.Fatalf("android = %v", android)
	}
	ios := Capabilities("iOS", nil)
	want = map[string]any{
		"platformName": "iOS", "appium:automationName": "XCUITest",
		"appium:newCommandTimeout": 300, "appium:autoLaunch": false,
	}
	if !reflect.DeepEqual(ios, want) {
		t.Fatalf("ios = %v", ios)
	}
}

func TestCapabilitiesStripAndDeepMerge(t *testing.T) {
	extra := map[string]any{
		"appium:udid": "X", "udid": "X", "platformVersion": "14", "appium:platformVersion": "14",
		"app": "/tmp/a.apk", "appium:app": "/tmp/a.apk", "deviceName": "d", "appium:deviceName": "d",
		"appium:autoLaunch": true,
		"appium:settings":   map[string]any{"waitForIdleTimeout": 100},
		"appium:options":    map[string]any{"udid": "Y", "noReset": true},
		"appium:otherApps":  []any{"a", map[string]any{"k": "v"}},
	}
	caps := Capabilities("android", extra)
	for _, k := range forbiddenCaps {
		if _, ok := caps[k]; ok {
			t.Errorf("%s kept", k)
		}
	}
	if caps["appium:autoLaunch"] != true {
		t.Error("caller caps did not win")
	}
	if opts := caps["appium:options"].(map[string]any); opts["noReset"] != true || opts["udid"] != nil {
		t.Errorf("appium:options = %v", opts)
	}
	// Nested maps merge into existing ones.
	caps2 := Capabilities("ios", map[string]any{"appium:settings": map[string]any{"a": 1}})
	mergeCaps(caps2, map[string]any{"appium:settings": map[string]any{"b": 2}})
	if s := caps2["appium:settings"].(map[string]any); s["a"] != 1 || s["b"] != 2 {
		t.Errorf("settings = %v", s)
	}
	// No aliasing of the caller's values.
	caps["appium:settings"].(map[string]any)["waitForIdleTimeout"] = 0
	caps["appium:otherApps"].([]any)[1].(map[string]any)["k"] = "changed"
	if extra["appium:settings"].(map[string]any)["waitForIdleTimeout"] != 100 ||
		extra["appium:otherApps"].([]any)[1].(map[string]any)["k"] != "v" {
		t.Error("caps alias the caller's maps")
	}
	if _, ok := extra["appium:udid"]; !ok {
		t.Error("the caller's map was changed")
	}
}
