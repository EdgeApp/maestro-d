package daemon

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/driver/mock"
	"github.com/devicelab-dev/maestro-runner/pkg/report"
)

// quadrants is an 8x6 image: red, green / blue, white.
func quadrants() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 8, 6))
	for y := 0; y < 6; y++ {
		for x := 0; x < 8; x++ {
			c := color.RGBA{255, 255, 255, 255}
			switch {
			case x < 4 && y < 3:
				c = color.RGBA{255, 0, 0, 255}
			case y < 3:
				c = color.RGBA{0, 255, 0, 255}
			case x < 4:
				c = color.RGBA{0, 0, 255, 255}
			}
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

// pngDriver is the mock with an 8x6 PNG screen.
type pngDriver struct{ *mock.Driver }

func (pngDriver) Screenshot() ([]byte, error) {
	var buf bytes.Buffer
	err := png.Encode(&buf, quadrants())
	return buf.Bytes(), err
}

// frameDriver captures pixels and defaults to JPEG, as the computer does.
type frameDriver struct {
	pngDriver
	frames  atomic.Int32
	asked   atomic.Value
	timeout time.Duration

	mu       sync.Mutex
	size     image.Point  // zero: the 8x6 quadrants
	resizeTo *image.Point // the next Frame fails as the screen changes to this size
	// alwaysResize fails every Frame as a resize.
	alwaysResize bool

	done chan struct{} // closed as a connection that ended
}

func (f *frameDriver) Done() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.done
}

func (f *frameDriver) Frame(_ context.Context, r image.Rectangle) (*image.RGBA, error) {
	f.frames.Add(1)
	f.asked.Store(r)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.resizeTo != nil {
		f.size, f.resizeTo = *f.resizeTo, nil
		return nil, core.ErrScreenResized
	}
	if f.alwaysResize {
		return nil, core.ErrScreenResized
	}
	if f.size != (image.Point{}) {
		if !r.In(image.Rectangle{Max: f.size}) {
			return nil, fmt.Errorf("rectangle %v outside the screen", r)
		}
		return image.NewRGBA(image.Rect(0, 0, r.Dx(), r.Dy())), nil
	}
	return quadrants().SubImage(r).(*image.RGBA), nil
}

// resizeOnNextFrame makes the next Frame fail as the screen changes size.
func (f *frameDriver) resizeOnNextFrame(w, h int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resizeTo = &image.Point{X: w, Y: h}
}

func (*frameDriver) DefaultImage() string { return "jpeg" }
func (f *frameDriver) ScreenshotSize() (int, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.size == (image.Point{}) {
		return 8, 6, nil
	}
	return f.size.X, f.size.Y, nil
}
func (f *frameDriver) IdleTimeout() time.Duration { return f.timeout }
func (f *frameDriver) GetPlatformInfo() *core.PlatformInfo {
	return &core.PlatformInfo{Platform: "macos"}
}

type shotEnv struct {
	c      *Client
	base   string
	frame  *frameDriver
	server *Server
}

func newShotEnv(t *testing.T, idle time.Duration) *shotEnv {
	t.Helper()
	t.Setenv(EnvHome, t.TempDir())
	var cleanups int32
	deps := testDeps(&cleanups, 0)
	env := &shotEnv{frame: &frameDriver{pngDriver: pngDriver{mock.New(mock.Config{Platform: "mock", DeviceID: "pix"})}, timeout: idle, done: make(chan struct{})}}
	inner := deps.NewDriver
	deps.NewDriver = func(ctx context.Context, id string, cfg AttachConfig) (core.Driver, DriverInfo, func(), error) {
		info := DriverInfo{DriverName: "mock", Device: report.Device{ID: id}, Platform: "mock", Kind: "device"}
		switch id {
		case "png":
			return pngDriver{mock.New(mock.Config{Platform: "mock", DeviceID: id})}, info, func() {}, nil
		case "pix":
			return env.frame, info, func() {}, nil
		}
		return inner(ctx, id, cfg)
	}
	env.server = New(Config{Name: "shot", ReportsDir: filepath.Join(t.TempDir(), "reports")}, deps)
	env.server.info = Info{Name: "shot", PID: os.Getpid(), APIVersion: APIVersion, Version: "test"}
	t.Cleanup(env.server.detachAll)
	hs := httptest.NewServer(env.server.Handler())
	t.Cleanup(hs.Close)
	env.base = hs.URL
	env.c = NewHTTPClient(hs.URL, "", nil)
	for _, id := range []string{"mock-1", "png", "pix"} {
		if _, err := env.c.Attach(context.Background(), id, AttachConfig{Platform: "mock"}); err != nil {
			t.Fatal(err)
		}
	}
	return env
}

func (e *shotEnv) get(t *testing.T, id, query string, accept string) (int, http.Header, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, e.base+"/v1/devices/"+id+"/screenshot"+query, nil)
	req.Header.Set("X-Maestro-Api-Version", APIVersion)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header, body
}

func TestScreenshot_DefaultUnchanged(t *testing.T) {
	env := newShotEnv(t, 0)
	want, _ := mock.New(mock.Config{}).Screenshot()
	status, h, body := env.get(t, "mock-1", "", "image/png")
	if status != 200 || !bytes.Equal(body, want) || h.Get("Content-Type") != "image/png" || h.Get("X-Maestro-Screen") != "" {
		t.Fatalf("%d %v %d bytes", status, h, len(body))
	}
	data, ct, err := env.c.ScreenshotWith(context.Background(), "mock-1", ScreenshotOptions{})
	if err != nil || ct != "image/png" || !bytes.Equal(data, want) {
		t.Fatalf("client: %s %v", ct, err)
	}
	if b, err := env.c.Screenshot(context.Background(), "mock-1"); err != nil || !bytes.Equal(b, want) {
		t.Fatalf("Screenshot: %v", err)
	}
	// format=json is unchanged.
	status, _, body = env.get(t, "mock-1", "?format=json", "")
	if status != 200 || !strings.Contains(string(body), `"data"`) {
		t.Fatalf("json: %d %s", status, body)
	}
}

func decodeAny(t *testing.T, b []byte) image.Image {
	t.Helper()
	img, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func near(c color.Color, r, g, b uint8) bool {
	cr, cg, cb, _ := c.RGBA()
	d := func(x uint32, y uint8) bool { v := int(x>>8) - int(y); return v < 40 && v > -40 }
	return d(cr, r) && d(cg, g) && d(cb, b)
}

func TestScreenshot_DecodeCropPath(t *testing.T) {
	env := newShotEnv(t, 0)
	status, h, body := env.get(t, "png", "?image=jpeg&quality=90", "")
	if status != 200 || h.Get("Content-Type") != "image/jpeg" || h.Get("X-Maestro-Screen") != "8,6" || h.Get("X-Maestro-Rect") != "0,0,8,6" {
		t.Fatalf("%d %v", status, h)
	}
	if _, err := jpeg.Decode(bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	status, h, body = env.get(t, "png", "?x=4&y=3&width=4&height=3", "")
	img := decodeAny(t, body)
	if status != 200 || h.Get("Content-Type") != "image/png" || img.Bounds().Dx() != 4 || !near(img.At(1, 1), 255, 255, 255) || h.Get("X-Maestro-Rect") != "4,3,4,3" {
		t.Fatalf("crop: %d %v %v", status, h, img.Bounds())
	}
	// A PNG with nothing else asked is the driver's own bytes.
	want, _ := pngDriver{}.Screenshot()
	if _, _, body = env.get(t, "png", "?image=png", ""); !bytes.Equal(body, want) {
		t.Fatal("image=png re-encoded the driver's PNG")
	}
}

func TestScreenshot_FrameGrabber(t *testing.T) {
	env := newShotEnv(t, 0)
	status, h, body := env.get(t, "pix", "", "image/png")
	if status != 200 || h.Get("Content-Type") != "image/jpeg" || h.Get("X-Maestro-Screen") != "8,6" {
		t.Fatalf("Accept image/png changed the computer's default: %d %v", status, h)
	}
	if img := decodeAny(t, body); img.Bounds().Dx() != 8 {
		t.Fatalf("size %v", img.Bounds())
	}
	status, h, body = env.get(t, "pix", "?image=png&x=0&y=0&width=4&height=3", "")
	if r := env.frame.asked.Load().(image.Rectangle); r != image.Rect(0, 0, 4, 3) {
		t.Fatalf("grabber asked for %v", r)
	}
	img := decodeAny(t, body)
	if status != 200 || h.Get("Content-Type") != "image/png" || !near(img.At(2, 1), 255, 0, 0) {
		t.Fatalf("%d %v", status, h)
	}
	data, ct, err := env.c.ScreenshotWith(context.Background(), "pix", ScreenshotOptions{Image: "jpeg", Quality: 40, Region: &image.Rectangle{Max: image.Pt(2, 2)}})
	if err != nil || ct != "image/jpeg" || decodeAny(t, data).Bounds().Dx() != 2 {
		t.Fatalf("client: %s %v", ct, err)
	}
}

func TestScreenshot_BadOptions(t *testing.T) {
	env := newShotEnv(t, 0)
	for _, q := range []string{"?image=gif", "?quality=0", "?quality=101", "?quality=x", "?x=1&y=1", "?x=0&y=0&width=9&height=1", "?x=-1&y=0&width=2&height=2", "?x=0&y=0&width=0&height=1", "?x=4&y=3&width=-2&height=-2", "?x=4&y=3&width=2&height=-1", "?x=a&y=0&width=1&height=1"} {
		for _, id := range []string{"png", "pix"} {
			status, _, body := env.get(t, id, q, "")
			if status != http.StatusBadRequest || !strings.Contains(string(body), "USAGE") {
				t.Errorf("%s %s: %d %s", id, q, status, body)
			}
		}
	}
	if _, _, body := env.get(t, "pix", "?x=0&y=0&width=9&height=1", ""); !strings.Contains(string(body), "8x6 screen") {
		t.Errorf("message does not name the screen: %s", body)
	}
	if status, _, _ := env.get(t, "png", "?image=jpg", ""); status != 200 {
		t.Error("jpg alias refused")
	}
}

// A capture through the frame path waits for the session and answers like
// any screenshot once the session is closed.
func TestScreenshot_FrameAfterTheSessionCloses(t *testing.T) {
	env := newShotEnv(t, 0)
	env.server.attached["pix"].session.Close()
	before := env.frame.frames.Load()
	status, _, body := env.get(t, "pix", "?image=png", "")
	if status == http.StatusOK || !strings.Contains(string(body), "session is closed") {
		t.Errorf("%d %s", status, body)
	}
	if env.frame.frames.Load() != before {
		t.Error("the driver captured a frame for a closed session")
	}
}

// A capture cut short by a change of screen size is taken again at the new
// size, smaller or larger, and the headers name the new size.
func TestScreenshot_FrameAcrossAResize(t *testing.T) {
	env := newShotEnv(t, 0)
	for _, size := range []image.Point{{4, 3}, {16, 12}} {
		env.frame.resizeOnNextFrame(size.X, size.Y)
		status, h, body := env.get(t, "pix", "?image=png", "")
		if status != http.StatusOK {
			t.Fatalf("%v: %d %s", size, status, body)
		}
		img, err := png.Decode(bytes.NewReader(body))
		if err != nil || img.Bounds().Dx() != size.X || img.Bounds().Dy() != size.Y {
			t.Errorf("%v: image %v %v", size, img.Bounds(), err)
		}
		if want := fmt.Sprintf("%d,%d", size.X, size.Y); h.Get("X-Maestro-Screen") != want {
			t.Errorf("%v: X-Maestro-Screen %q", size, h.Get("X-Maestro-Screen"))
		}
	}
	// A region that no longer fits answers USAGE.
	env.frame.resizeOnNextFrame(4, 3)
	status, _, body := env.get(t, "pix", "?x=8&y=6&width=4&height=3", "")
	if status != http.StatusBadRequest || !strings.Contains(string(body), "4x3 screen") {
		t.Errorf("region outside the new size: %d %s", status, body)
	}
	// It retries once: a screen that keeps changing size is an error.
	env.frame.mu.Lock()
	env.frame.alwaysResize = true
	env.frame.mu.Unlock()
	if status, _, body := env.get(t, "pix", "?image=png", ""); status == http.StatusOK || !strings.Contains(string(body), "changed size") {
		t.Errorf("a screen that keeps resizing: %d %s", status, body)
	}
}
