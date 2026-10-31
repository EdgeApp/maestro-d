package core

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"testing"
	"time"
)

func TestForegroundPackageFromDumpsys(t *testing.T) {
	cases := map[string]string{
		"    topResumedActivity=ActivityRecord{1a2b u0 app.teamstation.app/.MainActivity t12}": "app.teamstation.app",
		"  mResumedActivity: ActivityRecord{1a2b u0 com.android.settings/.Settings t3}":        "com.android.settings",
		"nothing resumed here": "",
		"":                     "",
	}
	for input, want := range cases {
		if got := ForegroundPackageFromDumpsys(input); got != want {
			t.Errorf("%q: got %q, want %q", input, got, want)
		}
	}
}

func TestForegroundProbeReturnsAndGivesUp(t *testing.T) {
	var probe ForegroundProbe
	if got := probe.Lookup(func(context.Context) (string, error) { return "app", nil }); got != "app" {
		t.Errorf("got %q", got)
	}
	if got := probe.Lookup(func(context.Context) (string, error) { return "app", errors.New("no") }); got != "" {
		t.Errorf("an error should leave the field absent, got %q", got)
	}
	var deadline time.Time
	probe.Lookup(func(ctx context.Context) (string, error) {
		deadline, _ = ctx.Deadline()
		return "", nil
	})
	if until := time.Until(deadline); until <= 0 || until > ForegroundLookupLimit {
		t.Errorf("the lookup's context should end at the limit, %v away", until)
	}
}

// The lookup's own call is cancelled at the limit, not left running beside
// the next command.
func TestForegroundProbeCancelsTheLookup(t *testing.T) {
	probe := ForegroundProbe{limit: 20 * time.Millisecond}
	ended := make(chan struct{})
	start := time.Now()
	got := probe.Lookup(func(ctx context.Context) (string, error) {
		defer close(ended)
		<-ctx.Done()
		return "late", ctx.Err()
	})
	if got != "" {
		t.Errorf("a timed-out lookup should give up, got %q", got)
	}
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("the lookup was abandoned, not cancelled")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("gave up after %v", elapsed)
	}
	// A lookup that answers just as its context ends is not trusted.
	if got := probe.Lookup(func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return "late", nil
	}); got != "" {
		t.Errorf("an answer after the limit should be dropped, got %q", got)
	}
}

// A lookup that ignores its context holds the probe: polling a wedged device
// starts no second call until the first returns.
func TestForegroundProbeRunsOneLookupAtATime(t *testing.T) {
	probe := ForegroundProbe{limit: 20 * time.Millisecond}
	hang := make(chan struct{})
	finished := make(chan struct{})
	calls := 0
	stuck := func(context.Context) (string, error) {
		calls++
		<-hang
		defer close(finished)
		return "late", nil
	}
	if got := probe.Lookup(stuck); got != "" {
		t.Errorf("got %q", got)
	}
	for i := 0; i < 3; i++ {
		if got := probe.Lookup(func(context.Context) (string, error) {
			t.Error("a second lookup ran while the first was stuck")
			return "", nil
		}); got != "" {
			t.Errorf("got %q", got)
		}
	}
	close(hang)
	<-finished
	deadline := time.Now().Add(time.Second)
	for probe.busy.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := probe.Lookup(func(context.Context) (string, error) { return "app", nil }); got != "app" || calls != 1 {
		t.Errorf("once the stuck lookup returned, lookups run again: %q, %d calls", got, calls)
	}
}

type plainShell struct{ out string }

func (s plainShell) Shell(string) (string, error) { return s.out, nil }

type ctxShell struct {
	plainShell
	got context.Context
}

func (s *ctxShell) ShellContext(ctx context.Context, _ string) (string, error) {
	s.got = ctx
	return "ctx", nil
}

func TestShellWithin(t *testing.T) {
	ctx := context.WithValue(context.Background(), struct{}{}, 1)
	if out, _ := ShellWithin(ctx, plainShell{out: "plain"}, "x"); out != "plain" {
		t.Errorf("a plain shell: %q", out)
	}
	sh := &ctxShell{}
	if out, _ := ShellWithin(ctx, sh, "x"); out != "ctx" || sh.got != ctx {
		t.Errorf("a context shell should get the context: %q", out)
	}
}

func TestSetForeground(t *testing.T) {
	state := &StateSnapshot{}
	state.SetForeground("", "app")
	if state.AppState != "" || state.ForegroundApp != "" {
		t.Errorf("empty id must leave fields absent: %+v", state)
	}
	state.SetForeground("app", "app")
	if state.AppState != "foreground" || state.ForegroundApp != "app" {
		t.Errorf("got %+v", state)
	}
	// The session's app crashed and the launcher came to the front.
	launcher := &StateSnapshot{}
	launcher.SetForeground("com.android.launcher3", "app")
	if launcher.AppState != "background" || launcher.ForegroundApp != "com.android.launcher3" {
		t.Errorf("another app in front is not the app in the foreground: %+v", launcher)
	}
	unknown := &StateSnapshot{}
	unknown.SetForeground("com.android.launcher3", "")
	if unknown.AppState != "" || unknown.ForegroundApp != "com.android.launcher3" {
		t.Errorf("without the session's app, appState stays absent: %+v", unknown)
	}
}

func TestMaskImage(t *testing.T) {
	if _, err := MaskImage(nil, nil, 0, 0); err == nil {
		t.Error("a mask with no screen size must fail")
	}
	if _, err := MaskImage([]byte("not an image"), nil, 10, 10); err == nil {
		t.Error("bad image data must fail")
	}
}

func TestMaskImagePaintsScaledBounds(t *testing.T) {
	source := image.NewRGBA(image.Rect(0, 0, 20, 20))
	for y := 0; y < 20; y++ {
		for x := 0; x < 20; x++ {
			source.Set(x, y, color.White)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, source); err != nil {
		t.Fatal(err)
	}
	// A 10x10 point screen on a 20x20 image: a 2x2 mask at (1,1) covers (2,2) to (6,6).
	masked, err := MaskImage(buf.Bytes(), []Bounds{{X: 1, Y: 1, Width: 2, Height: 2}, {X: 8, Y: 8, Width: 50, Height: 50}}, 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := png.Decode(bytes.NewReader(masked))
	if err != nil {
		t.Fatal(err)
	}
	black := func(x, y int) bool {
		r, g, b, _ := decoded.At(x, y).RGBA()
		return r == 0 && g == 0 && b == 0
	}
	if !black(2, 2) || !black(5, 5) {
		t.Error("the scaled region should be painted")
	}
	if black(1, 1) || black(6, 6) {
		t.Error("outside the region should be untouched")
	}
	if !black(19, 19) {
		t.Error("bounds off the image should clamp, not fail")
	}
}

func TestPadToCommonSize(t *testing.T) {
	if _, _, err := PadToCommonSize([]byte("x"), nil); err == nil {
		t.Fatal("bad expected image must fail")
	}
	same := solidPNG(t, 4, 4)
	if _, _, err := PadToCommonSize(same, []byte("x")); err == nil {
		t.Fatal("bad actual image must fail")
	}
	a, b, err := PadToCommonSize(same, same)
	if err != nil || !bytes.Equal(a, same) || !bytes.Equal(b, same) {
		t.Fatalf("same size must come back untouched: %v", err)
	}
	a, b, err = PadToCommonSize(same, solidPNG(t, 2, 6))
	if err != nil {
		t.Fatal(err)
	}
	stats, err := CompareImages(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalPixels != 24 || stats.DifferingPixels == 0 {
		t.Fatalf("stats = %+v", stats)
	}
}

func solidPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// A driver caches its portrait screen size at setup; after a rotation the
// capture is landscape and the element bounds are in landscape points.
func TestMaskImageFollowsARotatedCapture(t *testing.T) {
	// A 20x10 landscape capture, a cached 5x10 portrait screen (10x5 once
	// turned): a 5x5 point mask at the origin covers the image's left half.
	masked, err := MaskImage(solidPNG(t, 20, 10), []Bounds{{X: 0, Y: 0, Width: 5, Height: 5}}, 5, 10)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := png.Decode(bytes.NewReader(masked))
	if err != nil {
		t.Fatal(err)
	}
	black := func(x, y int) bool {
		r, g, b, _ := decoded.At(x, y).RGBA()
		return r == 0 && g == 0 && b == 0
	}
	if !black(2, 8) || !black(9, 9) {
		t.Error("the mask should cover the left half of the landscape image")
	}
	if black(15, 2) || black(10, 0) {
		t.Error("the mask must not stretch across the top of the landscape image")
	}
}

// The probe is free again by the time Lookup returns, so back-to-back state
// reads on a healthy device all answer.
func TestForegroundProbeIsFreeWhenLookupReturns(t *testing.T) {
	var probe ForegroundProbe
	for i := 0; i < 20000; i++ {
		if got := probe.Lookup(func(context.Context) (string, error) { return "app", nil }); got != "app" {
			t.Fatalf("lookup %d right after the last one got %q", i, got)
		}
		if probe.busy.Load() {
			t.Fatalf("lookup %d returned with the probe still held", i)
		}
	}
}

// A panic in a driver's lookup runs off the request goroutine, so the probe
// recovers it as a failed lookup rather than letting it end the daemon.
func TestForegroundProbeRecoversAPanic(t *testing.T) {
	var probe ForegroundProbe
	if got := probe.Lookup(func(context.Context) (string, error) { panic("device call blew up") }); got != "" {
		t.Errorf("a panicking lookup should read as failed, got %q", got)
	}
	if got := probe.Lookup(func(context.Context) (string, error) { return "app", nil }); got != "app" {
		t.Errorf("the probe should be free after a panic, got %q", got)
	}
}

type failingShell struct{}

func (failingShell) Shell(string) (string, error) { return "", errors.New("adb gone") }

func TestAndroidForegroundApp(t *testing.T) {
	var probe ForegroundProbe
	if got := AndroidForegroundApp(&probe, nil); got != "" {
		t.Errorf("no shell: %q", got)
	}
	resumed := plainShell{out: "  topResumedActivity=ActivityRecord{1a2b u0 com.example.app/.Main t4}"}
	if got := AndroidForegroundApp(&probe, resumed); got != "com.example.app" {
		t.Errorf("resumed package: %q", got)
	}
	if got := AndroidForegroundApp(&probe, failingShell{}); got != "" {
		t.Errorf("a failed shell: %q", got)
	}
}
