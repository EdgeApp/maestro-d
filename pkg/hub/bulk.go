package hub

// bulk.go: bulk links. A request body over 1 MiB (a blob upload), an
// artifact, a screenshot and an event stream each get their own
// short-lived peer link, GET /v1/hub/link?bulk=1, so a 300 MB APK never
// delays the control link's pings or another device's tapOn. At most
// bulkPerPeer transfers run at once per peer; an event stream, which
// lives as long as its subscriber, is not one of them. When the peer
// cannot be dialed (it joined by dialing out, from behind NAT) the
// exchange runs on the control link instead.
//
// A peer's address is dialable once this hub has dialed it: the address
// of a link this hub dialed is, and that of a link the peer dialed in on
// (its --advertise, or its remote host and listener port) is only after a
// background probe opens a bulk link to it. Until then, and for BulkRetry
// after a bulk dial fails, exchanges use the control link at once rather
// than wait DialTimeout on an address that may never answer.
//
// A bulk link lives no longer than its peer's control link: both sides
// close it when the control link closes.

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const bulkPerPeer = 4

// needsBulk reports whether r goes on a bulk link.
func needsBulk(r *http.Request) bool {
	p := r.URL.Path
	switch {
	case r.ContentLength > maxInlineBody:
		return true
	case r.Method == http.MethodPut && strings.Contains(p, "/blobs/"):
		return true
	case r.Method == http.MethodGet && (strings.Contains(p, "/artifacts/") || strings.HasSuffix(p, "/screenshot") || p == "/v1/events"):
		return true
	}
	return false
}

// isEventStream reports whether req subscribes to an event stream.
func isEventStream(req Request) bool {
	return req.Method == http.MethodGet && req.Path == "/v1/events"
}

func (m *Mesh) bulkSem(peer string) chan struct{} {
	m.bulkMu.Lock()
	defer m.bulkMu.Unlock()
	if m.bulkSems[peer] == nil {
		m.bulkSems[peer] = make(chan struct{}, bulkPerPeer)
	}
	return m.bulkSems[peer]
}

// exchange runs req with the peer of control link l, on a bulk link of
// its own when bulk is set and the peer can be dialed.
func (m *Mesh) exchange(ctx context.Context, l *Link, req Request, bulk bool) (*Response, error) {
	if !bulk || l.addr == "" || !l.dialable.Load() {
		return l.RoundTrip(ctx, req)
	}
	// An event stream would hold its slot for as long as anyone listens,
	// so four open subscriptions would block every transfer: it takes a
	// bulk link without one.
	release := func() {}
	if !isEventStream(req) {
		sem := m.bulkSem(l.peer)
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		release = func() { <-sem }
	}
	bl, err := m.dial(l.addr, l.peer, true)
	if err != nil {
		release()
		m.logf("bulk link to %s: %v; using the control link", l.peer, err)
		if l.dialable.CompareAndSwap(true, false) {
			go m.probeBulk(l)
		}
		return l.RoundTrip(ctx, req)
	}
	m.logf("bulk link to %s for %s %s", l.peer, req.Method, req.Path)
	go tieToControl(bl, l.closed)
	res, err := bl.RoundTrip(ctx, req)
	if err != nil {
		bl.close(websocket.StatusNormalClosure, "done")
		release()
		return nil, err
	}
	res.Body = &bulkBody{ReadCloser: res.Body, done: func() {
		bl.close(websocket.StatusNormalClosure, "done")
		release()
	}}
	return res, nil
}

// tieToControl closes bulk link bl when its peer's control link closes
// (ctl). Bulk links are not pinged: a peer that vanished without a FIN is
// noticed by the control link's missed pongs, and an event stream on a
// bulk link would otherwise stay open and silent for good. A nil ctl (no
// control link) leaves bl alone.
func tieToControl(bl *Link, ctl <-chan struct{}) {
	select {
	case <-ctl:
		bl.close(websocket.StatusNormalClosure, "control link to "+bl.peer+" closed")
	case <-bl.closed:
	}
}

type bulkBody struct {
	io.ReadCloser
	once sync.Once
	done func()
}

func (b *bulkBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.done)
	return err
}

// probeBulk tests the address of control link l with a bulk link until
// one opens (then bulk links to that peer are used, and its address is
// passed on to the other peers) or the link closes; it retries every
// BulkRetry. One probe runs per link.
func (m *Mesh) probeBulk(l *Link) {
	if !l.probing.CompareAndSwap(false, true) {
		return
	}
	defer l.probing.Store(false)
	for l.alive() && m.ctx.Err() == nil {
		bl, err := m.dial(l.addr, l.peer, true)
		if err == nil {
			bl.close(websocket.StatusNormalClosure, "probe")
			l.dialable.Store(true)
			m.logf("bulk links to %s at %s work", l.peer, l.addr)
			go m.broadcastPeers()
			return
		}
		m.logf("bulk links to %s at %s fail (%v); using the control link, testing again in %v", l.peer, l.addr, err, m.cfg.BulkRetry)
		t := time.NewTimer(m.cfg.BulkRetry)
		select {
		case <-t.C:
		case <-l.closed:
		case <-m.ctx.Done():
		}
		t.Stop()
	}
}
