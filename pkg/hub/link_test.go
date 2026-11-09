package hub

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// logMesh is a mesh that is never started, with its log captured.
type logMesh struct {
	*Mesh
	mu   sync.Mutex
	logs []string
}

func newLogMesh(name string) *logMesh {
	lm := &logMesh{}
	cfg := fastConfig(name)
	cfg.Logf = func(format string, args ...any) {
		lm.mu.Lock()
		lm.logs = append(lm.logs, fmt.Sprintf(format, args...))
		lm.mu.Unlock()
	}
	lm.Mesh = New(cfg)
	return lm
}

func (lm *logMesh) waitLog(t *testing.T, sub string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		lm.mu.Lock()
		for _, l := range lm.logs {
			if strings.Contains(l, sub) {
				lm.mu.Unlock()
				return
			}
		}
		lm.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	lm.mu.Lock()
	defer lm.mu.Unlock()
	t.Fatalf("no log line with %q: %q", sub, lm.logs)
}

// A bulk link knows its peer before its read loop runs: the loop's first
// log line (a bad frame here, sent at once) names the peer, and setting
// the name afterwards would race with that read (-race reports it).
func TestLink_BulkDialNamesThePeerBeforeReading(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		_ = conn.Write(r.Context(), websocket.MessageText, []byte("{"))
		<-r.Context().Done()
	}))
	defer srv.Close()
	lm := newLogMesh("hub-b")
	defer lm.Close()
	bl, err := lm.dial(strings.TrimPrefix(srv.URL, "http://"), "hub-a", true)
	if err != nil {
		t.Fatal(err)
	}
	defer bl.close(websocket.StatusNormalClosure, "done")
	lm.waitLog(t, "link hub-a: bad frame")
}

// A response body nobody reads holds up only its own exchange: its sender
// stops at the flow-control window, while the same control link keeps
// serving other exchanges and pongs; the body then reads in full, and
// closing one early stops its sender.
func TestLink_SlowReaderDoesNotStallTheLink(t *testing.T) {
	a, b := twoHubs(t)
	const size = 4*flowWindow + 12345
	writeErr := make(chan error, 4)
	a.setHook(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/v1/test/big" {
			return false
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		chunk := bytes.Repeat([]byte{7}, 64<<10)
		var err error
		for left := size; left > 0 && err == nil; left -= len(chunk) {
			_, err = w.Write(chunk[:min(len(chunk), left)])
		}
		writeErr <- err
		return true
	})
	l := waitLinked(t, b, "hub-a")
	auth := http.Header{"Authorization": {"Bearer " + testToken}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := l.RoundTrip(ctx, Request{Method: http.MethodGet, Path: "/v1/test/big", Header: auth.Clone(), Principal: "test"})
	if err != nil {
		t.Fatal(err)
	}
	// Several ping periods with the body unread, then other exchanges.
	time.Sleep(5 * fastConfig("x").PingEvery)
	for i := 0; i < 3; i++ {
		rctx, rcancel := context.WithTimeout(context.Background(), 3*time.Second)
		r2, err := l.RoundTrip(rctx, Request{Method: http.MethodGet, Path: "/v1/hub", Header: auth.Clone(), Principal: "test"})
		rcancel()
		if err != nil {
			t.Fatalf("exchange %d beside an unread body: %v", i, err)
		}
		_ = r2.Body.Close()
	}
	if !l.alive() {
		t.Fatalf("link closed: %s", l.Reason())
	}
	n, err := io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	if err != nil || n != size {
		t.Fatalf("body: %d of %d, %v", n, size, err)
	}
	if err := <-writeErr; err != nil {
		t.Fatalf("sender: %v", err)
	}
	// Closing a body early stops its sender.
	res, err = l.RoundTrip(ctx, Request{Method: http.MethodGet, Path: "/v1/test/big", Header: auth.Clone(), Principal: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := res.Body.Read(make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	select {
	case err := <-writeErr:
		if err == nil {
			t.Fatal("the sender wrote the whole body to a closed reader")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the sender of a closed body never stopped")
	}
}

func TestFlow_InboundAndCredits(t *testing.T) {
	lm := newLogMesh("hub-x")
	defer lm.Close()
	// A link to a peer that reads and drops every frame (the credits).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		for {
			if _, _, err := conn.Read(r.Context()); err != nil {
				return
			}
		}
	}))
	defer srv.Close()
	conn, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	l := newLink(lm.Mesh, conn, true, true)
	defer l.close(websocket.StatusNormalClosure, "done")
	in := newInbound(l, "d1")
	in.push([]byte("ab"))
	in.push([]byte("cd"))
	in.finish(nil)
	in.push([]byte("late"))
	got, err := io.ReadAll(in)
	// Reading a quarter window acknowledges it.
	credit := newInbound(l, "d0")
	credit.push(make([]byte, flowWindow/4))
	if n, err := io.ReadFull(credit, make([]byte, flowWindow/4)); err != nil || n != flowWindow/4 || credit.unacked != 0 {
		t.Fatalf("credit: %d %v unacked %d", n, err, credit.unacked)
	}
	if err != nil || string(got) != "abcd" {
		t.Fatalf("read: %q %v", got, err)
	}
	// A partial read keeps the rest of the chunk.
	in = newInbound(l, "d2")
	in.push([]byte("hello"))
	p := make([]byte, 2)
	if n, _ := in.Read(p); n != 2 || string(p) != "he" {
		t.Fatalf("partial: %q", p[:n])
	}
	in.finish(errLinkDown)
	if rest, err := io.ReadAll(in); string(rest) != "llo" || !errors.Is(err, errLinkDown) {
		t.Fatalf("drained before the error: %q %v", rest, err)
	}
	// Closed by the reader: Read fails, later data is dropped.
	in = newInbound(l, "d3")
	_ = in.Close()
	_ = in.Close()
	in.push([]byte("x"))
	if _, err := in.Read(p); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("closed: %v", err)
	}
	// A sender past twice its window is cut off.
	in = newInbound(l, "d4")
	big := make([]byte, flowWindow)
	for i := 0; i < 3; i++ {
		in.push(big)
	}
	if _, err := in.Read(p); !errors.Is(err, errFlowOverrun) {
		t.Fatalf("overrun: %v", err)
	}
	// A blocked reader wakes when the body closes.
	in = newInbound(l, "d5")
	done := make(chan error, 1)
	go func() { _, err := in.Read(p); done <- err }()
	time.Sleep(20 * time.Millisecond)
	_ = in.CloseWithError(context.Canceled)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("woken: %v", err)
	}

	c := newCredits()
	if err := c.acquire(flowWindow, l.closed); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- c.acquire(1, l.closed) }()
	select {
	case err := <-waited:
		t.Fatalf("acquired past the window: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	c.credit(flowWindow / 2)
	if err := <-waited; err != nil {
		t.Fatal(err)
	}
	go func() { waited <- c.acquire(flowWindow, l.closed) }()
	go func() { waited <- c.acquire(flowWindow, l.closed) }()
	if err := <-waited; err != nil {
		t.Fatal(err)
	}
	c.credit(creditStop)
	if err := <-waited; !errors.Is(err, errStreamStopped) {
		t.Fatalf("stopped: %v", err)
	}
	c = newCredits()
	_ = c.acquire(flowWindow, l.closed)
	l.close(websocket.StatusNormalClosure, "done")
	if err := c.acquire(1, l.closed); !errors.Is(err, errLinkDown) {
		t.Fatalf("link down: %v", err)
	}
}
