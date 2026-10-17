package core

import (
	"bytes"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"math"
)

// CropScreenshot crops a screenshot to the given element bounds.
//
// Element bounds are in device-pixel screen coordinates. The screenshot may
// be at a different resolution (some drivers downscale — e.g. the devicelab
// Android agent ships screenshots at 50% to keep WebSocket frames small),
// so bounds are scaled proportionally to the actual decoded image
// dimensions before cropping.
//
// Supports PNG and JPEG input; output format matches input. If the bounds
// fall partly off-screen they are clamped to the image rectangle.
//
// Used by takeScreenshot's cropOn selector (Maestro compatibility).
func CropScreenshot(data []byte, bounds Bounds, screenW, screenH int) ([]byte, error) {
	return CropRotatedScreenshot(data, bounds, screenW, screenH, 0)
}

// CropRotatedScreenshot crops like CropScreenshot, then turns the crop
// clockwise by degrees, which must be 0, 90, 180 or 270, before encoding it.
// The crop is turned in memory and encoded once, so a JPEG is compressed only
// the once CropScreenshot would compress it.
//
// Used by drivers whose screenshot is the device's portrait framebuffer, to
// turn a crop of it back to the app's interface orientation.
func CropRotatedScreenshot(data []byte, bounds Bounds, screenW, screenH, degrees int) ([]byte, error) {
	if degrees != 0 && degrees != 90 && degrees != 180 && degrees != 270 {
		return nil, fmt.Errorf("rotate screenshot: unsupported rotation %d", degrees)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("empty screenshot data")
	}
	if bounds.Width <= 0 || bounds.Height <= 0 {
		return nil, fmt.Errorf("cropOn: element has zero or negative bounds")
	}
	if screenW <= 0 || screenH <= 0 {
		return nil, fmt.Errorf("cropOn: screen dimensions required for bounds scaling")
	}

	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode screenshot: %w", err)
	}

	imgRect := img.Bounds()
	imgW, imgH := imgRect.Dx(), imgRect.Dy()

	// Scale element bounds (device pixels) → image pixels, rounding origin and
	// size independently so the crop's dimensions depend only on the element's
	// size — never on where it sits.
	//
	// Truncating (the old behaviour) coupled the two: with a driver that halves
	// the screenshot, an element of height 131 yields 65 at y=100 but 66 at
	// y=101, because both the origin and the size lose their fraction
	// separately. Two runs of one flow then produced crops of different heights
	// and assertScreenshot rejected the pair on size before comparing a single
	// pixel (#138). Deriving the size from the scaled edges has the same flaw —
	// round(115.5)-round(50) and round(116)-round(50.5) differ. Rounding the
	// width and height on their own gives one answer for a given element size,
	// stable across sub-pixel drift.
	sx := float64(imgW) / float64(screenW)
	sy := float64(imgH) / float64(screenH)
	x := int(math.Round(float64(bounds.X) * sx))
	y := int(math.Round(float64(bounds.Y) * sy))
	w := int(math.Round(float64(bounds.Width) * sx))
	h := int(math.Round(float64(bounds.Height) * sy))

	// Clamp to image rectangle so we don't ask for pixels outside the screenshot.
	if x < 0 {
		w += x
		x = 0
	}
	if y < 0 {
		h += y
		y = 0
	}
	if x+w > imgW {
		w = imgW - x
	}
	if y+h > imgH {
		h = imgH - y
	}
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("cropOn: element entirely outside screenshot bounds")
	}

	cropRect := image.Rect(imgRect.Min.X+x, imgRect.Min.Y+y, imgRect.Min.X+x+w, imgRect.Min.Y+y+h)
	subImager, ok := img.(interface {
		SubImage(r image.Rectangle) image.Image
	})
	if !ok {
		return nil, fmt.Errorf("decoded image type %T does not support SubImage", img)
	}
	cropped := subImager.SubImage(cropRect)
	if degrees != 0 {
		cropped = rotateImage(cropped, degrees)
	}

	var out bytes.Buffer
	switch format {
	case "jpeg":
		if err := jpeg.Encode(&out, cropped, &jpeg.Options{Quality: 90}); err != nil {
			return nil, fmt.Errorf("encode cropped jpeg: %w", err)
		}
	default:
		// PNG by default — preserves quality, no recompression artefacts.
		if err := png.Encode(&out, cropped); err != nil {
			return nil, fmt.Errorf("encode cropped png: %w", err)
		}
	}
	return out.Bytes(), nil
}

// rotateImage turns img clockwise by degrees (90, 180 or 270). RGBA and
// NRGBA pixels are copied four bytes at a time from the source's Pix, keeping
// its type; any other image (a decoded JPEG's YCbCr, a paletted PNG) is
// converted to RGBA first.
func rotateImage(img image.Image, degrees int) image.Image {
	in := img.Bounds()
	w, h := in.Dx(), in.Dy()
	var pix []uint8
	var stride int
	nrgba := false
	switch src := img.(type) {
	case *image.RGBA:
		pix, stride = src.Pix[src.PixOffset(in.Min.X, in.Min.Y):], src.Stride
	case *image.NRGBA:
		pix, stride, nrgba = src.Pix[src.PixOffset(in.Min.X, in.Min.Y):], src.Stride, true
	default:
		rgba := image.NewRGBA(image.Rect(0, 0, w, h))
		draw.Draw(rgba, rgba.Rect, img, in.Min, draw.Src)
		pix, stride = rgba.Pix, rgba.Stride
	}

	outW, outH := h, w
	if degrees == 180 {
		outW, outH = w, h
	}
	outStride := 4 * outW
	out := make([]uint8, outStride*outH)
	for y := 0; y < outH; y++ {
		row := out[y*outStride : (y+1)*outStride]
		for x := 0; x < outW; x++ {
			var sx, sy int
			switch degrees {
			case 90:
				sx, sy = y, h-1-x
			case 180:
				sx, sy = w-1-x, h-1-y
			case 270:
				sx, sy = w-1-y, x
			}
			at := sy*stride + 4*sx
			copy(row[4*x:4*x+4], pix[at:at+4])
		}
	}
	rect := image.Rect(0, 0, outW, outH)
	if nrgba {
		return &image.NRGBA{Pix: out, Stride: outStride, Rect: rect}
	}
	return &image.RGBA{Pix: out, Stride: outStride, Rect: rect}
}
