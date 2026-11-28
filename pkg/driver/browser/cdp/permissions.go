package cdp

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/go-rod/rod/lib/proto"
)

// browserPermissions maps a permission name, folded by permissionKey, to the
// Browser.grantPermissions type Chrome takes. It accepts Maestro's and the
// web Permissions API's names (clipboard-read, camera, microphone, location,
// persistent-storage, …) as well as every CDP type under its own name
// (clipboardReadWrite, videoCapture, …).
var browserPermissions = func() map[string]proto.BrowserPermissionType {
	m := map[string]proto.BrowserPermissionType{
		// Maestro and Permissions API names whose CDP type differs.
		"clipboard":              proto.BrowserPermissionTypeClipboardReadWrite,
		"clipboardread":          proto.BrowserPermissionTypeClipboardReadWrite,
		"clipboardwrite":         proto.BrowserPermissionTypeClipboardSanitizedWrite,
		"camera":                 proto.BrowserPermissionTypeVideoCapture,
		"camerapantiltzoom":      proto.BrowserPermissionTypeVideoCapturePanTiltZoom,
		"microphone":             proto.BrowserPermissionTypeAudioCapture,
		"location":               proto.BrowserPermissionTypeGeolocation,
		"notification":           proto.BrowserPermissionTypeNotifications,
		"push":                   proto.BrowserPermissionTypeNotifications,
		"persistentstorage":      proto.BrowserPermissionTypeDurableStorage,
		"screenwakelock":         proto.BrowserPermissionTypeWakeLockScreen,
		"systemwakelock":         proto.BrowserPermissionTypeWakeLockSystem,
		"accelerometer":          proto.BrowserPermissionTypeSensors,
		"gyroscope":              proto.BrowserPermissionTypeSensors,
		"magnetometer":           proto.BrowserPermissionTypeSensors,
		"ambientlightsensor":     proto.BrowserPermissionTypeSensors,
		"displaycapture":         proto.BrowserPermissionTypeDisplayCapture,
		"windowplacement":        proto.BrowserPermissionTypeWindowManagement,
		"backgroundperiodicsync": proto.BrowserPermissionTypePeriodicBackgroundSync,
	}
	for _, t := range []proto.BrowserPermissionType{
		proto.BrowserPermissionTypeAccessibilityEvents,
		proto.BrowserPermissionTypeAudioCapture,
		proto.BrowserPermissionTypeBackgroundSync,
		proto.BrowserPermissionTypeBackgroundFetch,
		proto.BrowserPermissionTypeCapturedSurfaceControl,
		proto.BrowserPermissionTypeClipboardReadWrite,
		proto.BrowserPermissionTypeClipboardSanitizedWrite,
		proto.BrowserPermissionTypeDisplayCapture,
		proto.BrowserPermissionTypeDurableStorage,
		proto.BrowserPermissionTypeGeolocation,
		proto.BrowserPermissionTypeIdleDetection,
		proto.BrowserPermissionTypeLocalFonts,
		proto.BrowserPermissionTypeMidi,
		proto.BrowserPermissionTypeMidiSysex,
		proto.BrowserPermissionTypeNfc,
		proto.BrowserPermissionTypeNotifications,
		proto.BrowserPermissionTypePaymentHandler,
		proto.BrowserPermissionTypePeriodicBackgroundSync,
		proto.BrowserPermissionTypeProtectedMediaIdentifier,
		proto.BrowserPermissionTypeSensors,
		proto.BrowserPermissionTypeStorageAccess,
		proto.BrowserPermissionTypeSpeakerSelection,
		proto.BrowserPermissionTypeTopLevelStorageAccess,
		proto.BrowserPermissionTypeVideoCapture,
		proto.BrowserPermissionTypeVideoCapturePanTiltZoom,
		proto.BrowserPermissionTypeWakeLockScreen,
		proto.BrowserPermissionTypeWakeLockSystem,
		proto.BrowserPermissionTypeWindowManagement,
	} {
		m[permissionKey(string(t))] = t
	}
	return m
}()

// permissionKey folds a permission name so clipboard-read, clipboard_read,
// Clipboard Read and clipboardRead are the same key.
func permissionKey(name string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '-', '_', ' ', '.':
			return -1
		}
		return r
	}, strings.ToLower(strings.TrimSpace(name)))
}

// browserPermissionTypes maps names to the CDP types to grant, in order and
// without repeats; an unknown name is an error listing the known names.
func browserPermissionTypes(names []string) ([]proto.BrowserPermissionType, error) {
	out := make([]proto.BrowserPermissionType, 0, len(names))
	seen := make(map[proto.BrowserPermissionType]bool, len(names))
	for _, name := range names {
		t, ok := browserPermissions[permissionKey(name)]
		if !ok {
			return nil, fmt.Errorf("unknown browser permission %q (known: %s)", name, knownPermissionNames())
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out, nil
}

// grantsClipboard reports whether perms holds a clipboard type.
func grantsClipboard(perms []proto.BrowserPermissionType) bool {
	for _, t := range perms {
		if t == proto.BrowserPermissionTypeClipboardReadWrite || t == proto.BrowserPermissionTypeClipboardSanitizedWrite {
			return true
		}
	}
	return false
}

// knownPermissionNames lists the Maestro-style names and the CDP types.
func knownPermissionNames() string {
	names := []string{
		"clipboard-read", "clipboard-write", "camera", "microphone", "location",
		"notifications", "persistent-storage", "screen-wake-lock", "accelerometer",
	}
	cdp := make([]string, 0, 32)
	seen := make(map[proto.BrowserPermissionType]bool)
	for _, t := range browserPermissions {
		if !seen[t] {
			seen[t] = true
			cdp = append(cdp, string(t))
		}
	}
	sort.Strings(cdp)
	return strings.Join(names, ", ") + ", …; CDP types: " + strings.Join(cdp, ", ")
}

// permissionOrigin is the scheme://host[:port] a grant applies to: the
// step's origin when given, else the current page's. A page with no web
// origin (about:blank, data:, file:) gives "", which grants to all origins.
func (d *Driver) permissionOrigin(origin string) (string, error) {
	if origin != "" {
		o := webOrigin(origin)
		if o == "" {
			return "", fmt.Errorf("origin %q is not an http(s) origin", origin)
		}
		return o, nil
	}
	if d.page == nil {
		return "", nil
	}
	info, err := d.page.Info()
	if err != nil {
		return "", fmt.Errorf("read the page's origin: %w", err)
	}
	return webOrigin(info.URL), nil
}

// webOrigin is raw's scheme://host[:port], or "" when raw is not http(s).
func webOrigin(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return ""
	}
	return scheme + "://" + strings.ToLower(u.Host)
}
