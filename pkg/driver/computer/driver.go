// Package computer is the driver for the computer device: the Mac maestro-d
// runs on, driven entirely over a VNC connection to its own Screen Sharing
// (127.0.0.1:5900). It has no UI hierarchy, so it takes points, keys and
// text, and answers with screenshots.
package computer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"sync"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
	"github.com/devicelab-dev/maestro-runner/pkg/rfb"
)

// Platform and DeviceID name the computer device everywhere.
const (
	Platform = "macos"
	DeviceID = "mac"
	Kind     = "computer"
)

// DefaultIdleTimeout is how long an attachment may go without a call.
const DefaultIdleTimeout = 5 * time.Minute

// Config opens a computer driver.
type Config struct {
	Addr        string    // Screen Sharing, normally 127.0.0.1:5900
	Login       rfb.Login // the macOS account from computer.json
	FullName    string    // the macOS user's full name, as a login screen shows it
	DeviceName  string    // the Mac's ComputerName
	OSVersion   string
	IdleTimeout time.Duration
	Recognizer  Recognizer
}

// Driver is an attached computer.
type Driver struct {
	cfg    Config
	client *rfb.Client

	loggedIn  time.Time // when the login finished (waitInput counts from here)
	mu        sync.Mutex
	ctx       context.Context
	pointer   image.Point
	buttons   uint8
	modifiers []uint32 // held while a pressKey runs
}

var dial = rfb.Dial

// New logs in to Screen Sharing with the macOS account and makes sure the
// connection shows the desktop, logging in on a login window or lock screen
// when the screen check recognises one.
func New(ctx context.Context, cfg Config) (*Driver, error) {
	if cfg.Recognizer == nil {
		cfg.Recognizer = VisionRecognizer{}
	}
	client, err := dial(ctx, cfg.Addr, cfg.Login)
	if err != nil {
		return nil, err
	}
	d := &Driver{cfg: cfg, client: client, ctx: context.Background(), loggedIn: time.Now()}
	if err := d.logIn(ctx); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

// Close releases anything held down, then closes the connection.
func (d *Driver) Close() {
	d.mu.Lock()
	buttons, mods, pt := d.buttons, d.modifiers, d.pointer
	d.buttons, d.modifiers = 0, nil
	d.mu.Unlock()
	if buttons != 0 {
		_ = d.client.Pointer(0, pt.X, pt.Y)
	}
	for i := len(mods) - 1; i >= 0; i-- {
		_ = d.client.Key(false, mods[i])
	}
	_ = d.client.Close()
}

// Done is closed when the Screen Sharing connection ends.
func (d *Driver) Done() <-chan struct{} { return d.client.Done() }

func (d *Driver) screen() image.Rectangle {
	w, h := d.client.Size()
	return image.Rect(0, 0, w, h)
}

func (d *Driver) context() context.Context {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ctx
}

// Screenshot is the whole screen as PNG, which flows and assertScreenshot
// use.
func (d *Driver) Screenshot() ([]byte, error) {
	img, err := d.captureScreen(d.context())
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Frame captures a rectangle as pixels (core.FrameGrabber). When the screen
// changed size meanwhile it fails with core.ErrScreenResized, and the
// caller, who chose the rectangle, re-reads the size.
func (d *Driver) Frame(ctx context.Context, rect image.Rectangle) (*image.RGBA, error) {
	img, err := d.client.Capture(ctx, rect)
	if errors.Is(err, rfb.ErrResized) {
		return nil, fmt.Errorf("%w: %w", core.ErrScreenResized, err)
	}
	return img, err
}

// captureScreen captures the whole screen, once more at the new size when
// it changed size during the first try (a login window and the user's
// session can use different resolutions).
func (d *Driver) captureScreen(ctx context.Context) (*image.RGBA, error) {
	img, err := d.client.Capture(ctx, d.screen())
	if errors.Is(err, rfb.ErrResized) {
		img, err = d.client.Capture(ctx, d.screen())
	}
	return img, err
}

// DefaultImage makes the screenshot route answer JPEG unless asked for PNG
// (core.DefaultImager).
func (d *Driver) DefaultImage() string { return "jpeg" }

// IdleTimeout is how long the attachment may go without a call
// (core.IdleTimer).
func (d *Driver) IdleTimeout() time.Duration { return d.cfg.IdleTimeout }

// ScreenshotSize is the screen in pixels (core.ScreenshotSizer).
func (d *Driver) ScreenshotSize() (int, int, error) {
	w, h := d.client.Size()
	return w, h, nil
}

// Hierarchy fails: the computer device reads no UI tree.
func (d *Driver) Hierarchy() ([]byte, error) {
	return nil, fmt.Errorf("the computer device has no UI hierarchy")
}

// GetState has nothing to report beyond the screen.
func (d *Driver) GetState() *core.StateSnapshot { return &core.StateSnapshot{} }

// GetPlatformInfo describes the Mac.
func (d *Driver) GetPlatformInfo() *core.PlatformInfo {
	w, h := d.client.Size()
	return &core.PlatformInfo{
		Platform:     Platform,
		OSVersion:    d.cfg.OSVersion,
		DeviceName:   d.cfg.DeviceName,
		DeviceID:     DeviceID,
		ScreenWidth:  w,
		ScreenHeight: h,
	}
}

func (d *Driver) SetFindTimeout(int)              {}
func (d *Driver) SetWaitForIdleTimeout(int) error { return nil }

func (d *Driver) SetContext(ctx context.Context) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	d.ctx = ctx
}

const noHierarchy = "the computer device has no UI hierarchy; use a point"

// Execute runs one step over VNC.
func (d *Driver) Execute(step flow.Step) *core.CommandResult {
	start := time.Now()
	r := d.execute(step)
	r.Duration = time.Since(start)
	return r
}

// waitInput holds the first key press or pointer event of a session until
// inputReady after the login. Measured on macOS 26: Screen Sharing passes
// modifiers and key-ups at once but drops key-downs and mouse buttons for
// about 3 s after a first login and up to about 7 s after a login that
// follows another session, while the step would still report success.
func (d *Driver) waitInput(ctx context.Context) {
	sleep(ctx, time.Until(d.loggedIn.Add(inputReady)))
}

//nolint:gocyclo
func (d *Driver) execute(step flow.Step) *core.CommandResult {
	select {
	case <-d.client.Done():
		return fail(fmt.Errorf("the Screen Sharing connection closed: %v", d.client.Err()))
	default:
	}
	switch s := step.(type) {
	case *flow.TapOnStep:
		if !s.Selector.IsEmpty() || s.Point == "" {
			return fail(fmt.Errorf(noHierarchy))
		}
		// repeat is the executor's: it calls Execute once per tap
		// (tap_options.go), 100 ms apart, which macOS counts as a
		// double click.
		return d.tapAt(s.Point, s.LongPress, s.DurationMs)
	case *flow.TapOnPointStep:
		if s.Point == "" {
			w, h := d.client.Size()
			return d.tapPoint(clampPoint(image.Pt(s.X, s.Y), w, h), s.LongPress, s.DurationMs)
		}
		return d.tapAt(s.Point, s.LongPress, s.DurationMs)
	case *flow.DoubleTapOnStep, *flow.LongPressOnStep:
		// Both take only a selector; at a point, tapOn's repeat and
		// longPress (or clickOn's count) do the same.
		return fail(fmt.Errorf("%s; for a point use tapOn with repeat: 2 or longPress: true, or clickOn", noHierarchy))
	case *flow.SwipeStep:
		return d.swipe(s)
	case *flow.ScrollStep:
		return d.scroll(s.Direction)
	case *flow.InputTextStep:
		if !s.Selector.IsEmpty() {
			return fail(fmt.Errorf(noHierarchy))
		}
		return result(d.typeText(s.Text), "typed %d characters", len([]rune(s.Text)))
	case *flow.EraseTextStep:
		n := s.Characters
		if n <= 0 {
			n = 50
		}
		for i := 0; i < n; i++ {
			if err := d.tapKey(0xff08); err != nil {
				return fail(err)
			}
		}
		return ok("erased %d characters", n)
	case *flow.PressKeyStep:
		return d.pressKey(s.Key)
	case *flow.TakeScreenshotStep:
		if s.CropOn != nil {
			return fail(fmt.Errorf(noHierarchy))
		}
		return d.screenshotResult()
	case *flow.AssertScreenshotStep:
		if s.CropOn != nil || len(s.Mask) > 0 {
			return fail(fmt.Errorf(noHierarchy))
		}
		return d.screenshotResult()
	case *flow.WaitForAnimationToEndStep:
		return d.waitForStill(s.TimeoutMs)
	case *flow.HideKeyboardStep:
		return ok("no on-screen keyboard")
	case *flow.MoveMouseStep:
		p, err := d.point(s.Point)
		if err != nil {
			return fail(err)
		}
		return result(d.move(p), "moved to %d, %d", p.X, p.Y)
	case *flow.ClickOnStep:
		return d.clickOn(s)
	case *flow.ScrollAtStep:
		p, err := d.point(s.Point)
		if err != nil {
			return fail(err)
		}
		return result(d.wheel(p, s.X, s.Y), "scrolled %d, %d at %d, %d", s.X, s.Y, p.X, p.Y)
	}
	return fail(fmt.Errorf("%s is not supported on the computer device", step.Type()))
}

func (d *Driver) screenshotResult() *core.CommandResult {
	data, err := d.Screenshot()
	if err != nil {
		return fail(err)
	}
	r := core.SuccessResult("screenshot taken", nil)
	r.Data = data
	return r
}

// waitForStill captures the screen until two frames 250 ms apart match.
func (d *Driver) waitForStill(timeoutMs int) *core.CommandResult {
	if timeoutMs <= 0 {
		timeoutMs = 3000
	}
	ctx := d.context()
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	prev, err := d.captureScreen(ctx)
	if err != nil {
		return fail(err)
	}
	for time.Now().Before(deadline) {
		sleep(ctx, stillInterval)
		cur, err := d.captureScreen(ctx)
		if err != nil {
			return fail(err)
		}
		if bytes.Equal(prev.Pix, cur.Pix) {
			return ok("screen is still")
		}
		prev = cur
	}
	return ok("screen still changing after %d ms", timeoutMs)
}

var stillInterval = 250 * time.Millisecond

func ok(format string, args ...any) *core.CommandResult {
	return core.SuccessResult(fmt.Sprintf(format, args...), nil)
}

func fail(err error) *core.CommandResult { return core.ErrorResult(err, "") }

func result(err error, format string, args ...any) *core.CommandResult {
	if err != nil {
		return fail(err)
	}
	return ok(format, args...)
}
