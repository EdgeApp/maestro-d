package rfbtest_test

import (
	"context"
	"crypto/des"
	"image"
	"image/color"
	"io"
	"net"
	"testing"
	"time"

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

// rawClient dials the fake and writes a byte script, so the server's own
// error paths (a client that hangs up mid-handshake, a bad message) run.
func rawClient(t *testing.T, s *rfbtest.Server, script []byte, readFirst int) {
	t.Helper()
	c, err := net.Dial("tcp", s.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	if readFirst > 0 {
		buf := make([]byte, readFirst)
		_, _ = io.ReadFull(c, buf)
	}
	_, _ = c.Write(script)
	_ = c.(*net.TCPConn).CloseWrite()
	_, _ = io.Copy(io.Discard, c) // until the server hangs up
}

func TestServer_HangUpsAndBadMessages(t *testing.T) {
	s := rfbtest.Start(t, rfbtest.Config{VNCPassword: "v", Types: []byte{2, 30}})
	greeting := len("RFB 003.889\n")
	rawClient(t, s, nil, 0)                                                             // hangs up at the greeting
	rawClient(t, s, []byte("RFB 003.008\n"), 0)                                         // then before choosing a type
	rawClient(t, s, []byte("RFB 003.008\n\x07"), greeting)                              // an unknown type
	rawClient(t, s, []byte("RFB 003.008\n\x02"), greeting)                              // no VNC response
	rawClient(t, s, []byte("RFB 003.008\n\x1e"), greeting)                              // no account response
	rawClient(t, s, append([]byte("RFB 003.008\n\x02"), make([]byte, 16)...), greeting) // a wrong VNC response
	if s.Logins() != 0 {
		t.Errorf("logins %d", s.Logins())
	}
}

// vncResponse is the client's answer to a challenge, written here
// independently of package rfb.
func vncResponse(password string, challenge []byte) []byte {
	key := make([]byte, 8)
	copy(key, password)
	for i, b := range key {
		var r byte
		for j := 0; j < 8; j++ {
			r = r<<1 | b&1
			b >>= 1
		}
		key[i] = r
	}
	c, _ := des.NewCipher(key)
	out := make([]byte, 16)
	c.Encrypt(out[:8], challenge[:8])
	c.Encrypt(out[8:], challenge[8:])
	return out
}

// session logs in over a raw connection, sends tail, half-closes and reads
// until the server hangs up.
func session(t *testing.T, s *rfbtest.Server, tail []byte) {
	t.Helper()
	c, err := net.Dial("tcp", s.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	greeting := make([]byte, 12)
	if _, err := io.ReadFull(c, greeting); err != nil {
		t.Fatal(err)
	}
	_, _ = c.Write([]byte("RFB 003.008\n"))
	types := make([]byte, 1+5)
	if _, err := io.ReadFull(c, types); err != nil {
		t.Fatal(err)
	}
	_, _ = c.Write([]byte{2})
	challenge := make([]byte, 16)
	if _, err := io.ReadFull(c, challenge); err != nil {
		t.Fatal(err)
	}
	_, _ = c.Write(vncResponse("v", challenge))
	result := make([]byte, 4)
	_, _ = io.ReadFull(c, result)
	_, _ = c.Write([]byte{1})
	init := make([]byte, 24+5)
	_, _ = io.ReadFull(c, init)
	_, _ = c.Write(tail)
	_ = c.(*net.TCPConn).CloseWrite()
	_, _ = io.Copy(io.Discard, c)
}

func TestServer_ClientMessages(t *testing.T) {
	s := rfbtest.Start(t, rfbtest.Config{VNCPassword: "v"})
	cut := append([]byte{6, 0, 0, 0, 0, 0, 0, 3}, "abc"...)
	session(t, s, append(cut, 9)) // ClientCutText, then an unknown message
	for _, truncated := range [][]byte{{0, 0}, {2, 0}, {2, 0, 0, 1, 0}, {3, 0}, {4, 0}, {5, 0}, {6, 0}, {6, 0, 0, 0, 0, 0, 0, 9}, {}} {
		session(t, s, truncated)
	}
	if s.Logins() != 10 {
		t.Errorf("logins %d", s.Logins())
	}
	s.DropClients()
	if ev := s.Events(); len(ev) != 0 {
		t.Errorf("events from truncated messages: %v", ev)
	}
}

func TestServer_UpdateWriteFails(t *testing.T) {
	// A client that asks for an update and hangs up before reading it.
	s := rfbtest.Start(t, rfbtest.Config{VNCPassword: "v", Width: 400, Height: 400, BeforeUpdate: func(io.Writer) {}})
	session(t, s, []byte{3, 0, 0, 0, 0, 0, 1, 144, 1, 144})
	if len(s.Requests()) != 1 {
		t.Errorf("requests %v", s.Requests())
	}
}

func TestServerHelpers(t *testing.T) {
	if m := rfbtest.DesktopSize(3, 4); len(m) != 16 || m[9] != 3 || m[11] != 4 {
		t.Errorf("DesktopSize % x", m)
	}
	q := rfbtest.Quadrants(4, 4)
	if q(3, 3) != (color.RGBA{255, 255, 255, 255}) || q(0, 3) != (color.RGBA{0, 0, 255, 255}) {
		t.Error("Quadrants")
	}
	s := rfbtest.Start(t, rfbtest.Config{})
	s.Close()
	s.Close() // twice is fine
}
