package rfb_test

import (
	"context"
	"errors"
	"image"
	"image/color"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/rfb"
	"github.com/devicelab-dev/maestro-runner/pkg/rfb/rfbtest"
)

func account(user, pass string) rfb.Login {
	return rfb.Login{Method: rfb.MethodAccount, User: user, Password: pass}
}

func dial(t *testing.T, s *rfbtest.Server, login rfb.Login) *rfb.Client {
	t.Helper()
	c, err := rfb.Dial(context.Background(), s.Addr(), login)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestDial_AccountLogin(t *testing.T) {
	for _, prime := range []struct {
		name string
		cfg  rfbtest.Config
	}{
		{"1024-bit", rfbtest.Config{User: "paul", Password: "pw"}},
		{"2048-bit", rfbtest.Config{User: "paul", Password: "pw", Prime: rfbtest.Prime2048}},
	} {
		t.Run(prime.name, func(t *testing.T) {
			s := rfbtest.Start(t, prime.cfg)
			c := dial(t, s, account("paul", "pw"))
			if c.Method() != rfb.MethodAccount {
				t.Errorf("method %q", c.Method())
			}
			if w, h := c.Size(); w != 64 || h != 48 {
				t.Errorf("size %dx%d", w, h)
			}
			if c.Name() != "kefir" {
				t.Errorf("name %q", c.Name())
			}
		})
	}
}

func TestDial_VNCLogin(t *testing.T) {
	s := rfbtest.Start(t, rfbtest.Config{VNCPassword: "secret-long-password"})
	c := dial(t, s, rfb.Login{Method: rfb.MethodVNC, VNCPassword: "secret-long-password"})
	if c.Method() != rfb.MethodVNC {
		t.Errorf("method %q", c.Method())
	}
	// Only the first 8 bytes count, as in every VNC server.
	s2 := rfbtest.Start(t, rfbtest.Config{VNCPassword: "secret-l"})
	dial(t, s2, rfb.Login{Method: rfb.MethodVNC, VNCPassword: "secret-long-password"})
	// A short password is NUL-padded.
	s3 := rfbtest.Start(t, rfbtest.Config{VNCPassword: "ab"})
	dial(t, s3, rfb.Login{Method: rfb.MethodVNC, VNCPassword: "ab"})
}

func TestDial_AutoPicksAccountThenVNC(t *testing.T) {
	s := rfbtest.Start(t, rfbtest.Config{User: "paul", Password: "pw", VNCPassword: "v"})
	login := rfb.Login{Method: rfb.MethodAuto, User: "paul", Password: "pw", VNCPassword: "v"}
	if c := dial(t, s, login); c.Method() != rfb.MethodAccount {
		t.Errorf("with type 30 offered: %q", c.Method())
	}
	onlyVNC := rfbtest.Start(t, rfbtest.Config{Types: []byte{2}, VNCPassword: "v"})
	if c := dial(t, onlyVNC, login); c.Method() != rfb.MethodVNC {
		t.Errorf("with only type 2: %q", c.Method())
	}
	// Without an account configured, auto uses the VNC password.
	if c := dial(t, s, rfb.Login{Method: rfb.MethodAuto, VNCPassword: "v"}); c.Method() != rfb.MethodVNC {
		t.Errorf("no account: %q", c.Method())
	}
}

func TestDial_AuthUnavailable(t *testing.T) {
	s := rfbtest.Start(t, rfbtest.Config{Types: []byte{33, 36, 35}})
	for _, m := range []rfb.Method{rfb.MethodAuto, rfb.MethodAccount, rfb.MethodVNC} {
		_, err := rfb.Dial(context.Background(), s.Addr(), rfb.Login{Method: m, User: "u", Password: "p", VNCPassword: "v"})
		var ua *rfb.AuthUnavailable
		if !errors.As(err, &ua) {
			t.Fatalf("%s: got %v", m, err)
		}
		if string(ua.Offered) != string([]byte{33, 36, 35}) {
			t.Errorf("%s: offered %v", m, ua.Offered)
		}
		if !strings.Contains(err.Error(), "[33 36 35]") {
			t.Errorf("message %q", err)
		}
	}
}

func TestDial_AuthFailed(t *testing.T) {
	s := rfbtest.Start(t, rfbtest.Config{User: "paul", Password: "right", VNCPassword: "right", FailReason: "Authentication failure"})
	for _, login := range []rfb.Login{account("paul", "wrong"), {Method: rfb.MethodVNC, VNCPassword: "wrong"}} {
		_, err := rfb.Dial(context.Background(), s.Addr(), login)
		var af *rfb.AuthFailed
		if !errors.As(err, &af) {
			t.Fatalf("%s: got %v", login.Method, err)
		}
		if af.Reason != "Authentication failure" || af.Method != login.Method {
			t.Errorf("%+v", af)
		}
		if strings.Contains(err.Error(), "wrong") {
			t.Errorf("the error carries the password: %q", err)
		}
	}
}

func TestDial_LongCredentialsRefused(t *testing.T) {
	s := rfbtest.Start(t, rfbtest.Config{})
	long := strings.Repeat("x", 64)
	for _, login := range []rfb.Login{account(long, "p"), account("u", long)} {
		if _, err := rfb.Dial(context.Background(), s.Addr(), login); err == nil || !strings.Contains(err.Error(), "63 bytes") {
			t.Errorf("got %v", err)
		}
	}
	// 63 bytes is fine.
	ok := strings.Repeat("y", 63)
	s2 := rfbtest.Start(t, rfbtest.Config{User: ok, Password: ok})
	dial(t, s2, account(ok, ok))
}

func TestDial_InitMessages(t *testing.T) {
	s := rfbtest.Start(t, rfbtest.Config{User: "u", Password: "p"})
	c := dial(t, s, account("u", "p"))
	// A capture round-trip guarantees the server has read everything sent
	// before it.
	if _, err := c.Capture(context.Background(), image.Rect(0, 0, 1, 1)); err != nil {
		t.Fatal(err)
	}
	if got := s.Shared(); len(got) != 1 || got[0] != 1 {
		t.Errorf("ClientInit shared flag %v", got)
	}
	want := []byte{0, 0, 0, 0, 32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 16, 8, 0, 0, 0, 0}
	if got := s.PixelFormat(); string(got) != string(want) {
		t.Errorf("SetPixelFormat % x", got)
	}
	if got := s.Encodings(); len(got) != 2 || got[0] != 0 || got[1] != -223 {
		t.Errorf("SetEncodings %v", got)
	}
}

func TestCapture_FullAndRect(t *testing.T) {
	s := rfbtest.Start(t, rfbtest.Config{User: "u", Password: "p", Split: 7})
	c := dial(t, s, account("u", "p"))
	img, err := c.Capture(context.Background(), image.Rect(0, 0, 64, 48))
	if err != nil {
		t.Fatal(err)
	}
	checks := map[image.Point]color.RGBA{
		{1, 1}: {255, 0, 0, 255}, {62, 1}: {0, 255, 0, 255},
		{1, 46}: {0, 0, 255, 255}, {62, 46}: {255, 255, 255, 255},
	}
	for p, want := range checks {
		if got := img.RGBAAt(p.X, p.Y); got != want {
			t.Errorf("full %v: %v want %v", p, got, want)
		}
	}
	sub, err := c.Capture(context.Background(), image.Rect(40, 30, 50, 40))
	if err != nil {
		t.Fatal(err)
	}
	if b := sub.Bounds(); b.Dx() != 10 || b.Dy() != 10 || b.Min != (image.Point{}) {
		t.Errorf("rect bounds %v", b)
	}
	if got := sub.RGBAAt(5, 5); got != (color.RGBA{255, 255, 255, 255}) {
		t.Errorf("rect pixel %v", got)
	}
	reqs := s.Requests()
	if last := reqs[len(reqs)-1]; last != image.Rect(40, 30, 50, 40) {
		t.Errorf("last request %v", last)
	}
}

func TestCapture_OutsideScreen(t *testing.T) {
	s := rfbtest.Start(t, rfbtest.Config{User: "u", Password: "p"})
	c := dial(t, s, account("u", "p"))
	for _, r := range []image.Rectangle{image.Rect(60, 0, 70, 10), {}, image.Rect(-1, 0, 5, 5)} {
		if _, err := c.Capture(context.Background(), r); err == nil || !strings.Contains(err.Error(), "outside the 64x48 screen") {
			t.Errorf("%v: %v", r, err)
		}
	}
}

func TestCapture_OneByteWrites(t *testing.T) {
	s := rfbtest.Start(t, rfbtest.Config{User: "u", Password: "p", OneByte: true, Width: 8, Height: 6})
	c := dial(t, s, account("u", "p"))
	img, err := c.Capture(context.Background(), image.Rect(0, 0, 8, 6))
	if err != nil {
		t.Fatal(err)
	}
	if got := img.RGBAAt(7, 5); got != (color.RGBA{255, 255, 255, 255}) {
		t.Errorf("pixel %v", got)
	}
}

func TestCapture_SkipsBellAndCutText(t *testing.T) {
	s := rfbtest.Start(t, rfbtest.Config{User: "u", Password: "p", BeforeUpdate: func(w io.Writer) {
		_, _ = w.Write([]byte{2})
		cut := []byte{3, 0, 0, 0, 0, 0, 0, 5}
		_, _ = w.Write(append(cut, "hello"...))
	}})
	c := dial(t, s, account("u", "p"))
	if _, err := c.Capture(context.Background(), image.Rect(0, 0, 64, 48)); err != nil {
		t.Fatal(err)
	}
}

func TestCapture_ProtocolErrors(t *testing.T) {
	for name, msg := range map[string][]byte{
		"colour map":   {1, 0, 0, 0, 0, 1},
		"unknown type": {99},
		"zrle rect":    {0, 0, 0, 1, 0, 0, 0, 0, 0, 1, 0, 1, 0, 0, 0, 16},
		"outside rect": append([]byte{0, 0, 0, 1}, rfbtest.RawRect(image.Rect(60, 40, 70, 50), func(int, int) color.RGBA { return color.RGBA{} })...),
	} {
		t.Run(name, func(t *testing.T) {
			s := rfbtest.Start(t, rfbtest.Config{User: "u", Password: "p", BeforeUpdate: func(w io.Writer) { _, _ = w.Write(msg) }})
			c := dial(t, s, account("u", "p"))
			_, err := c.Capture(context.Background(), image.Rect(0, 0, 64, 48))
			if !errors.Is(err, rfb.ErrProtocol) {
				t.Fatalf("capture: %v", err)
			}
			<-c.Done()
			if !errors.Is(c.Err(), rfb.ErrProtocol) {
				t.Errorf("Err %v", c.Err())
			}
			if _, err := c.Capture(context.Background(), image.Rect(0, 0, 1, 1)); !errors.Is(err, rfb.ErrProtocol) {
				t.Errorf("capture after close: %v", err)
			}
		})
	}
}

func TestDesktopSize(t *testing.T) {
	var once sync.Once
	s := rfbtest.Start(t, rfbtest.Config{User: "u", Password: "p", BeforeUpdate: func(w io.Writer) {
		once.Do(func() { _, _ = w.Write(rfbtest.DesktopSize(32, 24)) })
	}})
	c := dial(t, s, account("u", "p"))
	if _, err := c.Capture(context.Background(), image.Rect(0, 0, 64, 48)); !errors.Is(err, rfb.ErrResized) {
		t.Fatalf("capture across a resize: %v", err)
	}
	if w, h := c.Size(); w != 32 || h != 24 {
		t.Errorf("size %dx%d", w, h)
	}
}

func TestCapture_Timeout(t *testing.T) {
	restore := rfb.SetCaptureTimeoutForTest(50 * time.Millisecond)
	defer restore()
	s := rfbtest.Start(t, rfbtest.Config{User: "u", Password: "p", Hold: true})
	c := dial(t, s, account("u", "p"))
	if _, err := c.Capture(context.Background(), image.Rect(0, 0, 4, 4)); !errors.Is(err, rfb.ErrCaptureTimeout) {
		t.Errorf("got %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Capture(ctx, image.Rect(0, 0, 4, 4)); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}

func TestServerClose(t *testing.T) {
	s := rfbtest.Start(t, rfbtest.Config{User: "u", Password: "p", Hold: true})
	c := dial(t, s, account("u", "p"))
	errc := make(chan error, 1)
	go func() {
		_, err := c.Capture(context.Background(), image.Rect(0, 0, 4, 4))
		errc <- err
	}()
	for len(s.Requests()) == 0 {
		time.Sleep(time.Millisecond)
	}
	s.DropClients()
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Done not closed")
	}
	if !errors.Is(c.Err(), rfb.ErrClosed) {
		t.Errorf("Err %v", c.Err())
	}
	if err := <-errc; !errors.Is(err, rfb.ErrClosed) {
		t.Errorf("pending capture %v", err)
	}
	if err := c.Key(true, 'a'); !errors.Is(err, rfb.ErrClosed) {
		t.Errorf("write after close %v", err)
	}
}

func TestKeyAndPointerBytes(t *testing.T) {
	s := rfbtest.Start(t, rfbtest.Config{User: "u", Password: "p"})
	c := dial(t, s, account("u", "p"))
	_ = c.Key(true, rfb.KeyMeta)
	_ = c.Key(false, 'k')
	_ = c.Pointer(4, 70000, -5)
	ev := s.WaitEvents(3)
	want := []rfbtest.Event{
		{Key: true, Down: true, Keysym: 0xffeb},
		{Key: true, Down: false, Keysym: 'k'},
		{Mask: 4, X: 0xffff, Y: 0},
	}
	for i, w := range want {
		if ev[i] != w {
			t.Errorf("event %d: %+v want %+v", i, ev[i], w)
		}
	}
}

func TestConcurrentWritesDoNotInterleave(t *testing.T) {
	s := rfbtest.Start(t, rfbtest.Config{User: "u", Password: "p"})
	c := dial(t, s, account("u", "p"))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if i%2 == 0 {
					_ = c.Key(j%2 == 0, uint32('a'+i))
				} else {
					_ = c.Pointer(uint8(i), i, j)
				}
			}
		}(i)
	}
	wg.Wait()
	ev := s.WaitEvents(400)
	if len(ev) != 400 {
		t.Fatalf("got %d events", len(ev))
	}
	for _, e := range ev {
		if e.Key && (e.Keysym < 'a' || e.Keysym > 'h') {
			t.Fatalf("garbled key event %+v", e)
		}
		if !e.Key && (e.Mask%2 != 1 || e.X != int(e.Mask)) {
			t.Fatalf("garbled pointer event %+v", e)
		}
	}
}

func TestProbe(t *testing.T) {
	s := rfbtest.Start(t, rfbtest.Config{Types: []byte{30, 2}})
	v, types, err := rfb.Probe(context.Background(), s.Addr(), time.Second)
	if err != nil || v != "RFB 003.889" || string(types) != string([]byte{30, 2}) {
		t.Errorf("%q %v %v", v, types, err)
	}
	if s.Logins() != 0 {
		t.Error("Probe logged in")
	}
	if _, _, err := rfb.Probe(context.Background(), "127.0.0.1:1", 200*time.Millisecond); err == nil {
		t.Error("probe of a closed port succeeded")
	}
}

func TestDial_ConnectionRefused(t *testing.T) {
	if _, err := rfb.Dial(context.Background(), "127.0.0.1:1", rfb.Login{}); err == nil {
		t.Error("dial of a closed port succeeded")
	}
}

// rawServer answers each connection with a fixed byte script.
func rawServer(t *testing.T, script []byte) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = c.Write(script)
				buf := make([]byte, 64)
				for {
					if _, err := c.Read(buf); err != nil {
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().String()
}

func TestGreetErrors(t *testing.T) {
	refused := append([]byte("RFB 003.889\n\x00\x00\x00\x00\x04"), "busy"...)
	for name, tc := range map[string]struct {
		script []byte
		want   string
	}{
		"not rfb":      {[]byte("SSH-2.0-OpenSSH\n"), "protocol error"},
		"refused":      {refused, "refused the connection: busy"},
		"short":        {[]byte("RFB 0"), "read greeting"},
		"no types":     {[]byte("RFB 003.889\n\x02\x1e"), "i/o timeout"},
		"bad key len":  {[]byte("RFB 003.889\n\x01\x1e\x00\x02\x00\x00"), "key length 0"},
		"no keys":      {[]byte("RFB 003.889\n\x01\x1e\x00\x02\x00\x80"), "read account login keys"},
		"no challenge": {[]byte("RFB 003.889\n\x01\x02"), "read VNC challenge"},
	} {
		t.Run(name, func(t *testing.T) {
			addr := rawServer(t, tc.script)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			restore := rfb.SetHandshakeTimeoutForTest(300 * time.Millisecond)
			defer restore()
			_, err := rfb.Dial(ctx, addr, rfb.Login{User: "u", Password: "p", VNCPassword: "v"})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want %q", err, tc.want)
			}
		})
	}
}

func TestDial_NoLoginResult(t *testing.T) {
	// The server takes the VNC response and hangs up before SecurityResult.
	addr := rawServer(t, append([]byte("RFB 003.889\n\x01\x02"), make([]byte, 16)...))
	restore := rfb.SetHandshakeTimeoutForTest(300 * time.Millisecond)
	defer restore()
	if _, err := rfb.Dial(context.Background(), addr, rfb.Login{Method: rfb.MethodVNC, VNCPassword: "v"}); err == nil || !strings.Contains(err.Error(), "read login result") {
		t.Errorf("got %v", err)
	}
}

func TestErrorStrings(t *testing.T) {
	if got := (&rfb.AuthFailed{Method: rfb.MethodVNC}).Error(); got != "screen sharing rejected the vnc login" {
		t.Errorf("%q", got)
	}
}
