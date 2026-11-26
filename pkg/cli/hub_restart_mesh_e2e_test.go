package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/hub"
)

// Controlled, loopback-only link between two mock hubs. Hold one successful
// grant at the peer boundary after the owner has acknowledged its disk write.
func cutGrantLink(t *testing.T, addr string) (string, <-chan daemon.Lease) {
	t.Helper()
	captured := make(chan daemon.Lease, 1)
	var cut atomic.Bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers := http.Header{}
		for k, v := range r.Header {
			if k == "Authorization" || strings.HasPrefix(k, "X-Maestro-") {
				headers[k] = v
			}
		}
		upstream, _, err := websocket.Dial(r.Context(), "ws://"+addr+r.URL.RequestURI(), &websocket.DialOptions{HTTPHeader: headers})
		if err != nil {
			http.Error(w, "fixture peer unavailable", http.StatusServiceUnavailable)
			return
		}
		defer upstream.CloseNow()
		downstream, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer downstream.CloseNow()
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		var wg sync.WaitGroup
		for _, fromOwner := range []bool{false, true} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer cancel()
				src, dst := downstream, upstream
				if fromOwner {
					src, dst = upstream, downstream
				}
				for {
					typ, data, err := src.Read(ctx)
					if err != nil {
						return
					}
					if fromOwner && typ == websocket.MessageText && !cut.Load() {
						var frame hub.Frame
						var result daemon.LeaseResult
						if json.Unmarshal(data, &frame) == nil && frame.T == "res" && frame.Status == 200 && json.Unmarshal(frame.Body, &result) == nil && result.OK && result.Lease.ID != "" && cut.CompareAndSwap(false, true) {
							captured <- result.Lease
							<-ctx.Done()
							return
						}
					}
					if dst.Write(ctx, typ, data) != nil {
						return
					}
				}
			}()
		}
		wg.Wait()
	}))
	t.Cleanup(proxy.Close)
	return strings.TrimPrefix(proxy.URL, "http://"), captured
}
func waitRestartPeers(t *testing.T, h *restartHub) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		ds, err := h.client().Devices(ctx, "mock")
		if err == nil && len(ds) == 2 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("isolated peers did not link")
}
func testLostMeshGrant(t *testing.T, e *e2e, betweenHubs bool) {
	t.Helper()
	a := newRestartHub(t, e, "entry", "mock-a")
	b := newRestartHub(t, e, "owner", "mock-b")
	b.start()
	peer := b.info.HTTPAddr
	var captured <-chan daemon.Lease
	if betweenHubs {
		peer, captured = cutGrantLink(t, peer)
	}
	a.start("--peer", peer)
	waitRestartPeers(t, a)
	blocker := a.acquire("mock-a", 360)
	body := []byte(`{"select":{"platform":"mock"},"ttlSec":300}`)
	restart := func() {
		a.kill()
		b.stop()
		a.start("--peer", peer)
		if _, err := a.client().ReleaseLease(context.Background(), blocker.ID); err != nil {
			t.Fatal(err)
		}
	}
	var orig daemon.Lease
	if betweenHubs {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		request, _ := http.NewRequestWithContext(ctx, "POST", "http://"+a.info.HTTPAddr+"/v1/leases", bytes.NewReader(body))
		request.Header.Set("Authorization", "Bearer restart-token")
		request.Header.Set(daemon.HeaderRequestID, "lost-key")
		done := make(chan struct{})
		go func() {
			defer close(done)
			resp, _ := http.DefaultClient.Do(request)
			if resp != nil {
				resp.Body.Close()
			}
		}()
		select {
		case orig = <-captured:
		case <-done:
			t.Fatal("client got response before link cut")
		case <-time.After(18 * time.Second):
			t.Fatal("no owner grant reached proxy")
		}
		t.Logf("$ withhold peer response\n%+v", orig)
		restart()
		<-done
		out, _ := sendGrant(t, a.info.HTTPAddr, "lost-key", body)
		if out.Error == nil || out.Error.Code != daemon.CodePeerUnavailable {
			t.Fatalf("uncertain owner: %+v", out)
		}
	} else {
		orig = withholdGrant(t, a.info.HTTPAddr, body, restart)
		reply, header := sendGrant(t, a.info.HTTPAddr, "lost-key", body)
		if !reply.OK || header != "true" || reply.Lease.ID != orig.ID || reply.Lease.ExpiresAt != orig.ExpiresAt {
			t.Fatalf("offline replay %q %+v", header, reply)
		}
	}
	if local := a.leases(); len(local) != 0 {
		t.Fatalf("entry allocated locally while peer offline: %+v", local)
	}
	b.start()
	waitRestartPeers(t, a)
	replay, _ := sendGrant(t, a.info.HTTPAddr, "lost-key", body)
	if !replay.OK || replay.Lease.ID != orig.ID || replay.Lease.Device != "mock-b@owner" || replay.Lease.ExpiresAt != orig.ExpiresAt {
		t.Fatalf("lost peer grant: %+v want %+v", replay, orig)
	}
	if grants := a.leases(); len(grants) != 1 || grants[0].ID != orig.ID {
		t.Fatalf("duplicate owner grant: %+v", grants)
	}
	a.acquire("mock-a", 60)
}
