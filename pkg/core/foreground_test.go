package core

import (
	"bytes"
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

func TestWithinLimitReturnsAndGivesUp(t *testing.T) {
	if got := withinLimit(time.Second, func() (string, error) { return "app", nil }); got != "app" {
		t.Errorf("got %q", got)
	}
	if got := withinLimit(time.Second, func() (string, error) { return "app", errors.New("no") }); got != "" {
		t.Errorf("an error should leave the field absent, got %q", got)
	}
	hang := make(chan struct{})
	defer close(hang)
	start := time.Now()
	got := withinLimit(20*time.Millisecond, func() (string, error) {
		<-hang
		return "late", nil
	})
	if got != "" {
		t.Errorf("a hung lookup should give up, got %q", got)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("gave up after %v", elapsed)
	}
	if got := WithinForegroundLimit(func() (string, error) { return "app", nil }); got != "app" {
		t.Errorf("got %q", got)
	}
}

func TestSetForeground(t *testing.T) {
	state := &StateSnapshot{}
	state.SetForeground("")
	if state.AppState != "" || state.ForegroundApp != "" {
		t.Errorf("empty id must leave fields absent: %+v", state)
	}
	state.SetForeground("app")
	if state.AppState != "foreground" || state.ForegroundApp != "app" {
		t.Errorf("got %+v", state)
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
