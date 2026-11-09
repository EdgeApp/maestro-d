package hub

// link.go: one peer link. A link is a WebSocket between two hubs that
// carries framed HTTP exchanges in both directions: the side that sends a
// req frame waits for the matching res frame, and the other side runs the
// request through its own router (so every route, check and error code is
// the same as for a local client). Bodies up to 1 MiB travel inline; a
// larger or streamed body follows as binary data frames. A control link
// is pinged every PingEvery; after MissedPongs missed pongs it is down.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// errLinkDown fails the exchanges of a link that closed.
var errLinkDown = errors.New("peer link closed")

// errBodyUnread ends a streamed request body its handler did not read.
var errBodyUnread = errors.New("request answered before its body was read")

// Request is one exchange's request, as the entry hub sends it.
type Request struct {
	Method, Path, Query string
	Header              http.Header
	// Principal names the client the entry hub authenticated.
	Principal string
	// Body is sent inline; Stream, when set, is streamed instead.
	Body   []byte
	Stream io.Reader
}

// Response is an exchange's answer. Body must be closed.
type Response struct {
	Status int
	Header http.Header
	Body   io.ReadCloser
}

type exchange struct {
	res  chan resFrame
	body *inbound
}

type resFrame struct {
	f    Frame
	body *inbound
}

type served struct {
	cancel context.CancelFunc
	body   *inbound
}

// Link is one open peer link.
type Link struct {
	m      *Mesh
	conn   *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc

	// peer, instance and addr describe the other hub (from its hello).
	peer, instance, addr string
	// outbound is true on the side that dialed.
	outbound bool
	// bulk links carry one exchange and are never pinged.
	bulk  bool
	since time.Time

	idPrefix string
	nextID   atomic.Uint64
	rtt      atomic.Int64
	// dialable: bulk links to addr open (bulk.go); probing: a probe of
	// addr is running.
	dialable, probing atomic.Bool

	mu      sync.Mutex
	pending map[string]*exchange
	serving map[string]*served
	// sending holds the flow-control windows of the bodies this side
	// streams, by exchange id (flow.go).
	sending map[string]*credits

	closed    chan struct{}
	closeOnce sync.Once
	reason    string
	// goingAway is set when the peer closed the link because it stopped.
	goingAway bool
}

func newLink(m *Mesh, conn *websocket.Conn, outbound, bulk bool) *Link {
	ctx, cancel := context.WithCancel(context.Background())
	l := &Link{m: m, conn: conn, ctx: ctx, cancel: cancel, outbound: outbound, bulk: bulk, since: time.Now(),
		pending: map[string]*exchange{}, serving: map[string]*served{}, sending: map[string]*credits{}, closed: make(chan struct{})}
	l.idPrefix = "a"
	if outbound {
		l.idPrefix = "d"
	}
	conn.SetReadLimit(16 << 20)
	return l
}

// RTT is the last measured ping round trip (0 before the first pong).
func (l *Link) RTT() time.Duration { return time.Duration(l.rtt.Load()) }

func (l *Link) alive() bool {
	select {
	case <-l.closed:
		return false
	default:
		return true
	}
}

// Reason says why the link closed.
func (l *Link) Reason() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reason
}

func (l *Link) writeFrame(f Frame) error {
	data, err := encodeFrame(f)
	if err != nil {
		return err
	}
	return l.conn.Write(l.ctx, websocket.MessageText, data)
}

func (l *Link) writeData(id string, payload []byte) error {
	return l.conn.Write(l.ctx, websocket.MessageBinary, encodeData(id, payload))
}

// writeBody streams p as data frames of at most chunkSize, each within
// the body's flow-control window o.
func (l *Link) writeBody(o *credits, id string, p []byte) error {
	for len(p) > 0 {
		n := min(len(p), chunkSize)
		if err := o.acquire(n, l.closed); err != nil {
			return err
		}
		if err := l.writeData(id, p[:n]); err != nil {
			return err
		}
		p = p[n:]
	}
	return nil
}

// sendWindow registers the flow-control window of a body this side
// streams for exchange id; dropSend forgets it.
func (l *Link) sendWindow(id string) *credits {
	o := newCredits()
	l.mu.Lock()
	l.sending[id] = o
	l.mu.Unlock()
	return o
}

func (l *Link) dropSend(id string) {
	l.mu.Lock()
	delete(l.sending, id)
	l.mu.Unlock()
}

func (l *Link) onCredit(f Frame) {
	l.mu.Lock()
	o := l.sending[f.ID]
	l.mu.Unlock()
	if o != nil {
		o.credit(f.N)
	}
}

// close ends the link with a reason; status is the WebSocket close code.
func (l *Link) close(status websocket.StatusCode, reason string) {
	l.closeOnce.Do(func() {
		l.mu.Lock()
		l.reason = reason
		pending, serving := l.pending, l.serving
		l.pending, l.serving = map[string]*exchange{}, map[string]*served{}
		l.mu.Unlock()
		go func() {
			// Close waits for the peer's close frame; never block on it.
			_ = l.conn.Close(status, reason)
			l.cancel()
		}()
		time.AfterFunc(2*time.Second, l.cancel)
		for _, ex := range pending {
			if ex.body != nil {
				ex.body.finish(errLinkDown)
			}
		}
		for _, s := range serving {
			s.cancel()
			if s.body != nil {
				s.body.finish(errLinkDown)
			}
		}
		close(l.closed)
	})
}

// readLoop reads frames until the link fails.
func (l *Link) readLoop() {
	for {
		typ, data, err := l.conn.Read(l.ctx)
		if err != nil {
			reason := err.Error()
			status := websocket.CloseStatus(err)
			if status == websocket.StatusGoingAway {
				l.mu.Lock()
				l.goingAway = true
				l.mu.Unlock()
			}
			if status != -1 {
				var ce websocket.CloseError
				if errors.As(err, &ce) && ce.Reason != "" {
					reason = ce.Reason
				}
			}
			l.close(websocket.StatusNormalClosure, reason)
			return
		}
		if typ == websocket.MessageBinary {
			l.onData(data)
			continue
		}
		f, err := decodeFrame(data)
		if err != nil {
			l.m.logf("link %s: %v", l.peer, err)
			continue
		}
		switch f.T {
		case frameReq:
			l.onReq(f)
		case frameRes:
			l.onRes(f)
		case frameCancel:
			l.onCancel(f.ID)
		case frameCredit:
			l.onCredit(f)
		case frameHello, framePeers:
			l.m.learn(f.Peers)
		}
	}
}

// onData routes a data frame to the body it belongs to: a response body
// for one of this side's exchanges, or a request body this side serves.
// It queues the payload and never blocks on the body's reader (flow.go).
func (l *Link) onData(data []byte) {
	id, payload, err := decodeData(data)
	if err != nil {
		return
	}
	l.mu.Lock()
	var in *inbound
	if ex := l.pending[id]; ex != nil {
		in = ex.body
	} else if s := l.serving[id]; s != nil {
		in = s.body
	}
	l.mu.Unlock()
	if in == nil {
		return
	}
	if len(payload) == 0 {
		in.finish(nil)
		return
	}
	// A reader that went away (a cancelled exchange) just drops the rest.
	in.push(payload)
}

func (l *Link) onRes(f Frame) {
	l.mu.Lock()
	ex := l.pending[f.ID]
	if ex != nil && f.Stream {
		ex.body = newInbound(l, f.ID)
	}
	l.mu.Unlock()
	if ex == nil {
		return
	}
	select {
	case ex.res <- resFrame{f: f, body: ex.body}:
	default:
	}
}

func (l *Link) onCancel(id string) {
	l.mu.Lock()
	s := l.serving[id]
	o := l.sending[id]
	l.mu.Unlock()
	if o != nil {
		o.credit(creditStop)
	}
	if s != nil {
		s.cancel()
		if s.body != nil {
			_ = s.body.CloseWithError(context.Canceled)
		}
	}
}

func (l *Link) forget(id string) {
	l.mu.Lock()
	delete(l.pending, id)
	l.mu.Unlock()
}

func (l *Link) sendCancel(id string) {
	_ = l.writeFrame(Frame{T: frameCancel, ID: id})
}

// RoundTrip sends one exchange and waits for its answer. A link that goes
// down first fails it with errLinkDown; a ctx that ends first sends the
// owner a cancel.
func (l *Link) RoundTrip(ctx context.Context, req Request) (*Response, error) {
	id := l.idPrefix + strconv.FormatUint(l.nextID.Add(1), 10)
	ex := &exchange{res: make(chan resFrame, 1)}
	l.mu.Lock()
	if !l.alive() {
		l.mu.Unlock()
		return nil, errLinkDown
	}
	l.pending[id] = ex
	var o *credits
	if req.Stream != nil {
		o = newCredits()
		l.sending[id] = o
	}
	l.mu.Unlock()
	f := Frame{T: frameReq, ID: id, Method: req.Method, Path: req.Path, Query: req.Query, Headers: req.Header,
		Principal: req.Principal, Body: req.Body, Stream: req.Stream != nil}
	if err := l.writeFrame(f); err != nil {
		l.forget(id)
		l.dropSend(id)
		return nil, fmt.Errorf("%w: %v", errLinkDown, err)
	}
	if o != nil {
		go l.streamBody(ctx, o, id, req.Stream)
	}
	select {
	case r := <-ex.res:
		res := &Response{Status: r.f.Status, Header: r.f.Headers}
		if res.Header == nil {
			res.Header = http.Header{}
		}
		if r.body == nil {
			l.forget(id)
			res.Body = io.NopCloser(bytes.NewReader(r.f.Body))
			return res, nil
		}
		b := &streamBody{l: l, id: id, r: r.body}
		// A caller that goes away while the body is idle (an event stream
		// between events) unblocks its reader, which then closes it.
		b.stop = context.AfterFunc(ctx, func() { _ = r.body.CloseWithError(ctx.Err()) })
		res.Body = b
		return res, nil
	case <-ctx.Done():
		l.forget(id)
		l.sendCancel(id)
		return nil, ctx.Err()
	case <-l.closed:
		return nil, errLinkDown
	}
}

// streamBody sends a request body as data frames within its window o,
// then the end frame. A reader on the owner that stops it ends it early.
func (l *Link) streamBody(ctx context.Context, o *credits, id string, r io.Reader) {
	defer l.dropSend(id)
	buf := make([]byte, chunkSize)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if werr := l.writeBody(o, id, buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				l.m.logf("link %s: request body: %v", l.peer, err)
				l.sendCancel(id)
			}
			_ = l.writeData(id, nil)
			return
		}
		if ctx.Err() != nil {
			l.sendCancel(id)
			return
		}
	}
}

// streamBody is a streamed response body; closing it early cancels the
// exchange on the owner. When the exchange's ctx ends, a Read fails with
// its error.
type streamBody struct {
	l    *Link
	id   string
	r    *inbound
	stop func() bool
	done atomic.Bool
}

func (b *streamBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if errors.Is(err, io.EOF) {
		b.done.Store(true)
	}
	return n, err
}

func (b *streamBody) Close() error {
	b.stop()
	if !b.done.Load() {
		b.l.sendCancel(b.id)
	}
	b.l.forget(b.id)
	return b.r.Close()
}

// onReq serves one exchange the peer sent.
func (l *Link) onReq(f Frame) {
	ctx, cancel := context.WithCancel(l.ctx)
	var body io.ReadCloser = io.NopCloser(bytes.NewReader(f.Body))
	s := &served{cancel: cancel}
	if f.Stream {
		s.body = newInbound(l, f.ID)
		body = s.body
	}
	l.mu.Lock()
	l.serving[f.ID] = s
	l.mu.Unlock()
	go func() {
		defer func() {
			cancel()
			l.mu.Lock()
			delete(l.serving, f.ID)
			delete(l.sending, f.ID)
			l.mu.Unlock()
			// A handler that answered without reading the whole body (a
			// refused upload): close it, so the sender stops and what
			// still arrives is dropped.
			if s.body != nil {
				_ = s.body.CloseWithError(errBodyUnread)
			}
		}()
		w := &linkWriter{l: l, id: f.ID, header: http.Header{}}
		target := f.Path
		if f.Query != "" {
			target += "?" + f.Query
		}
		req, err := http.NewRequestWithContext(ctx, f.Method, "http://maestro-d"+target, body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			w.finish()
			return
		}
		if f.Headers != nil {
			req.Header = f.Headers
		}
		if !f.Stream {
			req.ContentLength = int64(len(f.Body))
		}
		req.RemoteAddr = f.Principal + " via hub " + l.peer
		l.m.serveForwarded(w, req, l.peer)
		w.finish()
	}()
}

// linkWriter is the ResponseWriter of an exchange this side serves: it
// buffers a small answer for one res frame, and streams a large or flushed
// one (an event stream) as data frames.
type linkWriter struct {
	l  *Link
	id string
	// out is the streamed body's flow-control window.
	out       *credits
	header    http.Header
	status    int
	streaming bool
	buf       bytes.Buffer
	err       error
}

func (w *linkWriter) Header() http.Header { return w.header }

func (w *linkWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *linkWriter) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	if w.err != nil {
		return 0, w.err
	}
	if w.streaming {
		if err := w.l.writeBody(w.out, w.id, p); err != nil {
			w.err = err
			return 0, err
		}
		return len(p), nil
	}
	w.buf.Write(p)
	if w.buf.Len() > maxInlineBody {
		w.startStream()
	}
	return len(p), w.err
}

// Flush starts streaming: an event stream's lines go out as they come.
func (w *linkWriter) Flush() {
	w.WriteHeader(http.StatusOK)
	if !w.streaming && w.err == nil {
		w.startStream()
	}
}

func (w *linkWriter) startStream() {
	w.streaming = true
	w.out = w.l.sendWindow(w.id)
	if err := w.l.writeFrame(Frame{T: frameRes, ID: w.id, Status: w.status, Headers: w.header, Stream: true}); err != nil {
		w.err = err
		return
	}
	if err := w.l.writeBody(w.out, w.id, w.buf.Bytes()); err != nil {
		w.err = err
	}
	w.buf.Reset()
}

func (w *linkWriter) finish() {
	w.WriteHeader(http.StatusOK)
	if w.err != nil {
		return
	}
	if w.streaming {
		_ = w.l.writeData(w.id, nil)
		return
	}
	_ = w.l.writeFrame(Frame{T: frameRes, ID: w.id, Status: w.status, Headers: w.header, Body: w.buf.Bytes()})
}

// pingLoop pings the peer until the link closes, recording the round
// trip; after maxMissed missed pongs in a row it closes the link.
func (l *Link) pingLoop(every, timeout time.Duration, maxMissed int) {
	missed := 0
	ping := func() {
		ctx, cancel := context.WithTimeout(l.ctx, timeout)
		start := time.Now()
		err := l.conn.Ping(ctx)
		cancel()
		if l.ctx.Err() != nil {
			return
		}
		if err == nil {
			l.rtt.Store(int64(time.Since(start)))
			missed = 0
			return
		}
		missed++
		l.m.logf("link %s: no pong (%d of %d)", l.peer, missed, maxMissed)
		if missed >= maxMissed {
			l.close(websocket.StatusPolicyViolation, fmt.Sprintf("no pong for %d pings", maxMissed))
		}
	}
	ping()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-l.closed:
			return
		case <-t.C:
			ping()
		}
	}
}
