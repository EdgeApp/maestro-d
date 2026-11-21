package rfbtest_test

import (
	"context"
	"image"
	"image/color"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/rfb"
	"github.com/devicelab-dev/maestro-runner/pkg/rfb/rfbtest"
)

// The fake is exercised end to end through the real client.
func TestServer(t *testing.T) {
	s := rfbtest.Start(t, rfbtest.Config{User: "u", Password: "p", VNCPassword: "v", Split: 3, OneByte: true, Width: 6, Height: 4})
	for _, login := range []rfb.Login{{Method: rfb.MethodAccount, User: "u", Password: "p"}, {Method: rfb.MethodVNC, VNCPassword: "v"}} {
		c, err := rfb.Dial(context.Background(), s.Addr(), login)
		if err != nil {
			t.Fatal(err)
		}
		img, err := c.Capture(context.Background(), image.Rect(0, 0, 6, 4))
		if err != nil {
			t.Fatal(err)
		}
		if img.RGBAAt(0, 0) != (color.RGBA{255, 0, 0, 255}) || img.RGBAAt(5, 0) != (color.RGBA{0, 255, 0, 255}) || img.RGBAAt(0, 3) != (color.RGBA{0, 0, 255, 255}) {
			t.Errorf("quadrants wrong")
		}
		_ = c.Key(true, 'x')
		_ = c.Pointer(1, 2, 3)
		_ = c.Close()
	}
	if s.Logins() != 2 {
		t.Errorf("logins %d", s.Logins())
	}
	if ev := s.WaitEvents(4); len(ev) != 4 {
		t.Errorf("events %v", ev)
	}
	s.ResetEvents()
	if len(s.Events()) != 0 {
		t.Error("ResetEvents kept events")
	}
	s.SetFrame(func(int, int) color.RGBA { return color.RGBA{1, 2, 3, 255} })
	c, err := rfb.Dial(context.Background(), s.Addr(), rfb.Login{Method: rfb.MethodVNC, VNCPassword: "v"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	img, _ := c.Capture(context.Background(), image.Rect(1, 1, 3, 3))
	if img.RGBAAt(0, 0) != (color.RGBA{1, 2, 3, 255}) {
		t.Errorf("SetFrame ignored: %v", img.RGBAAt(0, 0))
	}
	if len(s.Requests()) == 0 || len(s.Encodings()) != 2 || len(s.PixelFormat()) != 20 || len(s.Shared()) != 3 {
		t.Error("recorders empty")
	}
}

func TestServer_RejectsUnknownType(t *testing.T) {
	s := rfbtest.Start(t, rfbtest.Config{Types: []byte{33}})
	if _, err := rfb.Dial(context.Background(), s.Addr(), rfb.Login{}); err == nil {
		t.Error("dial succeeded")
	}
}
