package wda

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

// landscapeWDA is a fake WebDriverAgent for an app in landscape: /window/size
// answers the interface-orientation size (852×393 on a 393×852 phone), while
// the driver's PlatformInfo still holds the portrait size measured at attach.
type landscapeWDA struct {
	mu          sync.Mutex
	width       float64
	height      float64
	sizeFails   bool
	sizeCalls   int
	rotation    interface{} // z of GET /rotation; nil answers an error
	orientation string      // last POST /orientation body
	tapX, tapY  float64
	rect        map[string]interface{}
	displayed   bool
	source      string
	screenshot  []byte
}

func newLandscapeWDA() *landscapeWDA {
	return &landscapeWDA{width: 852, height: 393, rotation: 270.0}
}

func (f *landscapeWDA) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, "/window/size"):
		f.sizeCalls++
		if f.sizeFails {
			jsonResponse(w, map[string]interface{}{
				"value": map[string]interface{}{"error": "unknown error", "message": "no window"},
			})
			return
		}
		jsonResponse(w, map[string]interface{}{
			"value": map[string]interface{}{"width": f.width, "height": f.height},
		})
	case strings.HasSuffix(path, "/rotation"):
		if f.rotation == nil {
			jsonResponse(w, map[string]interface{}{
				"value": map[string]interface{}{"error": "unknown error", "message": "no app"},
			})
			return
		}
		jsonResponse(w, map[string]interface{}{
			"value": map[string]interface{}{"x": 0.0, "y": 0.0, "z": f.rotation},
		})
	case strings.HasSuffix(path, "/orientation") && r.Method == http.MethodPost:
		body, _ := io.ReadAll(r.Body)
		var payload map[string]interface{}
		_ = json.Unmarshal(body, &payload)
		f.orientation, _ = payload["orientation"].(string)
		jsonResponse(w, map[string]interface{}{"status": 0})
	case strings.HasSuffix(path, "/wda/tap"):
		body, _ := io.ReadAll(r.Body)
		var payload map[string]interface{}
		_ = json.Unmarshal(body, &payload)
		f.tapX, _ = payload["x"].(float64)
		f.tapY, _ = payload["y"].(float64)
		jsonResponse(w, map[string]interface{}{"status": 0})
	case strings.HasSuffix(path, "/element") && r.Method == http.MethodPost:
		jsonResponse(w, map[string]interface{}{
			"value": map[string]interface{}{"ELEMENT": "elem-1"},
		})
	case strings.HasSuffix(path, "/rect"):
		jsonResponse(w, map[string]interface{}{"value": f.rect})
	case strings.HasSuffix(path, "/displayed"):
		jsonResponse(w, map[string]interface{}{"value": f.displayed})
	case strings.HasSuffix(path, "/text"):
		jsonResponse(w, map[string]interface{}{"value": "Record"})
	case strings.HasSuffix(path, "/name"):
		jsonResponse(w, map[string]interface{}{"value": "XCUIElementTypeButton"})
	case strings.HasSuffix(path, "/source"):
		jsonResponse(w, map[string]interface{}{"value": f.source})
	case strings.HasSuffix(path, "/screenshot"):
		jsonResponse(w, map[string]interface{}{
			"value": base64.StdEncoding.EncodeToString(f.screenshot),
		})
	default:
		jsonResponse(w, map[string]interface{}{"status": 0})
	}
}

// set changes the fake's answers under its lock.
func (f *landscapeWDA) set(change func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change()
}

func (f *landscapeWDA) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sizeCalls
}

// startLandscapeWDA serves f and returns a driver whose PlatformInfo holds the
// portrait size 393×852, as attach stores it.
func startLandscapeWDA(t *testing.T, f *landscapeWDA) *Driver {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	client := &Client{baseURL: server.URL, httpClient: http.DefaultClient, sessionID: "s1"}
	info := &core.PlatformInfo{Platform: "ios", ScreenWidth: 393, ScreenHeight: 852}
	return NewDriver(client, info, "")
}

func TestTapOnPointPercentageUsesLandscapeViewport(t *testing.T) {
	f := newLandscapeWDA()
	d := startLandscapeWDA(t, f)

	result := d.tapOn(&flow.TapOnStep{Point: "90%,50%"})
	if !result.Success {
		t.Fatalf("tapOn failed: %s", result.Message)
	}
	if f.tapX < 766 || f.tapX > 767 || f.tapY < 196 || f.tapY > 197 {
		t.Errorf("tap = (%.0f, %.0f), want about (767, 196)", f.tapX, f.tapY)
	}
	if d.info.ScreenWidth != 852 || d.info.ScreenHeight != 393 {
		t.Errorf("PlatformInfo = %dx%d, want the live 852x393", d.info.ScreenWidth, d.info.ScreenHeight)
	}
}

func TestTapOnPointAbsoluteBeyondPortraitWidthInLandscape(t *testing.T) {
	f := newLandscapeWDA()
	d := startLandscapeWDA(t, f)

	result := d.tapOnPointWithCoords("700, 200")
	if !result.Success {
		t.Fatalf("tap at x=700 refused in landscape: %s", result.Message)
	}
	if f.tapX != 700 || f.tapY != 200 {
		t.Errorf("tap = (%.0f, %.0f), want (700, 200)", f.tapX, f.tapY)
	}
}

func TestGetElementInfoAcceptsLandscapeElementBeyondPortraitWidth(t *testing.T) {
	f := newLandscapeWDA()
	f.rect = map[string]interface{}{"x": 600.0, "y": 100.0, "width": 100.0, "height": 40.0}
	f.displayed = false
	d := startLandscapeWDA(t, f)

	info, err := d.getElementInfo("elem-1")
	if err != nil {
		t.Fatalf("element at x=600 refused in landscape: %v", err)
	}
	if !strings.Contains(info.MatchNote, "852x393") {
		t.Errorf("MatchNote = %q, want the landscape viewport", info.MatchNote)
	}

	// With WDA unable to say, the stored portrait size still applies and
	// the element is outside it.
	f.set(func() { f.sizeFails = true })
	d.invalidateViewport()
	d.info.ScreenWidth, d.info.ScreenHeight = 393, 852
	if _, err := d.getElementInfo("elem-1"); err == nil ||
		!strings.Contains(err.Error(), "bounds outside viewport") {
		t.Errorf("err = %v, want bounds outside viewport against the portrait size", err)
	}
}

func TestPageSourceKeepsLandscapeElementBeyondPortraitWidth(t *testing.T) {
	f := newLandscapeWDA()
	f.source = `<?xml version="1.0" encoding="UTF-8"?>
<AppiumAUT>
  <XCUIElementTypeApplication type="XCUIElementTypeApplication" name="App" enabled="true" visible="true" x="0" y="0" width="852" height="393">
    <XCUIElementTypeButton type="XCUIElementTypeButton" name="record" label="Record" enabled="true" visible="true" x="760" y="170" width="60" height="60"/>
  </XCUIElementTypeApplication>
</AppiumAUT>`
	d := startLandscapeWDA(t, f)

	info, err := d.findElementByPageSourceOnce(flow.Selector{Text: "Record"})
	if err != nil {
		t.Fatalf("element at x=760 filtered out in landscape: %v", err)
	}
	if info.Bounds.X != 760 {
		t.Errorf("bounds.X = %d, want 760", info.Bounds.X)
	}
}

func TestScreenSizeCachesAndRefreshes(t *testing.T) {
	f := newLandscapeWDA()
	d := startLandscapeWDA(t, f)

	mustSize := func(wantW, wantH int) {
		t.Helper()
		w, h, err := d.screenSize()
		if err != nil || w != wantW || h != wantH {
			t.Fatalf("screenSize = %d, %d, %v; want %d, %d", w, h, err, wantW, wantH)
		}
	}

	mustSize(852, 393)
	mustSize(852, 393)
	if got := f.calls(); got != 1 {
		t.Fatalf("within the cache window: %d /window/size calls, want 1", got)
	}

	// setOrientation drops the cache, so the next read sees the new shape.
	f.set(func() { f.width, f.height = 393, 852 })
	if r := d.setOrientation(&flow.SetOrientationStep{Orientation: "PORTRAIT"}); !r.Success {
		t.Fatalf("setOrientation: %s", r.Message)
	}
	mustSize(393, 852)
	if got := f.calls(); got != 2 {
		t.Fatalf("after setOrientation: %d calls, want 2", got)
	}

	// launchApp drops it too.
	if r := d.launchApp(&flow.LaunchAppStep{AppID: "app.example.test"}); !r.Success {
		t.Fatalf("launchApp: %s", r.Message)
	}
	mustSize(393, 852)
	if got := f.calls(); got != 3 {
		t.Fatalf("after launchApp: %d calls, want 3", got)
	}

	// A new session is a new app state.
	d.client.sessionID = "s2"
	mustSize(393, 852)
	if got := f.calls(); got != 4 {
		t.Fatalf("after a session change: %d calls, want 4", got)
	}

	// And the cache expires.
	d.viewport.mu.Lock()
	d.viewport.at = time.Now().Add(-viewportCacheTTL - time.Millisecond)
	d.viewport.mu.Unlock()
	mustSize(393, 852)
	if got := f.calls(); got != 5 {
		t.Fatalf("after expiry: %d calls, want 5", got)
	}
	mustSize(393, 852)
	if got := f.calls(); got != 5 {
		t.Fatalf("right after a refresh: %d calls, want 5", got)
	}
}

func TestScreenSizeFallsBackToStoredSize(t *testing.T) {
	f := newLandscapeWDA()
	f.sizeFails = true
	d := startLandscapeWDA(t, f)

	w, h, err := d.screenSize()
	if err != nil || w != 393 || h != 852 {
		t.Fatalf("screenSize = %d, %d, %v; want the stored 393, 852", w, h, err)
	}

	// A zero size is no better than an error.
	f.set(func() { f.sizeFails, f.width, f.height = false, 0, 0 })
	w, h, err = d.screenSize()
	if err != nil || w != 393 || h != 852 {
		t.Fatalf("zero size: screenSize = %d, %d, %v; want the stored 393, 852", w, h, err)
	}

	// Nothing live and nothing stored is an error.
	f.set(func() { f.sizeFails = true })
	d.info = &core.PlatformInfo{Platform: "ios"}
	if _, _, err := d.screenSize(); err == nil {
		t.Fatal("want an error with no live and no stored size")
	}
}

func TestScreenSizeWithoutClientUsesStoredSize(t *testing.T) {
	d := NewDriver(nil, &core.PlatformInfo{ScreenWidth: 393, ScreenHeight: 852}, "")
	if w, h, err := d.screenSize(); err != nil || w != 393 || h != 852 {
		t.Fatalf("screenSize = %d, %d, %v; want 393, 852", w, h, err)
	}
	d = NewDriver(&Client{}, &core.PlatformInfo{ScreenWidth: 393, ScreenHeight: 852}, "")
	if w, h, err := d.screenSize(); err != nil || w != 393 || h != 852 {
		t.Fatalf("screenSize with an unconnected client = %d, %d, %v", w, h, err)
	}
}

func TestSetOrientationSendsWDAKeys(t *testing.T) {
	cases := map[string]string{
		"PORTRAIT":        "PORTRAIT",
		"portrait":        "PORTRAIT",
		"LANDSCAPE":       "LANDSCAPE",
		"LANDSCAPE_LEFT":  "LANDSCAPE",
		"LANDSCAPE_RIGHT": "UIA_DEVICE_ORIENTATION_LANDSCAPERIGHT",
		"landscape_right": "UIA_DEVICE_ORIENTATION_LANDSCAPERIGHT",
		"UPSIDE_DOWN":     "UIA_DEVICE_ORIENTATION_PORTRAIT_UPSIDEDOWN",
		// WDA's own keys pass through.
		"UIA_DEVICE_ORIENTATION_LANDSCAPERIGHT": "UIA_DEVICE_ORIENTATION_LANDSCAPERIGHT",
	}
	for name, want := range cases {
		f := newLandscapeWDA()
		d := startLandscapeWDA(t, f)
		if r := d.setOrientation(&flow.SetOrientationStep{Orientation: name}); !r.Success {
			t.Fatalf("%s: %s", name, r.Message)
		}
		if f.orientation != want {
			t.Errorf("%s sent %q, want %q", name, f.orientation, want)
		}
	}
}

func TestSetOrientationFailureStillDropsViewport(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, map[string]interface{}{
			"value": map[string]interface{}{"error": "unknown error", "message": "Unable To Rotate Device"},
		})
	}))
	defer server.Close()
	d := createTestDriver(server)
	d.viewport.width, d.viewport.height = 852, 393
	d.viewport.session, d.viewport.at = "test-session", time.Now()

	if r := d.setOrientation(&flow.SetOrientationStep{Orientation: "LANDSCAPE_LEFT"}); r.Success {
		t.Fatal("want failure")
	}
	if d.viewport.width != 0 {
		t.Error("viewport cache kept after setOrientation")
	}
}

func TestGetRotation(t *testing.T) {
	for _, z := range []float64{0, 90, 180, 270} {
		f := newLandscapeWDA()
		f.rotation = z
		d := startLandscapeWDA(t, f)
		got, err := d.client.GetRotation()
		if err != nil || got != int(z) {
			t.Errorf("z=%v: GetRotation = %d, %v", z, got, err)
		}
	}
}

func TestGetRotationErrors(t *testing.T) {
	bodies := map[string]interface{}{
		"unknown z":  map[string]interface{}{"value": map[string]interface{}{"x": -1.0, "y": -1.0, "z": -1.0}},
		"no value":   map[string]interface{}{"status": 0},
		"no z":       map[string]interface{}{"value": map[string]interface{}{"x": 0.0}},
		"WDA error":  map[string]interface{}{"value": map[string]interface{}{"error": "unknown error", "message": "x"}},
		"z a string": map[string]interface{}{"value": map[string]interface{}{"z": "90"}},
	}
	for name, body := range bodies {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasSuffix(r.URL.Path, "/session/s1/rotation") {
				t.Errorf("%s: path %s", name, r.URL.Path)
			}
			jsonResponse(w, body)
		}))
		c := &Client{baseURL: server.URL, httpClient: http.DefaultClient, sessionID: "s1"}
		if _, err := c.GetRotation(); err == nil {
			t.Errorf("%s: want an error", name)
		}
		server.Close()
	}
}

func TestPortraitCropBounds(t *testing.T) {
	// An element on a 852×393 landscape viewport, and one on a 393×852
	// portrait viewport.
	wide := core.Bounds{X: 600, Y: 100, Width: 100, Height: 40}
	tall := core.Bounds{X: 50, Y: 100, Width: 100, Height: 40}
	cases := []struct {
		name         string
		el           core.Bounds
		rotation     int
		viewW, viewH int
		want         core.Bounds
		wantW, wantH int
	}{
		{"90 landscape-left", wide, 90, 852, 393, core.Bounds{X: 100, Y: 152, Width: 40, Height: 100}, 393, 852},
		{"270 landscape-right", wide, 270, 852, 393, core.Bounds{X: 253, Y: 600, Width: 40, Height: 100}, 393, 852},
		{"180 upside down", tall, 180, 393, 852, core.Bounds{X: 243, Y: 712, Width: 100, Height: 40}, 393, 852},
		{"0 portrait", tall, 0, 393, 852, tall, 393, 852},
		{"90 on a portrait viewport", tall, 90, 393, 852, tall, 393, 852},
		{"270 on a portrait viewport", tall, 270, 393, 852, tall, 393, 852},
		{"180 on a landscape viewport", wide, 180, 852, 393, wide, 852, 393},
		{"0 on a landscape viewport", wide, 0, 852, 393, wide, 852, 393},
	}
	for _, c := range cases {
		got, w, h := portraitCropBounds(c.el, c.rotation, c.viewW, c.viewH)
		if got != c.want || w != c.wantW || h != c.wantH {
			t.Errorf("%s: got %+v on %dx%d, want %+v on %dx%d", c.name, got, w, h, c.want, c.wantW, c.wantH)
		}
		// A mapped element turns back by the rotation; an unmapped one not
		// at all.
		wantTurn := 0
		if got != c.el {
			wantTurn = c.rotation
		}
		if turn := framebufferTurn(c.rotation, c.viewW, c.viewH); turn != wantTurn {
			t.Errorf("%s: turn %d, want %d", c.name, turn, wantTurn)
		}
	}
}

// portraitPNG is a white w×h PNG with r painted red.
func portraitPNG(t *testing.T, w, h int, r image.Rectangle) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.RGBA{255, 255, 255, 255}
			if (image.Point{X: x, Y: y}).In(r) {
				c = color.RGBA{255, 0, 0, 255}
			}
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// splitPNG is a white w×h PNG with red painted red and blue painted blue.
func splitPNG(t *testing.T, w, h int, red, blue image.Rectangle) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			p := image.Point{X: x, Y: y}
			c := color.RGBA{255, 255, 255, 255}
			switch {
			case p.In(red):
				c = color.RGBA{255, 0, 0, 255}
			case p.In(blue):
				c = color.RGBA{0, 0, 255, 255}
			}
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestTakeScreenshotCropOnInLandscape(t *testing.T) {
	// The element sits at (600,100 100×40) in landscape points: its left half
	// red, its right half blue. In the portrait framebuffer it is the 40×100
	// block each rotation maps it to, with the halves along the long side.
	// The crop comes back as the element looks on screen: 100×40, red on the
	// left.
	cases := []struct {
		rotation  float64
		red, blue image.Rectangle
	}{
		// Interface x runs down the portrait screen.
		{270, image.Rect(253, 600, 293, 650), image.Rect(253, 650, 293, 700)},
		// Interface x runs up the portrait screen.
		{90, image.Rect(100, 202, 140, 252), image.Rect(100, 152, 140, 202)},
	}
	for _, c := range cases {
		f := newLandscapeWDA()
		f.rotation = c.rotation
		f.rect = map[string]interface{}{"x": 600.0, "y": 100.0, "width": 100.0, "height": 40.0}
		f.displayed = true
		f.screenshot = splitPNG(t, 393, 852, c.red, c.blue)
		d := startLandscapeWDA(t, f)

		sel := flow.Selector{ID: "record"}
		result := d.takeScreenshot(&flow.TakeScreenshotStep{CropOn: &sel})
		if !result.Success {
			t.Fatalf("z=%v: takeScreenshot: %s", c.rotation, result.Message)
		}
		data, _ := result.Data.([]byte)
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("z=%v: decode crop: %v", c.rotation, err)
		}
		b := img.Bounds()
		if b.Dx() != 100 || b.Dy() != 40 {
			t.Fatalf("z=%v: crop is %dx%d, want 100x40", c.rotation, b.Dx(), b.Dy())
		}
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				r, g, bl, _ := img.At(x, y).RGBA()
				wantRed := x-b.Min.X < 50
				if g>>8 != 0 || (wantRed && (r>>8 != 255 || bl>>8 != 0)) || (!wantRed && (r>>8 != 0 || bl>>8 != 255)) {
					t.Fatalf("z=%v: pixel (%d,%d) is %v, want the element's %s half", c.rotation, x, y, img.At(x, y),
						map[bool]string{true: "red", false: "blue"}[wantRed])
				}
			}
		}
	}
}

func TestScreenshotCropSpaceLeavesBoundsWhenItCannotTell(t *testing.T) {
	el := core.Bounds{X: 600, Y: 100, Width: 100, Height: 40}

	f := newLandscapeWDA()
	d := startLandscapeWDA(t, f)

	// Not an image.
	if b, w, h, turn := d.screenshotCropSpace([]byte("not a png"), el, 852, 393); b != el || w != 852 || h != 393 || turn != 0 {
		t.Errorf("undecodable: got %+v %dx%d turn %d", b, w, h, turn)
	}
	// A landscape-shaped screenshot already matches the viewport.
	wide := portraitPNG(t, 852, 393, image.Rectangle{})
	if b, w, h, turn := d.screenshotCropSpace(wide, el, 852, 393); b != el || w != 852 || h != 393 || turn != 0 {
		t.Errorf("landscape PNG: got %+v %dx%d turn %d", b, w, h, turn)
	}
	// No rotation from WDA.
	f.set(func() { f.rotation = nil })
	tall := portraitPNG(t, 393, 852, image.Rectangle{})
	if b, w, h, turn := d.screenshotCropSpace(tall, el, 852, 393); b != el || w != 852 || h != 393 || turn != 0 {
		t.Errorf("rotation unknown: got %+v %dx%d turn %d", b, w, h, turn)
	}
}

func TestTakeScreenshotCropOnFailures(t *testing.T) {
	sel := flow.Selector{ID: "record"}
	step := &flow.TakeScreenshotStep{CropOn: &sel}

	// The element cannot be found.
	notFound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/screenshot"):
			jsonResponse(w, map[string]interface{}{"value": base64.StdEncoding.EncodeToString([]byte("png"))})
		case strings.HasSuffix(r.URL.Path, "/source"):
			jsonResponse(w, map[string]interface{}{"value": `<AppiumAUT></AppiumAUT>`})
		default:
			jsonResponse(w, map[string]interface{}{
				"value": map[string]interface{}{"error": "no such element", "message": "not found"},
			})
		}
	}))
	defer notFound.Close()
	d := createTestDriver(notFound)
	d.SetFindTimeout(50)
	if r := d.takeScreenshot(step); r.Success || !strings.Contains(r.Message, "cropOn: element not found") {
		t.Errorf("not found: %+v", r)
	}

	// No viewport, live or stored.
	f := newLandscapeWDA()
	f.sizeFails = true
	f.rect = map[string]interface{}{"x": 600.0, "y": 100.0, "width": 100.0, "height": 40.0}
	f.displayed = true
	f.screenshot = portraitPNG(t, 393, 852, image.Rectangle{})
	d = startLandscapeWDA(t, f)
	d.info = &core.PlatformInfo{Platform: "ios"}
	if r := d.takeScreenshot(step); r.Success || !strings.Contains(r.Message, "cropOn requires screen dimensions") {
		t.Errorf("no viewport: %+v", r)
	}

	// The element has no area to crop.
	f = newLandscapeWDA()
	f.rect = map[string]interface{}{"x": 600.0, "y": 100.0, "width": 0.0, "height": 0.0}
	f.displayed = true
	f.screenshot = portraitPNG(t, 393, 852, image.Rectangle{})
	d = startLandscapeWDA(t, f)
	if r := d.takeScreenshot(step); r.Success || !strings.HasPrefix(r.Message, "cropOn: ") {
		t.Errorf("empty bounds: %+v", r)
	}
}
