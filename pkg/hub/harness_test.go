package hub

// harness_test.go: an in-process multi-hub harness. Each test hub is a
// real daemon.Server (hub mode, mock driver, a fixed device list) on
// 127.0.0.1:0 with its own Mesh, fast timings and a fake discoverer. A
// hook in front of each hub's own handler lets a test see, delay or
// answer what reaches the owner.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/driver/mock"
	"github.com/devicelab-dev/maestro-runner/pkg/report"
)

const testToken, testPeerToken = "client-tok", "peer-tok"

// fakeDisco is a Discoverer that answers from a list and counts browses.
type fakeDisco struct {
	mu        sync.Mutex
	found     []Found
	browses   int
	advertise []string
	err       error
}

func (f *fakeDisco) Advertise(name string, port int, txt []string) (func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.advertise = append(f.advertise, name)
	return func() {}, nil
}

func (f *fakeDisco) Browse(ctx context.Context, d time.Duration) ([]Found, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.browses++
	return append([]Found(nil), f.found...), f.err
}

type testHub struct {
	t      *testing.T
	name   string
	srv    *daemon.Server
	mesh   *Mesh
	addr   string
	done   chan error
	stopMu sync.Once
	// hook sees every request that reaches this hub's own handler; when
	// it returns true it has answered.
	hook atomic.Pointer[func(w http.ResponseWriter, r *http.Request) bool]
	seen sync.Map // path → count
	// lists counts the daemon's live device discoveries.
	lists atomic.Int32

	logMu sync.Mutex
	logs  []string
}

// logged counts the mesh log lines that contain sub.
func (h *testHub) logged(sub string) int {
	h.logMu.Lock()
	defer h.logMu.Unlock()
	n := 0
	for _, l := range h.logs {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

// setupHome gives the test its own maestro-d home.
func setupHome(t *testing.T) {
	t.Helper()
	t.Setenv(daemon.EnvHome, t.TempDir())
}

// homeIsTemp reports whether the daemon home is a test's temporary
// directory rather than the user's real one.
func homeIsTemp() bool {
	h := os.Getenv(daemon.EnvHome)
	return h != "" && strings.HasPrefix(filepath.Clean(h), filepath.Clean(os.TempDir()))
}

// hookedMesh wraps the daemon's handlers with the test hub's hook.
type hookedMesh struct {
	*Mesh
	h *testHub
}

func (hm *hookedMesh) Handler(local http.Handler, tcp bool) http.Handler {
	h := hm.h
	return hm.Mesh.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := h.seen.LoadOrStore(r.Method+" "+r.URL.Path, new(atomic.Int32))
		n.(*atomic.Int32).Add(1)
		if f := h.hook.Load(); f != nil && (*f)(w, r) {
			return
		}
		local.ServeHTTP(w, r)
	}), tcp)
}

func (h *testHub) count(key string) int {
	if n, ok := h.seen.Load(key); ok {
		return int(n.(*atomic.Int32).Load())
	}
	return 0
}

func (h *testHub) setHook(f func(w http.ResponseWriter, r *http.Request) bool) { h.hook.Store(&f) }

// readyWriter signals the first READY or FAILED line.
type readyWriter struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	line chan string
}

func (rw *readyWriter) Write(p []byte) (int, error) {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	rw.buf.Write(p)
	if s := rw.buf.String(); strings.Contains(s, "\n") {
		select {
		case rw.line <- s:
		default:
		}
	}
	return len(p), nil
}

type hubOpt func(*Config, *daemon.Config)

func withPeers(addrs ...string) hubOpt {
	return func(c *Config, _ *daemon.Config) { c.Peers = addrs }
}

func withDisco(d Discoverer) hubOpt {
	return func(c *Config, _ *daemon.Config) { c.Discoverer, c.NoMDNS = d, false }
}

func withShare(ids ...string) hubOpt {
	return func(_ *Config, d *daemon.Config) { d.ShareDevices = ids }
}

func testDevices(prefix string) []daemon.DeviceInfo {
	return []daemon.DeviceInfo{
		{ID: prefix + "1", Platform: "mock", Kind: "device", Name: "Mock " + prefix + "1", State: "device", Ready: true},
		{ID: prefix + "2", Platform: "mock", Kind: "device", Name: "Mock " + prefix + "2", State: "device", Ready: true},
	}
}

func fastConfig(name string) Config {
	return Config{Name: name, Token: testToken, PeerToken: testPeerToken, NoMDNS: true, Version: "test",
		BrowseFor: 20 * time.Millisecond, ReadyWait: 3 * time.Second, PingEvery: 100 * time.Millisecond,
		BackoffMin: 20 * time.Millisecond, BackoffMax: 200 * time.Millisecond, DialTimeout: 2 * time.Second,
		FanoutTimeout: time.Second, CacheFor: 2 * time.Second, Logf: func(string, ...any) {}}
}

// startHub runs a hub; it fails the test unless the hub reports READY.
func startHub(t *testing.T, name string, devs []daemon.DeviceInfo, opts ...hubOpt) *testHub {
	t.Helper()
	h, line := launchHub(t, name, devs, opts...)
	if !strings.HasPrefix(line, "READY") {
		t.Fatalf("hub %s: %s", name, line)
	}
	return h
}

// launchHub runs a hub and returns its first startup line.
func launchHub(t *testing.T, name string, devs []daemon.DeviceInfo, opts ...hubOpt) (*testHub, string) {
	t.Helper()
	// Never let a test hub write into the real home: a test that did not
	// call setupHome gets its own temporary one here.
	if !homeIsTemp() {
		setupHome(t)
	}
	mcfg := fastConfig(name)
	dcfg := daemon.Config{Name: name, HTTPAddr: "127.0.0.1:0", Token: testToken, Hub: true, HubName: name}
	for _, o := range opts {
		o(&mcfg, &dcfg)
	}
	h := &testHub{t: t, name: name, done: make(chan error, 1)}
	mcfg.Logf = func(format string, args ...any) {
		h.logMu.Lock()
		h.logs = append(h.logs, fmt.Sprintf(format, args...))
		h.logMu.Unlock()
	}
	h.mesh = New(mcfg)
	dcfg.Mesh = &hookedMesh{Mesh: h.mesh, h: h}
	rw := &readyWriter{line: make(chan string, 1)}
	dcfg.Ready = rw
	deps := daemon.Deps{
		Version: "test",
		NewDriver: func(ctx context.Context, id string, cfg daemon.AttachConfig) (core.Driver, daemon.DriverInfo, func(), error) {
			d := mock.New(mock.Config{Platform: "mock", DeviceID: id})
			return d, daemon.DriverInfo{DriverName: "mock", Device: report.Device{ID: id}, Platform: "mock", Kind: "device"}, func() {}, nil
		},
		ListDevices: func(platform string) []daemon.DeviceInfo {
			h.lists.Add(1)
			var out []daemon.DeviceInfo
			for _, d := range devs {
				if platform == "" || d.Platform == platform {
					out = append(out, d)
				}
			}
			return out
		},
	}
	h.srv = daemon.New(dcfg, deps)
	go func() { h.done <- h.srv.Run(context.Background()) }()
	select {
	case line := <-rw.line:
		if strings.HasPrefix(line, "READY") {
			st, err := daemon.Dial(name)
			if err != nil || st == nil {
				t.Fatalf("dial %s: %v", name, err)
			}
			h.addr = st.Info.HTTPAddr
			t.Cleanup(h.stop)
		}
		return h, line
	case err := <-h.done:
		t.Fatalf("hub %s exited: %v", name, err)
	case <-time.After(10 * time.Second):
		t.Fatalf("hub %s: no READY", name)
	}
	return nil, ""
}

func (h *testHub) stop() {
	h.stopMu.Do(func() {
		h.srv.Shutdown("test")
		select {
		case <-h.done:
		case <-time.After(10 * time.Second):
			h.t.Errorf("hub %s did not stop", h.name)
		}
	})
}

// client is a TCP client of this hub.
func (h *testHub) client() *daemon.Client {
	return daemon.NewHTTPClient("http://"+h.addr, testToken, nil)
}

// do sends a raw request to the hub's TCP listener.
func (h *testHub) do(t *testing.T, method, path string, body any, hdr ...string) (int, []byte) {
	t.Helper()
	var r io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		r = bytes.NewReader(b)
	case string:
		r = strings.NewReader(b)
	default:
		data, _ := json.Marshal(b)
		r = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, "http://"+h.addr+path, r)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func codeIn(body []byte) daemon.Code { return envelopeCode(body) }

// waitLinked waits until h has a live link to peer.
func waitLinked(t *testing.T, h *testHub, peer string) *Link {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if l, e := h.mesh.link(peer); e == nil {
			return l
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never linked to %s: %+v", h.name, peer, h.mesh.Status(context.Background()))
	return nil
}

// waitDown waits until h has no live link to peer.
func waitDown(t *testing.T, h *testHub, peer string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, e := h.mesh.link(peer); e != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s still linked to %s", h.name, peer)
}

func linkCount(m *Mesh) int { return len(m.linked()) }
