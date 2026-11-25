package hubd

import (
	"context"
	"io"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/internal/daemontest"
	"github.com/devicelab-dev/maestro-runner/pkg/capture"
	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/driver/mock"
	"github.com/devicelab-dev/maestro-runner/pkg/report"
)

// stalledStream opens path on base (http://host:port) over a raw
// connection with headers, reads the status line and then never reads
// again: the client of Review 2.harness-review.1, whose connection stays
// open while its socket buffers fill. The connection closes with the test.
func stalledStream(t *testing.T, base, path string, headers map[string]string) {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	_ = conn.(*net.TCPConn).SetReadBuffer(4 << 10)
	req := "GET " + path + " HTTP/1.1\r\nHost: test\r\n"
	for k, v := range headers {
		req += k + ": " + v + "\r\n"
	}
	if _, err := io.WriteString(conn, req+"\r\n"); err != nil {
		t.Fatal(err)
	}
	status := make([]byte, 12)
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(conn, status); err != nil || string(status) != "HTTP/1.1 200" {
		t.Fatalf("status %q: %v", status, err)
	}
}

// Review 2.harness-review.1: under a hub lease, a stalled client's write
// held the lease gate, so the lease's teardown waited on it and the device
// stayed "being released". The write now fails, the work under the lease
// unwinds, and the device is free again. The release comes while the
// write is blocked (its socket buffers fill in a few milliseconds), before
// the write's deadline.
func TestLeases_StalledStreamFreesTheDevice(t *testing.T) {

	env := newHubEnv(t, daemon.Config{})
	env.s.deps.NewDriver = func(ctx context.Context, id string, cfg daemon.AttachConfig) (core.Driver, daemon.DriverInfo, func(), error) {
		sess := capture.NewSession(daemontest.BurstSource{})
		return &daemontest.CaptureDriver{Driver: mock.New(mock.Config{Platform: "mock", DeviceID: id}), Session: sess},
			daemon.DriverInfo{DriverName: "mock", Device: report.Device{ID: id}, Platform: "mock", Kind: "device"}, sess.Close, nil
	}
	res := env.lease(env.remote, daemon.LeaseRequest{Device: "mock-1", Attach: &daemon.AttachConfig{Platform: "mock"}})
	stalledStream(t, env.remote.BaseURL(), "/v1/devices/mock-1/stream",
		map[string]string{"Authorization": "Bearer t", daemon.HeaderLease: res.Lease.ID})
	time.Sleep(500 * time.Millisecond)
	if _, err := env.remote.ReleaseLease(context.Background(), res.Lease.ID); err != nil {
		t.Fatal(err)
	}
	waitStalled(t, "the device to be released", func() bool { return !env.s.leases.releasing(daemon.DeviceInfo{ID: "mock-1"}) })
	env.lease(env.local, daemon.LeaseRequest{Device: "mock-1"})
}

// waitStalled waits up to 20 s for cond: a machine loaded by the rest of
// the suite (-race, coverage) can take a while, while a write that never
// fails would never meet it. It fails with every goroutine's stack.
func waitStalled(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			t.Fatalf("still waiting for %s after 20 s\n%s", what, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(10 * time.Millisecond)
	}
}
