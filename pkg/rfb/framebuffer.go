package rfb

import (
	"encoding/binary"
	"image"
)

const (
	encodingRaw         int32 = 0
	encodingDesktopSize int32 = -223
)

// framebuffer is the screen as the server last sent it, in the pixel format
// setPixelFormatMsg asks for: 32 bits, little-endian, R<<16|G<<8|B, so the
// bytes of each pixel are B, G, R, X.
type framebuffer struct {
	w, h int
	pix  []byte
}

func newFramebuffer(w, h int) *framebuffer {
	return &framebuffer{w: w, h: h, pix: make([]byte, w*h*4)}
}

// blit copies a Raw rectangle in; false when it does not fit the screen.
func (f *framebuffer) blit(rect image.Rectangle, src []byte) bool {
	if !rect.In(image.Rect(0, 0, f.w, f.h)) || len(src) < area(rect)*4 {
		return false
	}
	row := rect.Dx() * 4
	for y := 0; y < rect.Dy(); y++ {
		off := ((rect.Min.Y+y)*f.w + rect.Min.X) * 4
		copy(f.pix[off:off+row], src[y*row:(y+1)*row])
	}
	return true
}

// rgba copies rect out as an opaque RGBA image with its origin at 0,0.
func (f *framebuffer) rgba(rect image.Rectangle) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, rect.Dx(), rect.Dy()))
	for y := 0; y < rect.Dy(); y++ {
		src := f.pix[((rect.Min.Y+y)*f.w+rect.Min.X)*4:]
		dst := img.Pix[y*img.Stride:]
		for x := 0; x < rect.Dx(); x++ {
			s, d := src[x*4:x*4+4], dst[x*4:x*4+4]
			d[0], d[1], d[2], d[3] = s[2], s[1], s[0], 0xff
		}
	}
	return img
}

func setPixelFormatMsg() []byte {
	b := make([]byte, 20)
	b[0] = 0  // SetPixelFormat
	b[4] = 32 // bits per pixel
	b[5] = 24 // depth
	b[6] = 0  // little-endian
	b[7] = 1  // true colour
	binary.BigEndian.PutUint16(b[8:], 255)
	binary.BigEndian.PutUint16(b[10:], 255)
	binary.BigEndian.PutUint16(b[12:], 255)
	b[14], b[15], b[16] = 16, 8, 0 // red, green, blue shifts
	return b
}

func setEncodingsMsg() []byte {
	encs := []int32{encodingRaw, encodingDesktopSize}
	b := make([]byte, 4+4*len(encs))
	b[0] = 2 // SetEncodings
	binary.BigEndian.PutUint16(b[2:], uint16(len(encs)))
	for i, e := range encs {
		binary.BigEndian.PutUint32(b[4+4*i:], uint32(e))
	}
	return b
}

func updateRequestMsg(rect image.Rectangle) []byte {
	b := make([]byte, 10)
	b[0] = 3 // FramebufferUpdateRequest; b[1] = 0: not incremental
	binary.BigEndian.PutUint16(b[2:], uint16(rect.Min.X))
	binary.BigEndian.PutUint16(b[4:], uint16(rect.Min.Y))
	binary.BigEndian.PutUint16(b[6:], uint16(rect.Dx()))
	binary.BigEndian.PutUint16(b[8:], uint16(rect.Dy()))
	return b
}
