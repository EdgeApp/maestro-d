package core

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func encodeGradientPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode test png: %v", err)
	}
	return buf.Bytes()
}

func decodePNGSize(t *testing.T, data []byte) (int, int) {
	t.Helper()
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode cropped png: %v", err)
	}
	return cfg.Width, cfg.Height
}

func TestCropScreenshot_HappyPath(t *testing.T) {
	src := encodeGradientPNG(t, 1000, 2000)
	bounds := Bounds{X: 100, Y: 200, Width: 300, Height: 400}
	out, err := CropScreenshot(src, bounds, 1000, 2000)
	if err != nil {
		t.Fatalf("crop: %v", err)
	}
	w, h := decodePNGSize(t, out)
	if w != 300 || h != 400 {
		t.Errorf("expected cropped 300x400, got %dx%d", w, h)
	}
}

// Devicelab Android agent downscales screenshots to 50% before transmit.
// Bounds (in device-pixel space) must scale proportionally to image space.
func TestCropScreenshot_HalfResScreenshot(t *testing.T) {
	src := encodeGradientPNG(t, 500, 1000)                    // screenshot at 50% of device pixels
	bounds := Bounds{X: 200, Y: 400, Width: 400, Height: 600} // device-pixel bounds
	out, err := CropScreenshot(src, bounds, 1000, 2000)       // screen is 1000x2000 device pixels
	if err != nil {
		t.Fatalf("crop: %v", err)
	}
	// Bounds scaled by image/screen ratio (0.5) → expect 200x300 in image pixels.
	w, h := decodePNGSize(t, out)
	if w != 200 || h != 300 {
		t.Errorf("expected cropped 200x300 (scaled), got %dx%d", w, h)
	}
}

func TestCropScreenshot_ClampsToImage(t *testing.T) {
	src := encodeGradientPNG(t, 800, 600)
	bounds := Bounds{X: 700, Y: 500, Width: 200, Height: 200} // extends past image edge
	out, err := CropScreenshot(src, bounds, 800, 600)
	if err != nil {
		t.Fatalf("crop: %v", err)
	}
	w, h := decodePNGSize(t, out)
	if w != 100 || h != 100 {
		t.Errorf("expected clamped 100x100, got %dx%d", w, h)
	}
}

func TestCropScreenshot_RejectsZeroBounds(t *testing.T) {
	src := encodeGradientPNG(t, 100, 100)
	if _, err := CropScreenshot(src, Bounds{X: 0, Y: 0, Width: 0, Height: 50}, 100, 100); err == nil {
		t.Error("expected error for zero-width bounds")
	}
	if _, err := CropScreenshot(src, Bounds{X: 0, Y: 0, Width: 50, Height: 0}, 100, 100); err == nil {
		t.Error("expected error for zero-height bounds")
	}
}

func TestCropScreenshot_RejectsEmptyData(t *testing.T) {
	if _, err := CropScreenshot(nil, Bounds{X: 0, Y: 0, Width: 10, Height: 10}, 100, 100); err == nil {
		t.Error("expected error for nil data")
	}
}

func TestCropScreenshot_RejectsZeroScreenDims(t *testing.T) {
	src := encodeGradientPNG(t, 100, 100)
	if _, err := CropScreenshot(src, Bounds{X: 0, Y: 0, Width: 10, Height: 10}, 0, 100); err == nil {
		t.Error("expected error for zero screen width")
	}
}

func TestCropScreenshot_FullyOffScreenBoundsErrors(t *testing.T) {
	src := encodeGradientPNG(t, 100, 100)
	// Bounds entirely past the right edge.
	if _, err := CropScreenshot(src, Bounds{X: 200, Y: 50, Width: 50, Height: 20}, 100, 100); err == nil {
		t.Error("expected error for fully off-screen bounds")
	}
}

// TestCropScreenshot_SizeStableAcrossSubPixelShift is the #138 crop half. The
// old code truncated origin and size independently, so the same element at a
// slightly different Y produced a crop one pixel shorter or taller. Two runs of
// one flow then disagreed on dimensions and assertScreenshot rejected the pair
// on size before comparing any pixels. Rounding the rectangle's edges keeps the
// height fixed while the element drifts within a device pixel.
func TestCropScreenshot_SizeStableAcrossSubPixelShift(t *testing.T) {
	// Screenshot at 50% of device pixels, as the devicelab Android agent sends.
	src := encodeGradientPNG(t, 500, 1000)

	const height = 131 // odd height — the case truncation handles worst
	var sizes []string
	for y := 100; y <= 105; y++ {
		out, err := CropScreenshot(src, Bounds{X: 100, Y: y, Width: 200, Height: height}, 1000, 2000)
		if err != nil {
			t.Fatalf("crop at y=%d: %v", y, err)
		}
		w, h := decodePNGSize(t, out)
		sizes = append(sizes, fmt.Sprintf("%dx%d", w, h))
	}

	for _, got := range sizes {
		if got != sizes[0] {
			t.Errorf("crop size drifted as the element moved sub-pixel: %v", sizes)
			break
		}
	}
}

func TestRotateScreenshot(t *testing.T) {
	// A 4×3 gradient: pixel (x,y) carries R=x, G=y, so every rotated pixel
	// names the source pixel it came from.
	src := encodeGradientPNG(t, 4, 3)
	cases := []struct {
		degrees      int
		wantW, wantH int
		// source of output pixel (x,y), on the 4×3 input
		from func(x, y int) (int, int)
	}{
		{90, 3, 4, func(x, y int) (int, int) { return y, 2 - x }},
		{180, 4, 3, func(x, y int) (int, int) { return 3 - x, 2 - y }},
		{270, 3, 4, func(x, y int) (int, int) { return 3 - y, x }},
	}
	for _, c := range cases {
		out, err := RotateScreenshot(src, c.degrees)
		if err != nil {
			t.Fatalf("%d: %v", c.degrees, err)
		}
		img, err := png.Decode(bytes.NewReader(out))
		if err != nil {
			t.Fatalf("%d: decode: %v", c.degrees, err)
		}
		if b := img.Bounds(); b.Dx() != c.wantW || b.Dy() != c.wantH {
			t.Fatalf("%d: got %dx%d, want %dx%d", c.degrees, b.Dx(), b.Dy(), c.wantW, c.wantH)
		}
		for y := 0; y < c.wantH; y++ {
			for x := 0; x < c.wantW; x++ {
				r, g, _, _ := img.At(x, y).RGBA()
				sx, sy := c.from(x, y)
				if int(r>>8) != sx || int(g>>8) != sy {
					t.Errorf("%d: pixel (%d,%d) came from (%d,%d), want (%d,%d)", c.degrees, x, y, r>>8, g>>8, sx, sy)
				}
			}
		}
	}

	// 0 is the input itself.
	if out, err := RotateScreenshot(src, 0); err != nil || !bytes.Equal(out, src) {
		t.Errorf("0: changed the data (err %v)", err)
	}
}

func TestRotateScreenshot_KeepsJPEG(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 8, 4))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	out, err := RotateScreenshot(buf.Bytes(), 90)
	if err != nil {
		t.Fatal(err)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(out))
	if err != nil || format != "jpeg" || cfg.Width != 4 || cfg.Height != 8 {
		t.Errorf("got %s %dx%d, %v; want jpeg 4x8", format, cfg.Width, cfg.Height, err)
	}
}

func TestRotateScreenshot_Rejects(t *testing.T) {
	if _, err := RotateScreenshot(encodeGradientPNG(t, 2, 2), 45); err == nil {
		t.Error("45 degrees: want an error")
	}
	if _, err := RotateScreenshot([]byte("not an image"), 90); err == nil {
		t.Error("undecodable: want an error")
	}
}
