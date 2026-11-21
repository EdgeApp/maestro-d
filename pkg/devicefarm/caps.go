package devicefarm

import "strings"

// forbiddenCaps are rejected by Device Farm's strictCaps endpoint or pin a
// device it already chose; they are never sent, even from the caller's caps.
var forbiddenCaps = []string{
	"udid", "appium:udid",
	"platformVersion", "appium:platformVersion",
	"app", "appium:app",
	"deviceName", "appium:deviceName",
}

// Capabilities builds the W3C alwaysMatch capabilities for a Device Farm
// session on platform ("android" or "ios"), with extra deep-merged last.
// The device-pinning capabilities are stripped, also from extra and from
// its appium:options.
func Capabilities(platform string, extra map[string]any) map[string]any {
	caps := map[string]any{
		"appium:newCommandTimeout": 300,
		"appium:autoLaunch":        false,
	}
	if strings.EqualFold(platform, "ios") {
		caps["platformName"] = "iOS"
		caps["appium:automationName"] = "XCUITest"
	} else {
		caps["platformName"] = "Android"
		caps["appium:automationName"] = "UiAutomator2"
		caps["appium:autoGrantPermissions"] = true
	}
	mergeCaps(caps, extra)
	strip(caps)
	if opts, ok := caps["appium:options"].(map[string]any); ok {
		strip(opts)
	}
	return caps
}

func strip(m map[string]any) {
	for _, k := range forbiddenCaps {
		delete(m, k)
	}
}

// mergeCaps deep-merges src into dst: nested maps merge, anything else
// replaces. Values from src are copied, so dst never aliases them.
func mergeCaps(dst, src map[string]any) {
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			dm, ok := dst[k].(map[string]any)
			if !ok {
				dm = map[string]any{}
				dst[k] = dm
			}
			mergeCaps(dm, sm)
			continue
		}
		dst[k] = copyValue(v)
	}
}

func copyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := map[string]any{}
		mergeCaps(m, t)
		return m
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = copyValue(e)
		}
		return out
	}
	return v
}
