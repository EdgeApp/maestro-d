package daemon

// maestro-d: screenshot options for every driver — the image type (jpeg or
// png), JPEG quality and a rectangle. A driver that captures pixels
// (core.FrameGrabber, the computer device) is asked for just the rectangle
// and its frame is encoded once; any other driver's PNG is decoded, cropped
// and re-encoded, and comes back untouched when no option is given.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"net/url"
	"strconv"
	"strings"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
)

// shotOptions is a screenshot request's query.
type shotOptions struct {
	image   string // "", "png" or "jpeg"
	quality int    // 1–100; 0 = default
	rect    *image.Rectangle
}

const defaultJPEGQuality = 80

func parseShotOptions(q url.Values) (shotOptions, *Error) {
	var o shotOptions
	switch v := strings.ToLower(q.Get("image")); v {
	case "", "png", "jpeg":
		o.image = v
	case "jpg":
		o.image = "jpeg"
	default:
		return o, Errorf(CodeUsage, "image %q: use jpeg or png", v)
	}
	if v := q.Get("quality"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			return o, Errorf(CodeUsage, "quality %q: use 1 to 100", v)
		}
		o.quality = n
	}
	keys := []string{"x", "y", "width", "height"}
	var vals [4]int
	given := 0
	for i, k := range keys {
		v := q.Get(k)
		if v == "" {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return o, Errorf(CodeUsage, "%s %q is not a whole number", k, v)
		}
		vals[i] = n
		given++
	}
	switch given {
	case 0:
	case 4:
		if vals[2] < 1 || vals[3] < 1 {
			return o, Errorf(CodeUsage, "a region's width and height must be at least 1")
		}
		r := image.Rect(vals[0], vals[1], vals[0]+vals[2], vals[1]+vals[3])
		o.rect = &r
	default:
		return o, Errorf(CodeUsage, "a region needs x, y, width and height")
	}
	return o, nil
}

// any reports whether the request asks for anything but the default.
func (o shotOptions) any() bool { return o.image != "" || o.quality != 0 || o.rect != nil }

// shot is a screenshot answer: the bytes, their type, and, when an option
// was used or the driver captures pixels, the screen size and rectangle.
type shot struct {
	data        []byte
	contentType string
	screen      image.Point
	rect        image.Rectangle
	described   bool
}

// shotSource is what takeShot captures from: an attachment's session, whose
// Screenshot and Frame hold its lock and answer once it is closed.
type shotSource interface {
	Driver() core.Driver
	Screenshot(ctx context.Context) ([]byte, error)
	Frame(ctx context.Context, rect image.Rectangle) (*image.RGBA, error)
}

// takeShot captures and encodes one screenshot of src.
func takeShot(ctx context.Context, src shotSource, o shotOptions) (*shot, *Error) {
	d := core.Unwrap(src.Driver())
	kind := o.image
	if kind == "" {
		kind = "png"
		if di, ok := d.(core.DefaultImager); ok && di.DefaultImage() == "jpeg" {
			kind = "jpeg"
		}
	}
	if _, ok := d.(core.FrameGrabber); ok {
		return grabShot(ctx, src, d, kind, o)
	}
	data, err := src.Screenshot(ctx)
	if err != nil {
		return nil, WrapErr(CodeDeviceError, err)
	}
	if !o.any() || (kind == "png" && o.rect == nil) {
		return &shot{data: data, contentType: "image/png"}, nil
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, WrapErr(CodeDeviceError, fmt.Errorf("decode the driver's screenshot: %w", err))
	}
	b := img.Bounds()
	r := b
	if o.rect != nil {
		if e := checkRect(*o.rect, b.Dx(), b.Dy()); e != nil {
			return nil, e
		}
		r = o.rect.Add(b.Min)
		if s, ok := img.(interface {
			SubImage(image.Rectangle) image.Image
		}); ok {
			img = s.SubImage(r)
		}
	}
	out, err := encodeShot(img, kind, o.quality)
	if err != nil {
		return nil, WrapErr(CodeDeviceError, err)
	}
	return &shot{data: out, contentType: "image/" + kind, screen: image.Pt(b.Dx(), b.Dy()), rect: r.Sub(b.Min), described: true}, nil
}

// grabShot captures through a FrameGrabber. A capture cut short by a change
// of screen size is taken once more at the new size, with a requested
// region checked against it again.
func grabShot(ctx context.Context, src shotSource, d core.Driver, kind string, o shotOptions) (*shot, *Error) {
	for attempt := 0; ; attempt++ {
		w, h, err := core.ScreenshotSpace(d)
		if err != nil {
			return nil, WrapErr(CodeDeviceError, err)
		}
		r := image.Rect(0, 0, w, h)
		if o.rect != nil {
			if e := checkRect(*o.rect, w, h); e != nil {
				return nil, e
			}
			r = *o.rect
		}
		img, err := src.Frame(ctx, r)
		if errors.Is(err, core.ErrScreenResized) && attempt == 0 {
			continue
		}
		if err != nil {
			return nil, WrapErr(CodeDeviceError, err)
		}
		data, err := encodeShot(img, kind, o.quality)
		if err != nil {
			return nil, WrapErr(CodeDeviceError, err)
		}
		return &shot{data: data, contentType: "image/" + kind, screen: image.Pt(w, h), rect: r, described: true}, nil
	}
}

func checkRect(r image.Rectangle, w, h int) *Error {
	if r.Dx() < 1 || r.Dy() < 1 || !r.In(image.Rect(0, 0, w, h)) {
		return Errorf(CodeUsage, "region %d,%d %dx%d is not inside the %dx%d screen", r.Min.X, r.Min.Y, r.Dx(), r.Dy(), w, h)
	}
	return nil
}

func encodeShot(img image.Image, kind string, quality int) ([]byte, error) {
	var buf bytes.Buffer
	if kind == "jpeg" {
		if quality == 0 {
			quality = defaultJPEGQuality
		}
		err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality})
		return buf.Bytes(), err
	}
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	err := enc.Encode(&buf, img)
	return buf.Bytes(), err
}
