package daemon

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
)

// fakeMesh records how the daemon uses its mesh.
type fakeMesh struct {
	mu       sync.Mutex
	wrapped  []bool
	started  atomic.Int32
	closed   atomic.Int32
	startErr error
	// block, when set, holds Start until it is closed (a long browse).
	block chan struct{}
}

func (f *fakeMesh) Handler(local http.Handler, tcp bool) http.Handler {
	f.mu.Lock()
	f.wrapped = append(f.wrapped, tcp)
	f.mu.Unlock()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/hub" {
			writeJSON(w, http.StatusOK, HubStatusResult{Envelope: Envelope{OK: true}, Hub: HubStatus{Name: "fake", Peers: []HubPeer{}}})
			return
		}
		if r.URL.Path == "/v1/devices" {
			writeJSON(w, http.StatusOK, DevicesResult{Envelope: Envelope{OK: true}, Devices: []DeviceInfo{{ID: "x@fake"}},
				Unreachable: []UnreachablePeer{{Hub: "gone", Error: "link down"}}})
			return
		}
		local.ServeHTTP(w, r)
	})
}

func (f *fakeMesh) Start(ctx context.Context, self Info) error {
	f.started.Add(1)
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
		}
	}
	return f.startErr
}

func (f *fakeMesh) Close() { f.closed.Add(1) }

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestServer_MeshFrontsBothListenersAndGatesReady(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	var cleanups int32
	m := &fakeMesh{}
	ready := &syncBuf{}
	s := New(Config{Name: "meshd", HTTPAddr: "127.0.0.1:0", Token: "tk", Hub: true, HubName: "fake", Mesh: m, Ready: ready}, testDeps(&cleanups, 0))
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background()) }()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(ready.String(), "READY") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if m.started.Load() != 1 || len(m.wrapped) != 2 || m.wrapped[0] || !m.wrapped[1] {
		t.Fatalf("mesh: started %d, wrapped %v", m.started.Load(), m.wrapped)
	}
	c, err := Dial("meshd")
	if err != nil || c == nil {
		t.Fatal(err)
	}
	st, err := c.Hub(context.Background())
	if err != nil || st.Hub.Name != "fake" {
		t.Fatalf("hub: %+v %v", st, err)
	}
	res, err := c.DeviceList(context.Background(), "mock")
	if err != nil || len(res.Unreachable) != 1 || res.Devices[0].ID != "x@fake" {
		t.Fatalf("list: %+v %v", res, err)
	}
	tcp := NewHTTPClient("http://"+s.info.HTTPAddr, "tk", nil)
	if _, err := tcp.Hub(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.Shutdown("test")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if m.closed.Load() != 1 {
		t.Fatal("mesh not closed on shutdown")
	}
	// The handlers tests use go through the mesh too.
	_ = s.Handler()
	_ = s.TCPHandler()
	if len(m.wrapped) != 4 {
		t.Fatalf("wrapped %v", m.wrapped)
	}
}

func TestServer_MeshRefusalStopsTheHub(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	var cleanups int32
	m := &fakeMesh{startErr: Errorf(CodeHubNameTaken, "hub name %q is this hub's own name", "x")}
	ready := &syncBuf{}
	s := New(Config{Name: "meshr", HTTPAddr: "127.0.0.1:0", Token: "tk", Hub: true, Mesh: m, Ready: ready}, testDeps(&cleanups, 0))
	err := s.Run(context.Background())
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeHubNameTaken || e.ExitCode() != 5 {
		t.Fatalf("run: %v", err)
	}
	if !strings.HasPrefix(ready.String(), "FAILED code=HUB_NAME_TAKEN ") || strings.Contains(ready.String(), "READY") {
		t.Fatalf("startup line: %q", ready.String())
	}
	if _, err := os.Stat(InfoPath("meshr")); !os.IsNotExist(err) {
		t.Fatal("run files left")
	}
	// A plain error becomes DAEMON_UNAVAILABLE.
	if asError(errors.New("x")).Code != CodeDaemonUnavailable {
		t.Fatal("asError")
	}
}

// A hub writes daemon.json only once its mesh has started. A second start
// of the same daemon in that window is refused by the run-dir lock before
// it touches the socket, so the first keeps its socket and starts.
func TestServer_SecondStartDuringTheMeshStart(t *testing.T) {
	t.Setenv(EnvHome, t.TempDir())
	var cleanups int32
	m := &fakeMesh{block: make(chan struct{})}
	ready := &syncBuf{}
	s := New(Config{Name: "meshw", HTTPAddr: "127.0.0.1:0", Token: "tk", Hub: true, HubName: "fake", Mesh: m, Ready: ready}, testDeps(&cleanups, 0))
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background()) }()
	deadline := time.Now().Add(5 * time.Second)
	for m.started.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if m.started.Load() != 1 {
		t.Fatal("the mesh never started")
	}
	if info, _ := LiveInfo("meshw"); info != nil {
		t.Fatal("daemon.json written before the mesh started")
	}
	second := New(Config{Name: "meshw", HTTPAddr: "127.0.0.1:0", Token: "tk", Hub: true, HubName: "fake", Mesh: &fakeMesh{}}, testDeps(&cleanups, 0))
	secondDone := make(chan error, 1)
	go func() { secondDone <- second.Run(context.Background()) }()
	var err error
	select {
	case err = <-secondDone:
	case <-time.After(3 * time.Second):
		second.Shutdown("test")
		<-secondDone
		t.Fatal("a second start of a starting hub ran")
	}
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeDaemonUnavailable || !strings.Contains(e.Message, "already running or starting") {
		t.Fatalf("second start: %v", err)
	}
	close(m.block)
	for !strings.Contains(ready.String(), "READY") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	c, err := Dial("meshw")
	if err != nil || c == nil {
		t.Fatalf("dial: %v", err)
	}
	if err := c.Ping(2 * time.Second); err != nil {
		t.Fatalf("the first hub lost its socket: %v", err)
	}
	s.Shutdown("test")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// The lock goes with the daemon: the name starts again.
	again := New(Config{Name: "meshw"}, testDeps(&cleanups, 0))
	go func() { done <- again.Run(context.Background()) }()
	deadline = time.Now().Add(5 * time.Second)
	for c, _ = Dial("meshw"); c == nil && time.Now().Before(deadline); c, _ = Dial("meshw") {
		time.Sleep(10 * time.Millisecond)
	}
	if c == nil {
		t.Fatal("the name did not start again")
	}
	again.Shutdown("test")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSpawn_FailedLine(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "startup.log")
	_ = os.WriteFile(p, []byte("noise\nFAILED code=HUB_VERSION hub b speaks API 0\n"), 0o600)
	e := failedLine(p)
	if e == nil || e.Code != CodeHubVersion || e.Message != "hub b speaks API 0" {
		t.Fatalf("failed line: %+v", e)
	}
	_ = os.WriteFile(p, []byte("FAILED code=NOPE x\nREADY\n"), 0o600)
	if failedLine(p) != nil {
		t.Fatal("an unknown code")
	}
	if failedLine(filepath.Join(dir, "missing")) != nil {
		t.Fatal("missing file")
	}
}

func TestLeases_AttachRunsUnderTheLease(t *testing.T) {
	env := newHubEnv(t, Config{})
	// The clock jumps past the window while the attach runs: the window is
	// fixed, so the lease has ended, and the device is detached again
	// rather than left attached without a lease.
	slow := env.s.deps.NewDriver
	env.s.deps.NewDriver = func(ctx context.Context, id string, cfg AttachConfig) (core.Driver, DriverInfo, func(), error) {
		env.clock.advance(5 * time.Second)
		return slow(ctx, id, cfg)
	}
	_, err := env.remote.AcquireLease(context.Background(), LeaseRequest{Device: "mock-1", TTLSec: 2, Attach: &AttachConfig{Platform: "mock"}})
	wantErrCode(t, err, CodeLeaseExpired)
	if ls := env.s.leases.list(); len(ls) != 0 {
		t.Fatalf("leases after a window ended in its attach: %+v", ls)
	}
	if devs, _ := env.local.Devices(context.Background(), "mock"); devs[0].Attached {
		t.Fatalf("left attached: %+v", devs[0])
	}
	// A window that outlasts the attach is granted as asked: the attach
	// does not move it.
	res, err := env.remote.AcquireLease(context.Background(), LeaseRequest{Device: "mock-1", TTLSec: 60, Attach: &AttachConfig{Platform: "mock"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := env.clock.now().Add(55 * time.Second).Format(time.RFC3339); res.Lease.ExpiresAt != want {
		t.Fatalf("expiresAt %s, want %s (from the grant)", res.Lease.ExpiresAt, want)
	}
}

// On a hub every device entry names the machine it is attached to: the
// host name, and the advertised address followed by the host's
// non-loopback IPs.
func TestHub_DeviceEntriesNameTheMachine(t *testing.T) {
	env := newHubEnv(t, Config{Advertise: "hub.example:7433"})
	devs, err := env.remote.Devices(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	host, _ := os.Hostname()
	for _, d := range devs {
		if d.Machine != host || len(d.Addresses) == 0 || d.Addresses[0] != "hub.example:7433" {
			t.Fatalf("%s: machine %q addresses %v", d.ID, d.Machine, d.Addresses)
		}
		for _, a := range d.Addresses[1:] {
			if ip := net.ParseIP(a); ip == nil || ip.IsLoopback() {
				t.Fatalf("%s: address %q", d.ID, a)
			}
		}
	}
	// A plain daemon's entries carry neither.
	plain := newTestEnv(t, 0)
	pd, _ := plain.c.Devices(context.Background(), "")
	if len(pd) == 0 || pd[0].Machine != "" || pd[0].Addresses != nil {
		t.Fatalf("plain daemon: %+v", pd)
	}
	got := hostIPs(func() ([]net.Addr, error) {
		return []net.Addr{
			&net.IPNet{IP: net.ParseIP("127.0.0.1")}, &net.IPNet{IP: net.ParseIP("::1")},
			&net.IPNet{IP: net.ParseIP("fe80::1")}, &net.IPNet{IP: net.ParseIP("192.0.2.7")},
			&net.IPAddr{IP: net.ParseIP("2001:db8::7")}, &net.IPNet{IP: net.ParseIP("192.0.2.7")},
		}, nil
	})
	if strings.Join(got, ",") != "192.0.2.7,2001:db8::7" {
		t.Fatalf("hostIPs: %v", got)
	}
	if hostIPs(func() ([]net.Addr, error) { return nil, errors.New("no interfaces") }) != nil {
		t.Fatal("hostIPs on error")
	}
}
