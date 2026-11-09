// Package hubtest holds test harnesses for maestro-d hubs. Proxy is a TCP
// proxy that makes a link slow and lossy: it adds latency, limits
// bandwidth, and can drop a connection right after it has forwarded a
// request, so a client's retry and the daemon's request-id cache can be
// tested end to end. The wanproxy command (cmd/wanproxy) runs it.
package hubtest

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ProxyConfig shapes the link.
type ProxyConfig struct {
	// Listen is the address clients connect to; Target is where the proxy
	// forwards.
	Listen, Target string
	// RTT is the round trip the proxy adds: half on each direction.
	RTT time.Duration
	// Rate caps each direction, in bits per second (0 = unlimited).
	Rate int64
	// DropAfterRequest, when N > 0, reads the client side as HTTP and, after
	// forwarding every Nth request (counted across connections) in full,
	// closes both sides of that connection before any response returns.
	DropAfterRequest int
	// Log receives one line per connection and per drop (nil = silent).
	Log *log.Logger
}

// Proxy is a running shaped proxy.
type Proxy struct {
	cfg      ProxyConfig
	ln       net.Listener
	requests atomic.Int64
	drops    atomic.Int64
	wg       sync.WaitGroup
	closed   atomic.Bool
}

// ParseRate parses "20mbit", "500kbit", "1gbit", "64000" (bits per second).
func ParseRate(s string) (int64, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || s == "0" {
		return 0, nil
	}
	mult := int64(1)
	for _, u := range []struct {
		suffix string
		m      int64
	}{{"gbit", 1e9}, {"mbit", 1e6}, {"kbit", 1e3}, {"bit", 1}} {
		if strings.HasSuffix(s, u.suffix) {
			s, mult = strings.TrimSuffix(s, u.suffix), u.m
			break
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("bad rate %q (want e.g. 20mbit)", s)
	}
	return int64(f * float64(mult)), nil
}

// Start listens and serves in the background.
func Start(cfg ProxyConfig) (*Proxy, error) {
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return nil, err
	}
	p := &Proxy{cfg: cfg, ln: ln}
	p.wg.Add(1)
	go p.serve()
	return p, nil
}

// Addr is the listening address.
func (p *Proxy) Addr() string { return p.ln.Addr().String() }

// Requests is how many requests the proxy has forwarded (drop mode only).
func (p *Proxy) Requests() int64 { return p.requests.Load() }

// Drops is how many connections the proxy has dropped.
func (p *Proxy) Drops() int64 { return p.drops.Load() }

// Close stops accepting; open connections finish on their own.
func (p *Proxy) Close() error {
	p.closed.Store(true)
	err := p.ln.Close()
	p.wg.Wait()
	return err
}

func (p *Proxy) logf(format string, args ...any) {
	if p.cfg.Log != nil {
		p.cfg.Log.Printf(format, args...)
	}
}

func (p *Proxy) serve() {
	defer p.wg.Done()
	for {
		c, err := p.ln.Accept()
		if err != nil {
			if p.closed.Load() || errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		go p.handle(c)
	}
}

func (p *Proxy) handle(client net.Conn) {
	upstream, err := net.Dial("tcp", p.cfg.Target)
	if err != nil {
		p.logf("dial %s: %v", p.cfg.Target, err)
		_ = client.Close()
		return
	}
	p.logf("connection %s -> %s", client.RemoteAddr(), p.cfg.Target)
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			_ = client.Close()
			_ = upstream.Close()
		})
	}
	down := newShaper(client, p.cfg.RTT/2, p.cfg.Rate, closeBoth)
	go func() {
		_, _ = io.Copy(down, upstream)
		down.closeWhenDrained()
	}()
	up := newShaper(upstream, p.cfg.RTT/2, p.cfg.Rate, closeBoth)
	if p.cfg.DropAfterRequest <= 0 {
		_, _ = io.Copy(up, client)
		up.closeWhenDrained()
		return
	}
	br := bufio.NewReader(client)
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			up.closeWhenDrained()
			return
		}
		n := p.requests.Add(1)
		if err := req.Write(up); err != nil {
			closeBoth()
			return
		}
		if n%int64(p.cfg.DropAfterRequest) == 0 {
			p.drops.Add(1)
			p.logf("request %d (%s %s): forwarded, dropping the connection", n, req.Method, req.URL.Path)
			up.closeWhenDrained()
			return
		}
	}
}

// shaper delays and rate-limits writes to one side of a connection. Writes
// are queued with the time they may be delivered; a goroutine delivers
// them in order.
type shaper struct {
	dst   net.Conn
	delay time.Duration
	rate  int64
	q     chan chunk
	done  chan struct{}
	fail  func()
	mu    sync.Mutex
	shut  bool
}

type chunk struct {
	data []byte
	at   time.Time
	last bool
}

func newShaper(dst net.Conn, delay time.Duration, rate int64, fail func()) *shaper {
	s := &shaper{dst: dst, delay: delay, rate: rate, q: make(chan chunk, 1024), done: make(chan struct{}), fail: fail}
	go s.run()
	return s
}

func (s *shaper) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.shut {
		return 0, net.ErrClosed
	}
	data := append([]byte(nil), b...)
	select {
	case s.q <- chunk{data: data, at: time.Now().Add(s.delay)}:
		return len(b), nil
	case <-s.done:
		return 0, net.ErrClosed
	}
}

// closeWhenDrained delivers what is queued, then closes both sides.
func (s *shaper) closeWhenDrained() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.shut {
		return
	}
	s.shut = true
	select {
	case s.q <- chunk{last: true, at: time.Now().Add(s.delay)}:
	case <-s.done:
	}
}

func (s *shaper) run() {
	defer close(s.done)
	for c := range s.q {
		if d := time.Until(c.at); d > 0 {
			time.Sleep(d)
		}
		if c.last {
			s.fail()
			return
		}
		if _, err := s.dst.Write(c.data); err != nil {
			s.fail()
			return
		}
		if s.rate > 0 {
			time.Sleep(time.Duration(float64(len(c.data)*8) / float64(s.rate) * float64(time.Second)))
		}
	}
}
