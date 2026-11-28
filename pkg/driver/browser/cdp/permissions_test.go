package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/flow"
	"github.com/go-rod/rod"
	rodcdp "github.com/go-rod/rod/lib/cdp"
	"github.com/go-rod/rod/lib/proto"
)

// permissionTransport is a fake CDP that records Browser.grantPermissions
// and answers Target.getTargetInfo with pageURL.
type permissionTransport struct {
	mu       sync.Mutex
	events   chan *rodcdp.Event
	pageURL  string
	infoErr  error
	grantErr error
	focusErr error
	grants   []proto.BrowserGrantPermissions
	focused  int
}

func (f *permissionTransport) Event() <-chan *rodcdp.Event { return f.events }
func (f *permissionTransport) Call(ctx context.Context, _, method string, params interface{}) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch method {
	case "Target.getTargetInfo":
		if f.infoErr != nil {
			return nil, f.infoErr
		}
		return json.Marshal(map[string]any{"targetInfo": map[string]any{
			"targetId": "fixture-page", "type": "page", "url": f.pageURL,
		}})
	case "Emulation.setFocusEmulationEnabled":
		f.focused++
		if f.focusErr != nil {
			return nil, f.focusErr
		}
	case "Browser.grantPermissions":
		raw, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		var req proto.BrowserGrantPermissions
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		f.grants = append(f.grants, req)
		if f.grantErr != nil {
			return nil, f.grantErr
		}
	}
	return []byte(`{}`), nil
}

func fakePermissionDriver(t *testing.T, pageURL string) (*Driver, *permissionTransport) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	f := &permissionTransport{events: make(chan *rodcdp.Event, 8), pageURL: pageURL}
	b := rod.New().Context(ctx).NoDefaultDevice().Client(f)
	if err := b.Connect(); err != nil {
		t.Fatal(err)
	}
	p, err := b.PageFromTarget("fixture-page")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); close(f.events) })
	return &Driver{browser: b, page: p, stopCh: make(chan struct{}), tabLabels: make(map[string]*rod.Page)}, f
}

func grantStep(origin string, names ...string) *flow.GrantPermissionsStep {
	return &flow.GrantPermissionsStep{
		BaseStep:    flow.BaseStep{StepType: flow.StepGrantPermissions},
		Permissions: names,
		Origin:      origin,
	}
}

func TestGrantPermissionsMapsMaestroNames(t *testing.T) {
	d, f := fakePermissionDriver(t, "https://App.Example.test:8443/board?x=1#top")
	result := d.Execute(grantStep("", "clipboard-read", "clipboard-write", "camera", "microphone",
		"location", "notifications", "persistent-storage", "screen-wake-lock", "accelerometer",
		"clipboardReadWrite", "videoCapturePanTiltZoom", "midi_sysex"))
	if !result.Success {
		t.Fatalf("grant failed: %v", result.Error)
	}
	if !strings.Contains(result.Message, "https://app.example.test:8443") {
		t.Errorf("message %q does not name the origin", result.Message)
	}
	if len(f.grants) != 1 {
		t.Fatalf("grants = %d, want 1", len(f.grants))
	}
	if f.focused != 1 {
		t.Errorf("focus emulation calls = %d, want 1 for a clipboard grant", f.focused)
	}
	got := f.grants[0]
	if got.Origin != "https://app.example.test:8443" {
		t.Errorf("origin = %q", got.Origin)
	}
	want := []proto.BrowserPermissionType{
		proto.BrowserPermissionTypeClipboardReadWrite,
		proto.BrowserPermissionTypeClipboardSanitizedWrite,
		proto.BrowserPermissionTypeVideoCapture,
		proto.BrowserPermissionTypeAudioCapture,
		proto.BrowserPermissionTypeGeolocation,
		proto.BrowserPermissionTypeNotifications,
		proto.BrowserPermissionTypeDurableStorage,
		proto.BrowserPermissionTypeWakeLockScreen,
		proto.BrowserPermissionTypeSensors,
		proto.BrowserPermissionTypeVideoCapturePanTiltZoom,
		proto.BrowserPermissionTypeMidiSysex,
	}
	if len(got.Permissions) != len(want) {
		t.Fatalf("permissions = %v, want %v", got.Permissions, want)
	}
	for i := range want {
		if got.Permissions[i] != want[i] {
			t.Errorf("permissions[%d] = %q, want %q", i, got.Permissions[i], want[i])
		}
	}
}

func TestGrantPermissionsStepOrigin(t *testing.T) {
	d, f := fakePermissionDriver(t, "https://page.example.test/")
	if r := d.Execute(grantStep("http://Other.example.test:3000/some/path", "clipboard-read")); !r.Success {
		t.Fatalf("grant failed: %v", r.Error)
	}
	if got := f.grants[0].Origin; got != "http://other.example.test:3000" {
		t.Errorf("origin = %q", got)
	}
}

func TestGrantPermissionsNoWebOriginGrantsAll(t *testing.T) {
	for _, u := range []string{"about:blank", "data:text/html,x", "file:///index.html"} {
		d, f := fakePermissionDriver(t, u)
		r := d.Execute(grantStep("", "notifications"))
		if !r.Success {
			t.Fatalf("%s: grant failed: %v", u, r.Error)
		}
		if f.focused != 0 {
			t.Errorf("%s: emulated focus for a non-clipboard grant", u)
		}
		if f.grants[0].Origin != "" || !strings.Contains(r.Message, "all origins") {
			t.Errorf("%s: origin = %q, message %q", u, f.grants[0].Origin, r.Message)
		}
	}
}

func TestGrantPermissionsNoPageGrantsAll(t *testing.T) {
	d, f := fakePermissionDriver(t, "https://page.example.test/")
	d.page = nil
	if r := d.Execute(grantStep("", "clipboard-read")); !r.Success {
		t.Fatalf("grant failed: %v", r.Error)
	}
	if f.grants[0].Origin != "" {
		t.Errorf("origin = %q", f.grants[0].Origin)
	}
}

func TestGrantPermissionsErrors(t *testing.T) {
	t.Run("unknown name", func(t *testing.T) {
		d, f := fakePermissionDriver(t, "https://page.example.test/")
		r := d.Execute(grantStep("", "clipboard-read", "teleport"))
		if r.Success || r.Error == nil {
			t.Fatal("expected an error")
		}
		msg := r.Error.Error()
		for _, s := range []string{`"teleport"`, "clipboard-read", "clipboardReadWrite", "windowManagement"} {
			if !strings.Contains(msg, s) {
				t.Errorf("error %q lacks %s", msg, s)
			}
		}
		if len(f.grants) != 0 {
			t.Error("granted despite the unknown name")
		}
	})
	t.Run("bad origin", func(t *testing.T) {
		d, f := fakePermissionDriver(t, "https://page.example.test/")
		r := d.Execute(grantStep("chrome://settings", "clipboard-read"))
		if r.Success || !strings.Contains(r.Error.Error(), "not an http(s) origin") {
			t.Fatalf("result = %+v", r)
		}
		if len(f.grants) != 0 {
			t.Error("granted despite the bad origin")
		}
	})
	t.Run("page info fails", func(t *testing.T) {
		d, f := fakePermissionDriver(t, "https://page.example.test/")
		f.infoErr = errors.New("target gone")
		r := d.Execute(grantStep("", "clipboard-read"))
		if r.Success || !strings.Contains(r.Error.Error(), "page's origin") {
			t.Fatalf("result = %+v", r)
		}
	})
	t.Run("focus emulation fails", func(t *testing.T) {
		d, f := fakePermissionDriver(t, "https://page.example.test/")
		f.focusErr = errors.New("no session")
		r := d.Execute(grantStep("", "clipboard-write"))
		if r.Success || !strings.Contains(r.Error.Error(), "emulate page focus") {
			t.Fatalf("result = %+v", r)
		}
	})
	t.Run("chrome refuses", func(t *testing.T) {
		d, f := fakePermissionDriver(t, "https://page.example.test/")
		f.grantErr = errors.New("Unknown permission type")
		r := d.Execute(grantStep("", "clipboard-read"))
		if r.Success || !strings.Contains(r.Error.Error(), "Unknown permission type") {
			t.Fatalf("result = %+v", r)
		}
	})
}

func TestPermissionKeyAndWebOrigin(t *testing.T) {
	for _, n := range []string{"clipboard-read", "Clipboard_Read", " clipboard read ", "clipboardRead", "clipboard.read"} {
		if got := permissionKey(n); got != "clipboardread" {
			t.Errorf("permissionKey(%q) = %q", n, got)
		}
	}
	for in, want := range map[string]string{
		"https://a.test/x": "https://a.test",
		"HTTP://A.test:80": "http://a.test:80",
		"a.test":           "",
		"ftp://a.test":     "",
		"::":               "",
	} {
		if got := webOrigin(in); got != want {
			t.Errorf("webOrigin(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestGrantPermissionsClipboardReadRealChrome grants clipboard-read on a real
// headless Chrome and reads back text the page wrote to the clipboard.
func TestGrantPermissionsClipboardReadRealChrome(t *testing.T) {
	ts := newTestServer()
	defer ts.Close()
	d := newTestDriver(t, ts.URL)
	defer d.Close()

	if r := d.Execute(grantStep("", "clipboard-read", "clipboard-write")); !r.Success {
		t.Fatalf("grant failed: %v", r.Error)
	}
	res, err := d.page.Eval(`async () => {
		await navigator.clipboard.writeText('maestro-d clipboard check');
		return await navigator.clipboard.readText();
	}`)
	if err != nil {
		t.Fatalf("clipboard read failed: %v", err)
	}
	if got := res.Value.Str(); got != "maestro-d clipboard check" {
		t.Errorf("readText = %q", got)
	}
}
