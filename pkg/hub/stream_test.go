package hub

import (
	"context"
	"fmt"
	"io"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/capture"
	"github.com/devicelab-dev/maestro-runner/pkg/capture/fake"
	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/driver/mock"
)

// burstSource emits 1 MiB key frames 50 times a second, so a stream fills
// every buffer between it and a client that stops reading in moments, yet
// leaves a loaded machine's stream route seconds before its subscriber
// lags (512 packets).
type burstSource struct{}

func (burstSource) Mode() capture.Mode { return capture.OnDemand }
func (burstSource) FPSControl() bool   { return true }
func (burstSource) Run(ctx context.Context, _ capture.Options, emit func(capture.Packet)) error {
	format, frames, err := fake.ParseClip(fake.Clip())
	if err != nil {
		return err
	}
	emit(capture.Packet{Kind: capture.VideoConfig, Video: format})
	big := append(append([]byte(nil), frames[0].Data...), make([]byte, 1<<20)...)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for i := 0; ; i++ {
		emit(capture.Packet{Kind: capture.Video, PTS: time.Duration(i) * 20 * time.Millisecond, Key: true, Data: big})
		select {
		case <-tick.C:
		case <-ctx.Done():
			return nil
		}
	}
}

// capDriver is the mock driver with the capture a test gives it.
type capDriver struct {
	*mock.Driver
	sess *capture.Session
}

func (d *capDriver) Capture() (*capture.Session, error) { return d.sess, nil }

// goroutineIn reports whether one goroutine's stack holds every one of
// funcs, and returns every goroutine's stack.
func goroutineIn(funcs ...string) (bool, string) {
	buf := make([]byte, 4<<20)
	all := string(buf[:runtime.Stack(buf, true)])
	for _, g := range strings.Split(all, "\n\n") {
		found := true
		for _, f := range funcs {
			found = found && strings.Contains(g, f)
		}
		if found {
			return true, all
		}
	}
	return false, all
}

// waitStacks waits up to 30 s for cond, which reports whether it is met
// and every goroutine's stack; it fails with them.
func waitStacks(t *testing.T, what string, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		ok, stacks := cond()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("still waiting for %s after 30 s\n%s", what, stacks)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Review 3.harness-review.1: a client streams a peer's device through its
// hub and stops reading, its connection left open. The relaying hub's
// write to it fails at its deadline, the relay closes the bulk link, and
// the owner's stream route, which waited on the link's flow window, returns
// and lets the lease's release finish: the device is free again.
func TestMesh_StalledRelayedStreamFreesThePeersDevice(t *testing.T) {
	old := relayWriteTimeout
	relayWriteTimeout = 8 * time.Second
	t.Cleanup(func() { relayWriteTimeout = old })
	a, b := twoHubs(t)
	burst := func(id string) (core.Driver, func()) {
		sess := capture.NewSession(burstSource{})
		return &capDriver{Driver: mock.New(mock.Config{Platform: "mock", DeviceID: id}), sess: sess}, sess.Close
	}
	a.driver.Store(&burst)
	ctx := context.Background()
	c := b.client()
	lr, err := c.AcquireLease(ctx, daemon.LeaseRequest{Device: "a1@hub-a", Attach: &daemon.AttachConfig{Platform: "mock"}})
	if err != nil {
		t.Fatal(err)
	}

	// The client of hub b: it reads the status line, then never again.
	conn, err := net.Dial("tcp", b.addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	_ = conn.(*net.TCPConn).SetReadBuffer(4 << 10)
	if _, err := io.WriteString(conn, "GET /v1/devices/a1@hub-a/stream HTTP/1.1\r\nHost: test\r\nAuthorization: Bearer "+testToken+
		"\r\n"+daemon.HeaderLease+": "+lr.Lease.ID+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	status := make([]byte, 12)
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(conn, status); err != nil || string(status) != "HTTP/1.1 200" {
		t.Fatalf("status %q: %v", status, err)
	}

	// The owner's write waits on the link's flow window, which the
	// stalled relay no longer credits; the lease is released meanwhile.
	waitStacks(t, "the owner's stream to wait on the bulk link", func() (bool, string) {
		return goroutineIn("daemon.(*Server).handleStream", "hub.(*credits).acquire")
	})
	if _, err := c.ReleaseLease(ctx, lr.Lease.ID); err != nil {
		t.Fatal(err)
	}
	owner := a.client()
	waitStacks(t, "the owner's stream to end and its device to be free", func() (bool, string) {
		busy, stacks := goroutineIn("daemon.(*Server).handleStream")
		if busy {
			return false, stacks
		}
		_, err := owner.AcquireLease(ctx, daemon.LeaseRequest{Device: "a1"})
		return err == nil, fmt.Sprintf("lease: %v\n%s", err, stacks)
	})
	if b.logged("relayed stream /v1/devices/a1@hub-a/stream from hub-a ended") != 1 {
		t.Fatal("hub b did not log the relayed stream's end")
	}
}
