// Package rfbtest is a scripted RFB server for tests: it greets like Apple's
// Screen Sharing, checks a VNC password or an account login with its own
// cryptography, serves a framebuffer it is given, and records every key and
// pointer event a client sends.
package rfbtest

import (
	"bufio"
	"crypto/aes"
	"crypto/des"
	"crypto/md5"
	"crypto/rand"
	"encoding/binary"
	"image"
	"image/color"
	"io"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"
)

// Oakley group 2 (RFC 2409): a 1024-bit safe prime with generator 2. Apple's
// server sends a prime of the same size.
var Prime1024, _ = new(big.Int).SetString("FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD129024E088A67CC74020BBEA63B139B22514A08798E3404DDEF9519B3CD3A431B302B0A6DF25F14374FE1356D6D51C245E485B576625E7EC6F44C42E9A637ED6B0BFF5CB6F406B7EDEE386BFB5A899FA5AE9F24117C4B1FE649286651ECE65381FFFFFFFFFFFFFFFF", 16)

// Prime2048 is RFC 3526 group 14.
var Prime2048, _ = new(big.Int).SetString("FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD129024E088A67CC74020BBEA63B139B22514A08798E3404DDEF9519B3CD3A431B302B0A6DF25F14374FE1356D6D51C245E485B576625E7EC6F44C42E9A637ED6B0BFF5CB6F406B7EDEE386BFB5A899FA5AE9F24117C4B1FE649286651ECE45B3DC2007CB8A163BF0598DA48361C55D39A69163FA8FD24CF5F83655D23DCA3AD961C62F356208552BB9ED529077096966D670C354E4ABC9804F1746C08CA18217C32905E462E36CE3BE39E772C180E86039B2783A2EC07A28FB5C55DF06F4C52C9DE2BCBF6955817183995497CEA956AE515D2261898FA051015728E5A8AACAA68FFFFFFFFFFFFFFFF", 16)

// Event is one KeyEvent or PointerEvent the client sent.
type Event struct {
	Key    bool   // true for KeyEvent, false for PointerEvent
	Down   bool   // KeyEvent: pressed
	Keysym uint32 // KeyEvent
	Mask   uint8  // PointerEvent: buttons
	X, Y   int    // PointerEvent
}

// Config scripts the server. Zero values give an Apple-like server on a
// 64x48 screen offering types 30, 33, 36, 2 and 35.
type Config struct {
	Types       []byte
	VNCPassword string
	User        string
	Password    string
	FailReason  string // sent with a failed SecurityResult; default "Authentication failed"
	Prime       *big.Int
	Width       int
	Height      int
	Name        string
	// Frame paints the screen; nil gives four quadrants red, green, blue
	// and white.
	Frame func(x, y int) color.RGBA
	// Split cuts each update into this many horizontal strips (default 1).
	Split int
	// OneByte writes every byte separately, to test the client's buffering.
	OneByte bool
	// BeforeUpdate runs before each FramebufferUpdate is sent; it may write
	// extra server messages (Bell, ServerCutText, a bad type).
	BeforeUpdate func(w io.Writer)
	// Hold makes the server receive update requests without answering them.
	Hold bool
}

// Server is a running fake.
type Server struct {
	t   testing.TB
	cfg Config
	ln  net.Listener

	mu          sync.Mutex
	events      []Event
	shared      []byte
	pixelFormat []byte
	encodings   []int32
	requests    []image.Rectangle
	logins      int
	conns       []net.Conn
	frame       func(x, y int) color.RGBA
}

// Start listens on a loopback port and serves until the test ends.
func Start(t testing.TB, cfg Config) *Server {
	t.Helper()
	if cfg.Types == nil {
		cfg.Types = []byte{30, 33, 36, 2, 35}
	}
	if cfg.Width == 0 {
		cfg.Width, cfg.Height = 64, 48
	}
	if cfg.Prime == nil {
		cfg.Prime = Prime1024
	}
	if cfg.Name == "" {
		cfg.Name = "kefir"
	}
	if cfg.Split < 1 {
		cfg.Split = 1
	}
	if cfg.FailReason == "" {
		cfg.FailReason = "Authentication failed"
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{t: t, cfg: cfg, ln: ln, frame: cfg.Frame}
	if s.frame == nil {
		s.frame = Quadrants(cfg.Width, cfg.Height)
	}
	go s.accept()
	t.Cleanup(s.Close)
	return s
}

// Quadrants paints red top-left, green top-right, blue bottom-left, white
// bottom-right.
func Quadrants(w, h int) func(x, y int) color.RGBA {
	return func(x, y int) color.RGBA {
		switch {
		case x < w/2 && y < h/2:
			return color.RGBA{255, 0, 0, 255}
		case y < h/2:
			return color.RGBA{0, 255, 0, 255}
		case x < w/2:
			return color.RGBA{0, 0, 255, 255}
		default:
			return color.RGBA{255, 255, 255, 255}
		}
	}
}

// Addr is the host:port to dial.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Close stops listening and drops every connection.
func (s *Server) Close() {
	_ = s.ln.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		_ = c.Close()
	}
}

// DropClients closes the open connections, as a server that went away.
func (s *Server) DropClients() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.conns {
		_ = c.Close()
	}
	s.conns = nil
}

// SetFrame repaints the screen for later updates.
func (s *Server) SetFrame(f func(x, y int) color.RGBA) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frame = f
}

// Events returns what the client sent so far.
func (s *Server) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event(nil), s.events...)
}

// ResetEvents forgets the events recorded so far.
func (s *Server) ResetEvents() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = nil
}

// WaitEvents waits until at least n events arrived.
func (s *Server) WaitEvents(n int) []Event {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ev := s.Events(); len(ev) >= n {
			return ev
		}
		time.Sleep(5 * time.Millisecond)
	}
	return s.Events()
}

// Shared is the ClientInit byte of each login.
func (s *Server) Shared() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.shared...)
}

// PixelFormat is the last SetPixelFormat body (20 bytes with its header).
func (s *Server) PixelFormat() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.pixelFormat...)
}

// Encodings is the last SetEncodings list.
func (s *Server) Encodings() []int32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int32(nil), s.encodings...)
}

// Requests lists every FramebufferUpdateRequest rectangle.
func (s *Server) Requests() []image.Rectangle {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]image.Rectangle(nil), s.requests...)
}

// Logins counts successful logins.
func (s *Server) Logins() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.logins
}

func (s *Server) accept() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns = append(s.conns, c)
		s.mu.Unlock()
		go s.serve(c)
	}
}

type oneByteWriter struct{ w io.Writer }

func (o oneByteWriter) Write(p []byte) (int, error) {
	for i := range p {
		if _, err := o.w.Write(p[i : i+1]); err != nil {
			return i, err
		}
	}
	return len(p), nil
}

func (s *Server) serve(c net.Conn) {
	defer c.Close()
	var w io.Writer = c
	if s.cfg.OneByte {
		w = oneByteWriter{c}
	}
	r := bufio.NewReader(c)
	if _, err := w.Write([]byte("RFB 003.889\n")); err != nil {
		return
	}
	var ver [12]byte
	if _, err := io.ReadFull(r, ver[:]); err != nil {
		return
	}
	if _, err := w.Write(append([]byte{byte(len(s.cfg.Types))}, s.cfg.Types...)); err != nil {
		return
	}
	t, err := r.ReadByte()
	if err != nil {
		return
	}
	var ok bool
	switch t {
	case 2:
		ok = s.checkVNC(w, r)
	case 30:
		ok = s.checkAccount(w, r)
	default:
		return
	}
	if !ok {
		msg := []byte(s.cfg.FailReason)
		b := make([]byte, 8)
		binary.BigEndian.PutUint32(b, 1)
		binary.BigEndian.PutUint32(b[4:], uint32(len(msg)))
		_, _ = w.Write(append(b, msg...))
		return
	}
	_, _ = w.Write([]byte{0, 0, 0, 0})
	shared, err := r.ReadByte()
	if err != nil {
		return
	}
	s.mu.Lock()
	s.shared = append(s.shared, shared)
	s.logins++
	s.mu.Unlock()
	si := make([]byte, 24)
	binary.BigEndian.PutUint16(si[0:], uint16(s.cfg.Width))
	binary.BigEndian.PutUint16(si[2:], uint16(s.cfg.Height))
	si[4], si[5], si[7] = 32, 24, 1
	binary.BigEndian.PutUint32(si[20:], uint32(len(s.cfg.Name)))
	if _, err := w.Write(append(si, s.cfg.Name...)); err != nil {
		return
	}
	s.messages(w, r)
}

func (s *Server) checkVNC(w io.Writer, r *bufio.Reader) bool {
	challenge := make([]byte, 16)
	_, _ = rand.Read(challenge)
	if _, err := w.Write(challenge); err != nil {
		return false
	}
	resp := make([]byte, 16)
	if _, err := io.ReadFull(r, resp); err != nil {
		return false
	}
	key := make([]byte, 8)
	copy(key, s.cfg.VNCPassword)
	for i, b := range key {
		var rev byte
		for j := 0; j < 8; j++ {
			if b&(1<<j) != 0 {
				rev |= 1 << (7 - j)
			}
		}
		key[i] = rev
	}
	block, _ := des.NewCipher(key)
	want := make([]byte, 16)
	block.Encrypt(want[:8], challenge[:8])
	block.Encrypt(want[8:], challenge[8:])
	return string(want) == string(resp)
}

func (s *Server) checkAccount(w io.Writer, r *bufio.Reader) bool {
	p := s.cfg.Prime
	keyLen := (p.BitLen() + 7) / 8
	priv, _ := rand.Int(rand.Reader, p)
	pub := new(big.Int).Exp(big.NewInt(2), priv, p)
	hdr := make([]byte, 4)
	binary.BigEndian.PutUint16(hdr, 2)
	binary.BigEndian.PutUint16(hdr[2:], uint16(keyLen))
	msg := append(hdr, pad(p.Bytes(), keyLen)...)
	msg = append(msg, pad(pub.Bytes(), keyLen)...)
	if _, err := w.Write(msg); err != nil {
		return false
	}
	body := make([]byte, 128+keyLen)
	if _, err := io.ReadFull(r, body); err != nil {
		return false
	}
	clientPub := new(big.Int).SetBytes(body[128:])
	secret := new(big.Int).Exp(clientPub, priv, p)
	key := md5.Sum(pad(secret.Bytes(), keyLen))
	block, _ := aes.NewCipher(key[:])
	plain := make([]byte, 128)
	for i := 0; i < 128; i += 16 {
		block.Decrypt(plain[i:i+16], body[i:i+16])
	}
	return cstring(plain[:64]) == s.cfg.User && cstring(plain[64:]) == s.cfg.Password
}

func cstring(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

func pad(b []byte, n int) []byte {
	out := make([]byte, n)
	copy(out[n-len(b):], b)
	return out
}

func (s *Server) messages(w io.Writer, r *bufio.Reader) {
	for {
		t, err := r.ReadByte()
		if err != nil {
			return
		}
		switch t {
		case 0: // SetPixelFormat
			b := make([]byte, 19)
			if _, err := io.ReadFull(r, b); err != nil {
				return
			}
			s.mu.Lock()
			s.pixelFormat = append([]byte{0}, b...)
			s.mu.Unlock()
		case 2: // SetEncodings
			b := make([]byte, 3)
			if _, err := io.ReadFull(r, b); err != nil {
				return
			}
			n := int(binary.BigEndian.Uint16(b[1:]))
			encs := make([]int32, n)
			for i := range encs {
				e := make([]byte, 4)
				if _, err := io.ReadFull(r, e); err != nil {
					return
				}
				encs[i] = int32(binary.BigEndian.Uint32(e))
			}
			s.mu.Lock()
			s.encodings = encs
			s.mu.Unlock()
		case 3: // FramebufferUpdateRequest
			b := make([]byte, 9)
			if _, err := io.ReadFull(r, b); err != nil {
				return
			}
			x, y := int(binary.BigEndian.Uint16(b[1:])), int(binary.BigEndian.Uint16(b[3:]))
			rw, rh := int(binary.BigEndian.Uint16(b[5:])), int(binary.BigEndian.Uint16(b[7:]))
			rect := image.Rect(x, y, x+rw, y+rh)
			s.mu.Lock()
			s.requests = append(s.requests, rect)
			s.mu.Unlock()
			if s.cfg.Hold {
				continue
			}
			if s.cfg.BeforeUpdate != nil {
				s.cfg.BeforeUpdate(w)
			}
			if _, err := w.Write(s.update(rect)); err != nil {
				return
			}
		case 4: // KeyEvent
			b := make([]byte, 7)
			if _, err := io.ReadFull(r, b); err != nil {
				return
			}
			s.record(Event{Key: true, Down: b[0] == 1, Keysym: binary.BigEndian.Uint32(b[3:])})
		case 5: // PointerEvent
			b := make([]byte, 5)
			if _, err := io.ReadFull(r, b); err != nil {
				return
			}
			s.record(Event{Mask: b[0], X: int(binary.BigEndian.Uint16(b[1:])), Y: int(binary.BigEndian.Uint16(b[3:]))})
		case 6: // ClientCutText
			b := make([]byte, 7)
			if _, err := io.ReadFull(r, b); err != nil {
				return
			}
			if _, err := r.Discard(int(binary.BigEndian.Uint32(b[3:]))); err != nil {
				return
			}
		default:
			return
		}
	}
}

func (s *Server) record(e Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}

// update is one FramebufferUpdate holding rect as cfg.Split Raw strips.
func (s *Server) update(rect image.Rectangle) []byte {
	s.mu.Lock()
	frame := s.frame
	s.mu.Unlock()
	strips := s.cfg.Split
	if strips > rect.Dy() {
		strips = rect.Dy()
	}
	out := []byte{0, 0, 0, 0}
	binary.BigEndian.PutUint16(out[2:], uint16(strips))
	step := rect.Dy() / strips
	for i := 0; i < strips; i++ {
		y0 := rect.Min.Y + i*step
		y1 := y0 + step
		if i == strips-1 {
			y1 = rect.Max.Y
		}
		out = append(out, RawRect(image.Rect(rect.Min.X, y0, rect.Max.X, y1), frame)...)
	}
	return out
}

// RawRect is one Raw rectangle header and its B, G, R, X pixels.
func RawRect(rect image.Rectangle, frame func(x, y int) color.RGBA) []byte {
	b := make([]byte, 12, 12+rect.Dx()*rect.Dy()*4)
	binary.BigEndian.PutUint16(b[0:], uint16(rect.Min.X))
	binary.BigEndian.PutUint16(b[2:], uint16(rect.Min.Y))
	binary.BigEndian.PutUint16(b[4:], uint16(rect.Dx()))
	binary.BigEndian.PutUint16(b[6:], uint16(rect.Dy()))
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			c := frame(x, y)
			b = append(b, c.B, c.G, c.R, 0)
		}
	}
	return b
}

// DesktopSize is a FramebufferUpdate with one DesktopSize rectangle.
func DesktopSize(w, h int) []byte {
	b := []byte{0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0}
	binary.BigEndian.PutUint16(b[8:], uint16(w))
	binary.BigEndian.PutUint16(b[10:], uint16(h))
	enc := int32(-223)
	return append(b, byte(uint32(enc)>>24), byte(uint32(enc)>>16), byte(uint32(enc)>>8), byte(uint32(enc)))
}
