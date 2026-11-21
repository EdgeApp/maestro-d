// Package rfb is a small RFB (VNC) client for driving the Mac maestro-d runs
// on through its own Screen Sharing over loopback: log in, read the screen as
// raw pixels, and send key and pointer events.
//
// It speaks RFB 3.8 to Apple's server (which greets with "RFB 003.889") and
// implements only what that needs: Apple's account login (security type 30)
// and VNC password login (type 2), a 32-bit pixel format, the Raw and
// DesktopSize encodings, and KeyEvent / PointerEvent. Over loopback Raw is the
// cheapest encoding; the compressed ones Apple offers third-party clients
// (ZRLE, Zlib) only add CPU, and it offers no lossy or video encoding.
package rfb

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"io"
	"net"
	"sync"
	"time"
)

// Security types.
const (
	SecurityVNC     = 2  // VNC password (DES challenge response)
	SecurityAccount = 30 // Apple's Diffie-Hellman login with a macOS user and password
)

// Method chooses how Dial logs in.
type Method string

const (
	// MethodAuto uses the account login when the server offers it and an
	// account is configured, else the VNC password.
	MethodAuto    Method = "auto"
	MethodAccount Method = "account"
	MethodVNC     Method = "vnc"
)

// Login holds the credentials Dial may use. Nothing in this package logs or
// returns them.
type Login struct {
	Method      Method
	User        string // macOS short user name, for the account login
	Password    string // macOS password, for the account login
	VNCPassword string // Screen Sharing's VNC password
}

// AuthUnavailable is returned when the server does not offer the security
// type the login needs.
type AuthUnavailable struct {
	Wanted  []byte
	Offered []byte
}

func (e *AuthUnavailable) Error() string {
	return fmt.Sprintf("screen sharing does not offer login type %v (offered %v)", e.Wanted, e.Offered)
}

// AuthFailed is returned when the server rejects the credentials. Reason is
// the server's own text.
type AuthFailed struct {
	Method Method
	Reason string
}

func (e *AuthFailed) Error() string {
	if e.Reason == "" {
		return fmt.Sprintf("screen sharing rejected the %s login", e.Method)
	}
	return fmt.Sprintf("screen sharing rejected the %s login: %s", e.Method, e.Reason)
}

var (
	// ErrProtocol means the server sent something this client does not
	// handle; the connection is closed.
	ErrProtocol = errors.New("rfb: protocol error")
	// ErrCaptureTimeout means the requested rectangle did not arrive in time.
	ErrCaptureTimeout = errors.New("rfb: screen capture timed out")
	// ErrResized means the screen changed size while a capture waited.
	ErrResized = errors.New("rfb: screen size changed during capture")
	// ErrClosed means the connection is gone.
	ErrClosed = errors.New("rfb: connection closed")
)

// captureTimeout is how long Capture waits for its rectangle. A variable so
// tests can shorten it.
var captureTimeout = 10 * time.Second

// handshakeTimeout bounds Dial's exchange before the read loop starts.
var handshakeTimeout = 15 * time.Second

// Client is one logged-in connection.
type Client struct {
	conn   net.Conn
	r      *bufio.Reader
	method Method
	name   string

	writeMu sync.Mutex
	rawBuf  []byte // the read loop's own; never touched elsewhere

	mu      sync.Mutex
	fb      *framebuffer
	waiters []*captureWaiter
	err     error
	done    chan struct{}
}

type captureWaiter struct {
	rect    image.Rectangle
	covered int
	ch      chan error
}

// Probe reads the server's greeting and offered security types without
// logging in.
func Probe(ctx context.Context, addr string, timeout time.Duration) (version string, types []byte, err error) {
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	r := bufio.NewReader(conn)
	v, types, err := greet(conn, r)
	return v, types, err
}

// greet reads the version, answers 3.8 and reads the offered security types.
func greet(w io.Writer, r *bufio.Reader) (string, []byte, error) {
	var ver [12]byte
	if _, err := io.ReadFull(r, ver[:]); err != nil {
		return "", nil, fmt.Errorf("read greeting: %w", err)
	}
	if string(ver[:4]) != "RFB " {
		return "", nil, fmt.Errorf("%w: greeting %q", ErrProtocol, ver[:])
	}
	if _, err := w.Write([]byte("RFB 003.008\n")); err != nil {
		return "", nil, err
	}
	n, err := r.ReadByte()
	if err != nil {
		return "", nil, err
	}
	if n == 0 {
		reason, _ := readString(r)
		return "", nil, fmt.Errorf("screen sharing refused the connection: %s", reason)
	}
	types := make([]byte, n)
	if _, err := io.ReadFull(r, types); err != nil {
		return "", nil, err
	}
	return string(ver[:11]), types, nil
}

// Dial connects, logs in and starts reading screen updates.
func Dial(ctx context.Context, addr string, login Login) (*Client, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	c, err := handshake(conn, login)
	if err != nil {
		conn.Close()
		return nil, err
	}
	go c.readLoop()
	return c, nil
}

func handshake(conn net.Conn, login Login) (*Client, error) {
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	r := bufio.NewReaderSize(conn, 1<<20)
	_, offered, err := greet(conn, r)
	if err != nil {
		return nil, err
	}
	method, err := chooseMethod(login, offered)
	if err != nil {
		return nil, err
	}
	switch method {
	case MethodAccount:
		err = accountLogin(conn, r, login.User, login.Password)
	default:
		err = vncLogin(conn, r, login.VNCPassword)
	}
	if err != nil {
		return nil, err
	}
	var result [4]byte
	if _, err := io.ReadFull(r, result[:]); err != nil {
		return nil, fmt.Errorf("read login result: %w", err)
	}
	if binary.BigEndian.Uint32(result[:]) != 0 {
		reason, _ := readString(r)
		return nil, &AuthFailed{Method: method, Reason: reason}
	}
	// Shared: a person or another viewer already on Screen Sharing stays
	// connected.
	if _, err := conn.Write([]byte{1}); err != nil {
		return nil, err
	}
	var si [24]byte
	if _, err := io.ReadFull(r, si[:]); err != nil {
		return nil, fmt.Errorf("read server init: %w", err)
	}
	w, h := int(binary.BigEndian.Uint16(si[0:])), int(binary.BigEndian.Uint16(si[2:]))
	name, err := readStringLen(r, binary.BigEndian.Uint32(si[20:]))
	if err != nil {
		return nil, err
	}
	c := &Client{conn: conn, r: r, method: method, name: name, fb: newFramebuffer(w, h), done: make(chan struct{})}
	if err := c.write(setPixelFormatMsg(), setEncodingsMsg()); err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return c, nil
}

// chooseMethod applies Login.Method to what the server offers.
func chooseMethod(login Login, offered []byte) (Method, error) {
	has := func(t byte) bool {
		for _, o := range offered {
			if o == t {
				return true
			}
		}
		return false
	}
	switch login.Method {
	case MethodAccount:
		if !has(SecurityAccount) {
			return "", &AuthUnavailable{Wanted: []byte{SecurityAccount}, Offered: offered}
		}
		return MethodAccount, nil
	case MethodVNC:
		if !has(SecurityVNC) {
			return "", &AuthUnavailable{Wanted: []byte{SecurityVNC}, Offered: offered}
		}
		return MethodVNC, nil
	}
	if has(SecurityAccount) && login.User != "" && login.Password != "" {
		return MethodAccount, nil
	}
	if has(SecurityVNC) {
		return MethodVNC, nil
	}
	return "", &AuthUnavailable{Wanted: []byte{SecurityAccount, SecurityVNC}, Offered: offered}
}

// Method is the login that succeeded.
func (c *Client) Method() Method { return c.method }

// Name is the desktop name the server reported.
func (c *Client) Name() string { return c.name }

// Size is the current screen size in pixels.
func (c *Client) Size() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fb.w, c.fb.h
}

// Done is closed when the connection ends; Err then says why.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err is why the connection ended, or nil while it is open.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Close ends the connection.
func (c *Client) Close() error {
	err := c.conn.Close()
	<-c.done
	return err
}

// Capture asks for rect and returns it once every pixel of it has arrived.
func (c *Client) Capture(ctx context.Context, rect image.Rectangle) (*image.RGBA, error) {
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return nil, err
	}
	bounds := image.Rect(0, 0, c.fb.w, c.fb.h)
	if rect.Empty() || !rect.In(bounds) {
		c.mu.Unlock()
		return nil, fmt.Errorf("rfb: rectangle %v is outside the %dx%d screen", rect, c.fb.w, c.fb.h)
	}
	wt := &captureWaiter{rect: rect, ch: make(chan error, 1)}
	c.waiters = append(c.waiters, wt)
	c.mu.Unlock()

	if err := c.write(updateRequestMsg(rect)); err != nil {
		c.dropWaiter(wt)
		return nil, err
	}
	timer := time.NewTimer(captureTimeout)
	defer timer.Stop()
	select {
	case err := <-wt.ch:
		if err != nil {
			return nil, err
		}
	case <-timer.C:
		c.dropWaiter(wt)
		return nil, ErrCaptureTimeout
	case <-ctx.Done():
		c.dropWaiter(wt)
		return nil, ctx.Err()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fb.rgba(rect), nil
}

func (c *Client) dropWaiter(wt *captureWaiter) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, w := range c.waiters {
		if w == wt {
			c.waiters = append(c.waiters[:i], c.waiters[i+1:]...)
			return
		}
	}
}

// Key sends one KeyEvent.
func (c *Client) Key(down bool, keysym uint32) error {
	b := make([]byte, 8)
	b[0] = 4
	if down {
		b[1] = 1
	}
	binary.BigEndian.PutUint32(b[4:], keysym)
	return c.write(b)
}

// Pointer sends one PointerEvent: mask bit 0 is the left button, 1 middle,
// 2 right, 3/4 the vertical wheel and 5/6 the horizontal one.
func (c *Client) Pointer(mask uint8, x, y int) error {
	b := make([]byte, 6)
	b[0] = 5
	b[1] = mask
	binary.BigEndian.PutUint16(b[2:], uint16(clamp(x, 0, 0xffff)))
	binary.BigEndian.PutUint16(b[4:], uint16(clamp(y, 0, 0xffff)))
	return c.write(b)
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func (c *Client) write(msgs ...[]byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	for _, m := range msgs {
		if _, err := c.conn.Write(m); err != nil {
			return fmt.Errorf("%w: %v", ErrClosed, err)
		}
	}
	return nil
}

func (c *Client) readLoop() {
	err := c.readMessages()
	_ = c.conn.Close()
	c.mu.Lock()
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		err = fmt.Errorf("%w: %v", ErrClosed, err)
	}
	c.err = err
	for _, w := range c.waiters {
		w.ch <- err
	}
	c.waiters = nil
	c.mu.Unlock()
	close(c.done)
}

func (c *Client) readMessages() error {
	for {
		t, err := c.r.ReadByte()
		if err != nil {
			return err
		}
		switch t {
		case 0:
			if err := c.readUpdate(); err != nil {
				return err
			}
		case 2: // Bell
		case 3: // ServerCutText
			var hdr [7]byte
			if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
				return err
			}
			if _, err := c.r.Discard(int(binary.BigEndian.Uint32(hdr[3:]))); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: server message type %d", ErrProtocol, t)
		}
	}
}

func (c *Client) readUpdate() error {
	var hdr [3]byte
	if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
		return err
	}
	n := int(binary.BigEndian.Uint16(hdr[1:]))
	for i := 0; i < n; i++ {
		var rh [12]byte
		if _, err := io.ReadFull(c.r, rh[:]); err != nil {
			return err
		}
		x, y := int(binary.BigEndian.Uint16(rh[0:])), int(binary.BigEndian.Uint16(rh[2:]))
		w, h := int(binary.BigEndian.Uint16(rh[4:])), int(binary.BigEndian.Uint16(rh[6:]))
		switch enc := int32(binary.BigEndian.Uint32(rh[8:])); enc {
		case encodingRaw:
			if err := c.readRaw(image.Rect(x, y, x+w, y+h)); err != nil {
				return err
			}
		case encodingDesktopSize:
			c.resize(w, h)
		default:
			return fmt.Errorf("%w: encoding %d", ErrProtocol, enc)
		}
	}
	return nil
}

func (c *Client) readRaw(rect image.Rectangle) error {
	// The pixels are read off the socket outside the lock, into the read
	// loop's own buffer, and copied into the framebuffer under it, so a
	// Capture copying out never sees a half-written rectangle.
	n := area(rect) * 4
	if cap(c.rawBuf) < n {
		c.rawBuf = make([]byte, n)
	}
	buf := c.rawBuf[:n]
	if _, err := io.ReadFull(c.r, buf); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.fb.blit(rect, buf) {
		return fmt.Errorf("%w: rectangle %v outside the %dx%d screen", ErrProtocol, rect, c.fb.w, c.fb.h)
	}
	keep := c.waiters[:0]
	for _, w := range c.waiters {
		w.covered += area(w.rect.Intersect(rect))
		if w.covered >= area(w.rect) {
			w.ch <- nil
			continue
		}
		keep = append(keep, w)
	}
	c.waiters = keep
	return nil
}

func (c *Client) resize(w, h int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fb = newFramebuffer(w, h)
	for _, wt := range c.waiters {
		wt.ch <- ErrResized
	}
	c.waiters = nil
}

func area(r image.Rectangle) int { return r.Dx() * r.Dy() }

func readString(r *bufio.Reader) (string, error) {
	var n [4]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return "", err
	}
	return readStringLen(r, binary.BigEndian.Uint32(n[:]))
}

func readStringLen(r *bufio.Reader, n uint32) (string, error) {
	if n > 1<<16 {
		return "", fmt.Errorf("%w: string of %d bytes", ErrProtocol, n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", err
	}
	return string(b), nil
}
