package wda

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

// keyServer answers keyboardInput with answer (nil for success) and
// records the keys each request named.
func keyServer(t *testing.T, answer map[string]interface{}) (*Driver, *[][]string) {
	t.Helper()
	var got [][]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/wda/element/0/keyboardInput") {
			body, _ := io.ReadAll(r.Body)
			var req struct{ Keys []string }
			_ = json.Unmarshal(body, &req)
			got = append(got, req.Keys)
			if answer != nil {
				jsonResponse(w, map[string]interface{}{"value": answer})
				return
			}
		}
		jsonResponse(w, map[string]interface{}{"value": nil})
	}))
	t.Cleanup(server.Close)
	return createTestDriver(server), &got
}

// The arrows and escape go to typeKey by their XCUIKeyboardKey names.
func TestPressKey_HardwareKeys(t *testing.T) {
	d, got := keyServer(t, nil)
	want := map[string]string{
		"up": "XCUIKeyboardKeyUpArrow", "DPAD_UP": "XCUIKeyboardKeyUpArrow", "arrowUp": "XCUIKeyboardKeyUpArrow",
		"down": "XCUIKeyboardKeyDownArrow", "arrow_down": "XCUIKeyboardKeyDownArrow",
		"left": "XCUIKeyboardKeyLeftArrow", "dpad_left": "XCUIKeyboardKeyLeftArrow",
		"right": "XCUIKeyboardKeyRightArrow", "ArrowRight": "XCUIKeyboardKeyRightArrow",
		"escape": "XCUIKeyboardKeyEscape", "Esc": "XCUIKeyboardKeyEscape",
	}
	for key, name := range want {
		*got = nil
		res := d.pressKey(&flow.PressKeyStep{Key: key})
		if !res.Success || len(*got) != 1 || len((*got)[0]) != 1 || (*got)[0][0] != name {
			t.Errorf("pressKey %s: %+v sent %v", key, res, *got)
		}
	}
	if iosHardwareKey("tab") != "" || iosHardwareKey("enter") != "" {
		t.Error("tab and enter are typed, not typeKey'd")
	}
}

// A device or build without typeKey answers UNSUPPORTED for that key; any
// other refusal is an ordinary failure.
func TestPressKey_HardwareKeyRefused(t *testing.T) {
	d, _ := keyServer(t, map[string]interface{}{"error": "unsupported operation", "message": "typeKey API is only supported since Xcode15 and iPadOS 17"})
	res := d.pressKey(&flow.PressKeyStep{Key: "escape"})
	var unsupported *core.UnsupportedError
	if res.Success || !errors.As(res.Error, &unsupported) || !strings.Contains(res.Message, "pressKey escape") {
		t.Fatalf("old iOS: %+v", res)
	}
	d, _ = keyServer(t, map[string]interface{}{"error": "unknown command", "message": "Unhandled endpoint: /session/x/wda/element/0/keyboardInput"})
	if res := d.pressKey(&flow.PressKeyStep{Key: "up"}); res.Success || !errors.As(res.Error, &unsupported) {
		t.Fatalf("old build: %+v", res)
	}
	d, _ = keyServer(t, map[string]interface{}{"error": "stale element reference", "message": "gone"})
	if res := d.pressKey(&flow.PressKeyStep{Key: "down"}); res.Success || errors.As(res.Error, &unsupported) {
		t.Fatalf("other error: %+v", res)
	}
}
