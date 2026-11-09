package daemon

// idempotency.go: the owner's outcome cache for X-Maestro-Request-Id.
//
// Over a slow or lossy link a client cannot tell a request that never
// arrived from one whose answer was lost, so it retries with the same
// request id. The daemon keeps each device's last 256 outcomes by id for 10
// minutes: a repeated id whose request is still running waits for that same
// outcome, and one that has finished is answered from the cache, so a
// retried inputText never types twice.
//
// A request with an id that came over TCP is also not cancelled the moment
// its connection drops: it keeps running for idemGrace so a retry can
// collect its outcome, and is cancelled only when no retry arrives in that
// time. (A unix-socket disconnect is the client ending, and cancels at
// once.)

import (
	"bytes"
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/logger"
)

// HeaderRequestID carries the client's id for one logical request.
const HeaderRequestID = "X-Maestro-Request-Id"

// HeaderReplayed marks a response answered from the outcome cache.
const HeaderReplayed = "X-Maestro-Replayed"

const (
	idemPerDevice = 256
	idemTTL       = 10 * time.Minute
)

// idemGrace is how long a request whose client disconnected keeps running
// for a retry to join (Server.idemGrace; tests shorten it).
const idemGrace = 10 * time.Second

type idemEntry struct {
	done   chan struct{}
	status int
	header http.Header
	body   []byte
	// at is when the request finished (zero while it runs).
	at     time.Time
	joined atomic.Bool
}

func (e *idemEntry) finished() bool {
	select {
	case <-e.done:
		return true
	default:
		return false
	}
}

func (e *idemEntry) replay(w http.ResponseWriter) {
	for k, vs := range e.header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set(HeaderReplayed, "true")
	w.WriteHeader(e.status)
	_, _ = w.Write(e.body)
}

type idemDevice struct {
	entries map[string]*idemEntry
	order   []string // oldest first
}

// idemCache holds outcomes per device.
type idemCache struct {
	mu      sync.Mutex
	now     func() time.Time
	devices map[string]*idemDevice
}

func newIdemCache() *idemCache {
	return &idemCache{now: time.Now, devices: map[string]*idemDevice{}}
}

// begin returns the entry for (device, id) and whether the caller must run
// the request (true) or wait for and replay the entry (false).
func (c *idemCache) begin(device, id string) (*idemEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	d := c.devices[device]
	if d == nil {
		d = &idemDevice{entries: map[string]*idemEntry{}}
		c.devices[device] = d
	}
	c.pruneLocked(d)
	if e := d.entries[id]; e != nil {
		return e, false
	}
	e := &idemEntry{done: make(chan struct{})}
	d.entries[id] = e
	d.order = append(d.order, id)
	for len(d.order) > idemPerDevice {
		delete(d.entries, d.order[0])
		d.order = d.order[1:]
	}
	return e, true
}

// pruneLocked drops finished entries older than the TTL.
func (c *idemCache) pruneLocked(d *idemDevice) {
	cutoff := c.now().Add(-idemTTL)
	keep := d.order[:0]
	for _, id := range d.order {
		e := d.entries[id]
		if e != nil && e.finished() && e.at.Before(cutoff) {
			delete(d.entries, id)
			continue
		}
		keep = append(keep, id)
	}
	d.order = keep
}

// finish records the outcome and wakes every waiter.
func (c *idemCache) finish(e *idemEntry, status int, header http.Header, body []byte) {
	c.mu.Lock()
	e.status, e.header, e.body, e.at = status, header, body, c.now()
	c.mu.Unlock()
	close(e.done)
}

// len reports the number of entries held for a device (tests).
func (c *idemCache) len(device string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if d := c.devices[device]; d != nil {
		return len(d.entries)
	}
	return 0
}

// teeWriter passes a response through while keeping a copy for the cache.
// After the client has gone, writes fail silently and the copy is still
// complete.
type teeWriter struct {
	w      http.ResponseWriter
	status int
	buf    bytes.Buffer
}

func (t *teeWriter) Header() http.Header { return t.w.Header() }

func (t *teeWriter) WriteHeader(status int) {
	t.status = status
	t.w.WriteHeader(status)
}

func (t *teeWriter) Write(p []byte) (int, error) {
	t.buf.Write(p)
	_, _ = t.w.Write(p)
	return len(p), nil
}

// idempotent wraps a step-running handler with the outcome cache, outside
// the device's busy lock, so a retry never queues behind the lock its own
// first attempt holds. On a hub it sits inside the lease guard (dev), not
// outside it: every attempt, a retry answered from the cache included, is
// checked for the device's share and lease first, so the cache never
// replays an outcome to a caller the guard would refuse. The guard's
// context ends with the lease (underLease); this wrapper detaches the
// request from its client, so it watches the lease itself (leaseFrom) to
// keep the lease's end cancelling the request at once.
func (s *Server) idempotent(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(HeaderRequestID)
		if id == "" {
			h(w, r)
			return
		}
		device := r.PathValue("id")
		e, run := s.idem.begin(device, id)
		if !run {
			e.joined.Store(true)
			if !e.finished() {
				logger.Info("request %s %s %s: a retry; waiting for the running attempt", id, r.Method, r.URL.Path)
			}
			select {
			case <-e.done:
			case <-r.Context().Done():
				// The lease ending also ends this context. A client that
				// is still here gets the running attempt's outcome, which
				// the lease's end cancels too; only a client that has
				// gone gets no answer.
				client := clientContext(r.Context())
				if client.Err() != nil {
					return
				}
				select {
				case <-e.done:
				case <-client.Done():
					return
				}
			}
			logger.Info("request %s %s %s: answered from the outcome cache (status %d)", id, r.Method, r.URL.Path, e.status)
			e.replay(w)
			return
		}
		caller := CallerFrom(r.Context())
		logger.Info("request %s %s %s: executing (caller %s)", id, r.Method, r.URL.Path, describeCaller(caller))
		tw := &teeWriter{w: w, status: http.StatusOK}
		if caller.Addr == "" {
			// The unix socket: a disconnect is the client ending (Ctrl-C),
			// never the network, so it cancels at once.
			h(tw, r)
			s.idem.finish(e, tw.status, w.Header().Clone(), tw.buf.Bytes())
			return
		}
		ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
		defer cancel()
		if le := leaseFrom(r.Context()); le != nil {
			// Detached from the client, but never from its lease: the end
			// of the lease cancels the request at once, not after the grace.
			var stopLease context.CancelFunc
			ctx, stopLease = untilLeaseEnds(ctx, le)
			defer stopLease()
		}
		grace := s.idemGrace
		stop := context.AfterFunc(r.Context(), func() {
			t := time.NewTimer(grace)
			defer t.Stop()
			select {
			case <-t.C:
				if !e.joined.Load() {
					logger.Info("request %s: client gone and no retry within %v; cancelling", id, grace)
					cancel()
				}
			case <-ctx.Done():
			}
		})
		defer stop()
		h(tw, r.WithContext(ctx))
		s.idem.finish(e, tw.status, w.Header().Clone(), tw.buf.Bytes())
		logger.Info("request %s %s %s: done (status %d)", id, r.Method, r.URL.Path, tw.status)
	}
}

func describeCaller(c Caller) string {
	switch {
	case c.Addr == "":
		return "unix socket"
	case c.Remote:
		return "remote " + c.Addr
	}
	return "local " + c.Addr
}
