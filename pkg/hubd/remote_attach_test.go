package hubd

import (
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"strings"
	"testing"
)

// 5.harness-review.1: Appium capabilities name host files and programs, and
// appiumUrl aims the daemon at any address, so a remote attach gets no
// Appium; the route applies this only to remote callers.
func TestCheckRemoteAttach_Appium(t *testing.T) {
	blob := "blob:sha256:" + strings.Repeat("a", 64) + "/caps.json"
	for _, tc := range []struct {
		name  string
		cfg   daemon.AttachConfig
		field string
	}{
		{"driver appium", daemon.AttachConfig{Platform: "android", Driver: "appium"}, "driver"},
		{"driver Appium", daemon.AttachConfig{Driver: "Appium"}, "driver"},
		{"driver APPIUM padded", daemon.AttachConfig{Platform: "ios", Driver: " APPIUM "}, "driver"},
		{"appiumUrl", daemon.AttachConfig{Platform: "android", AppiumURL: "http://127.0.0.1:4723"}, "appiumUrl"},
		{"capsFile blob", daemon.AttachConfig{Platform: "android", Extra: map[string]any{"capsFile": blob}}, "extra.capsFile"},
		{"capsFile path", daemon.AttachConfig{Platform: "android", Extra: map[string]any{"capsFile": "/tmp/caps.json"}}, "extra.capsFile"},
		{"capsFile non-string", daemon.AttachConfig{Platform: "android", Extra: map[string]any{"capsFile": map[string]any{"appium:app": "/bin/sh"}}}, "extra.capsFile"},
		{"no driver", daemon.AttachConfig{Platform: "android"}, ""},
		{"uiautomator2", daemon.AttachConfig{Platform: "android", Driver: "uiautomator2"}, ""},
		{"devicelab", daemon.AttachConfig{Platform: "ios", Driver: "devicelab"}, ""},
		{"empty capsFile", daemon.AttachConfig{Platform: "android", Extra: map[string]any{"capsFile": ""}}, ""},
	} {
		e := checkRemoteAttach(&tc.cfg)
		switch {
		case tc.field == "" && e != nil:
			t.Errorf("%s: refused: %v", tc.name, e)
		case tc.field != "" && (e == nil || e.Code != daemon.CodeForbidden || e.Details["field"] != tc.field):
			t.Errorf("%s: want FORBIDDEN on %s, got %+v", tc.name, tc.field, e)
		}
	}
}

func TestCheckRemoteAttach_Computer(t *testing.T) {
	if e := checkRemoteAttach(&daemon.AttachConfig{Platform: "macOS"}); e == nil || e.Code != daemon.CodeForbidden || !strings.Contains(e.Message, "local only") {
		t.Fatalf("%v", e)
	}
	if e := checkRemoteAttach(&daemon.AttachConfig{Platform: "android"}); e != nil {
		t.Fatalf("android refused: %v", e)
	}
}
