package aicheck

// image.go: fitting a screenshot to what each model takes.

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"

	// Screenshots are PNG; a driver may hand back a JPEG.
	_ "image/jpeg"
)

// Jev takes images whose sides are each 280–1568 pixels; anything outside
// is rejected, never resized for us.
const (
	JevMinSide = 280
	JevMaxSide = 1568
)

// Claude reads images up to 1568 pixels on the long side and about 1.15
// megapixels; larger ones are downscaled by the API anyway, at the same
// token cost, so they are sent at that size.
const (
	claudeMaxSide   = 1568
	claudeMaxPixels = 1_150_000
)

// decodeImage reads a screenshot.
func decodeImage(data []byte) (image.Image, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("screenshot is not a PNG or JPEG: %w", err)
	}
	if b := img.Bounds(); b.Dx() <= 0 || b.Dy() <= 0 {
		return nil, fmt.Errorf("screenshot is empty")
	}
	return img, nil
}

// fitJev scales img so each side is within JevMinSide–JevMaxSide: down
// until the long side fits, up until the short side does when that keeps
// the long side within bounds, and otherwise pads the short side with
// black to JevMinSide.
func fitJev(img image.Image) image.Image {
	b := img.Bounds()
	w, h := float64(b.Dx()), float64(b.Dy())
	long, short := max(w, h), min(w, h)
	scale := 1.0
	if long > JevMaxSide {
		scale = JevMaxSide / long
	}
	if short*scale < JevMinSide {
		if up := JevMinSide / short; long*up <= JevMaxSide {
			scale = up
		}
	}
	nw, nh := clampSide(w*scale), clampSide(h*scale)
	scaled := resize(img, nw, nh)
	if nw >= JevMinSide && nh >= JevMinSide {
		return scaled
	}
	pw, ph := max(nw, JevMinSide), max(nh, JevMinSide)
	out := image.NewRGBA(image.Rect(0, 0, pw, ph))
	draw.Draw(out, out.Bounds(), &image.Uniform{C: color.Black}, image.Point{}, draw.Src)
	draw.Draw(out, image.Rect(0, 0, nw, nh), scaled, image.Point{}, draw.Src)
	return out
}

// fitClaude scales img down (never up) to Claude's size.
func fitClaude(img image.Image) image.Image {
	b := img.Bounds()
	w, h := float64(b.Dx()), float64(b.Dy())
	scale := 1.0
	if long := max(w, h); long > claudeMaxSide {
		scale = claudeMaxSide / long
	}
	if px := w * h * scale * scale; px > claudeMaxPixels {
		scale *= math.Sqrt(claudeMaxPixels / px)
	}
	if scale == 1 {
		return img
	}
	// Rounded down, so the pixel cap holds.
	return resize(img, max(int(w*scale), 1), max(int(h*scale), 1))
}

func clampSide(v float64) int {
	n := int(v + 0.5)
	if n < 1 {
		return 1
	}
	return n
}

// resize scales img to w×h, averaging the source pixels each target pixel
// covers when shrinking and sampling the nearest when growing.
func resize(img image.Image, w, h int) image.Image {
	b := img.Bounds()
	if b.Dx() == w && b.Dy() == h {
		return img
	}
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	sx, sy := float64(b.Dx())/float64(w), float64(b.Dy())/float64(h)
	for y := 0; y < h; y++ {
		y0 := b.Min.Y + int(float64(y)*sy)
		y1 := max(b.Min.Y+int(float64(y+1)*sy), y0+1)
		for x := 0; x < w; x++ {
			x0 := b.Min.X + int(float64(x)*sx)
			x1 := max(b.Min.X+int(float64(x+1)*sx), x0+1)
			var r, g, bl, a, n uint64
			for yy := y0; yy < y1 && yy < b.Max.Y; yy++ {
				for xx := x0; xx < x1 && xx < b.Max.X; xx++ {
					cr, cg, cb, ca := img.At(xx, yy).RGBA()
					r, g, bl, a, n = r+uint64(cr), g+uint64(cg), bl+uint64(cb), a+uint64(ca), n+1
				}
			}
			out.Set(x, y, color.RGBA64{R: uint16(r / n), G: uint16(g / n), B: uint16(bl / n), A: uint16(a / n)})
		}
	}
	return out
}

func encodePNG(img image.Image) []byte {
	var buf bytes.Buffer
	// Encoding an in-memory RGBA image to a buffer cannot fail.
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}
