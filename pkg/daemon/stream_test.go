package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/capture"
	"github.com/devicelab-dev/maestro-runner/pkg/capture/fake"
	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/driver/mock"
	"github.com/devicelab-dev/maestro-runner/pkg/report"
)

// stepSource is a capture source a test drives: Run emits the clip's
// config, then one packet per value sent on next (a keyframe when true),
// and fails with the error sent on fail.
type stepSource struct {
	fpsControl bool
	next       chan bool
	fail       chan error
	started    atomic.Int32
	silent     bool // emit nothing at all
	tiny       bool // emit 4-byte samples
}

func newStepSource() *stepSource {
	return &stepSource{fpsControl: true, next: make(chan bool, 64), fail: make(chan error, 1)}
}

func (s *stepSource) Mode() capture.Mode { return capture.OnDemand }
func (s *stepSource) FPSControl() bool   { return s.fpsControl }

func (s *stepSource) Run(ctx context.Context, opts capture.Options, emit func(capture.Packet)) error {
	s.started.Add(1)
	format, frames, err := fake.ParseClip(fake.Clip())
	if err != nil {
		return err
	}
	if !s.silent {
		emit(capture.Packet{Kind: capture.VideoConfig, Video: format})
	}
	var pts time.Duration
	for i := 0; ; i++ {
		select {
		case <-ctx.Done():
			return nil
		case err := <-s.fail:
			return err
		case key := <-s.next:
			f := frames[i%len(frames)]
			if key {
				f = frames[0]
			}
			data := f.Data
			if s.tiny {
				data = []byte{0, 0, 0, 1, 0x65}[:5]
			}
			emit(capture.Packet{Kind: capture.Video, PTS: pts, Key: key || f.Key, Data: data})
			pts += time.Second / 15
		}
	}
}

// burstSource emits the config and a keyframe, then large packets as fast
// as it can until ctx ends: a subscriber that does not read falls behind.
type burstSource struct{}

func (burstSource) Mode() capture.Mode { return capture.OnDemand }
func (burstSource) FPSControl() bool   { return true }
func (burstSource) Run(ctx context.Context, _ capture.Options, emit func(capture.Packet)) error {
	format, frames, err := fake.ParseClip(fake.Clip())
	if err != nil {
		return err
	}
	emit(capture.Packet{Kind: capture.VideoConfig, Video: format})
	big := append(append([]byte(nil), frames[0].Data...), make([]byte, 16<<10)...)
	for i := 0; ctx.Err() == nil; i++ {
		emit(capture.Packet{Kind: capture.Video, PTS: time.Duration(i) * time.Millisecond, Key: true, Data: big})
		if i%256 == 255 {
			time.Sleep(time.Millisecond)
		}
	}
	return nil
}

// capDriver is the mock driver with the capture a test gives it.
type capDriver struct {
	*mock.Driver
	sess *capture.Session
	err  error
}

func (d *capDriver) Capture() (*capture.Session, error) { return d.sess, d.err }

// plainDriver hides every optional interface of the mock: no capture.
type plainDriver struct{ core.Driver }

type streamEnv struct {
	t       *testing.T
	s       *Server
	c       *Client
	url     string
	mu      sync.Mutex
	sources map[string]capture.Source
}

func newStreamEnv(t *testing.T) *streamEnv {
	t.Helper()
	t.Setenv(EnvHome, t.TempDir())
	env := &streamEnv{t: t, sources: map[string]capture.Source{}}
	var n int32
	deps := testDeps(&n, 0)
	deps.NewDriver = func(ctx context.Context, id string, cfg AttachConfig) (core.Driver, DriverInfo, func(), error) {
		info := DriverInfo{DriverName: "mock", Device: report.Device{ID: id, Platform: "mock"}, Platform: "mock", Kind: "device"}
		m := mock.New(mock.Config{Platform: "mock", DeviceID: id})
		switch {
		case strings.HasPrefix(id, "plain-"):
			info.DriverName = "plain"
			return plainDriver{m}, info, func() {}, nil
		case strings.HasPrefix(id, "perm-"):
			return &capDriver{Driver: m, err: &capture.Unavailable{Reason: capture.ReasonCameraPermission, Message: "denied"}}, info, func() {}, nil
		case strings.HasPrefix(id, "ctl-"):
			env.mu.Lock()
			src := env.sources[id]
			env.mu.Unlock()
			sess := capture.NewSession(src)
			return &capDriver{Driver: m, sess: sess}, info, sess.Close, nil
		}
		return m, info, m.Close, nil
	}
	env.s = New(Config{Name: "stream-" + strings.ToLower(t.Name()), ReportsDir: filepath.Join(t.TempDir(), "reports")}, deps)
	env.s.info = Info{Name: env.s.cfg.Name, PID: os.Getpid(), APIVersion: APIVersion, Version: "test"}
	hs := httptest.NewServer(env.s.Handler())
	t.Cleanup(hs.Close)
	t.Cleanup(env.s.detachAll)
	env.url = hs.URL
	env.c = NewHTTPClient(hs.URL, "", nil)
	return env
}

func (e *streamEnv) source(id string, src capture.Source) {
	e.mu.Lock()
	e.sources[id] = src
	e.mu.Unlock()
}

func (e *streamEnv) attach(id string) {
	e.t.Helper()
	if _, err := e.c.Attach(context.Background(), id, AttachConfig{Platform: "mock"}); err != nil {
		e.t.Fatalf("attach %s: %v", id, err)
	}
}

func (e *streamEnv) get(ctx context.Context, path string) *http.Response {
	e.t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, e.url+path, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

// failure decodes an error answer.
func failure(t *testing.T, resp *http.Response) (int, ErrorBody) {
	t.Helper()
	defer resp.Body.Close()
	var env Envelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil || env.Error == nil {
		t.Fatalf("HTTP %d: not an error envelope (%v)", resp.StatusCode, err)
	}
	if got := resp.Header.Get(HeaderErrorCode); got != string(env.Error.Code) {
		t.Fatalf("%s header %q, body code %s", HeaderErrorCode, got, env.Error.Code)
	}
	return resp.StatusCode, *env.Error
}

func (e *streamEnv) holds() int {
	e.s.idle.mu.Lock()
	defer e.s.idle.mu.Unlock()
	return e.s.idle.holds
}

// U13
func TestStream_Refusals(t *testing.T) {
	env := newStreamEnv(t)
	ctx := context.Background()
	if st, e := failure(t, env.get(ctx, "/v1/devices/mock-1/stream")); st != 404 || e.Code != CodeDeviceNotAttached {
		t.Fatalf("unattached: %d %+v", st, e)
	}
	env.attach("mock-1")
	for _, q := range []string{"fps=2", "fps=61", "fps=-1", "fps=x", "fps=15.5", "audio=maybe"} {
		st, e := failure(t, env.get(ctx, "/v1/devices/mock-1/stream?"+q))
		if st != 400 || e.Code != CodeUsage {
			t.Fatalf("%s: %d %+v", q, st, e)
		}
		if strings.HasPrefix(q, "fps") && !strings.Contains(e.Message, "3 to 60") {
			t.Fatalf("%s: %q does not name the range", q, e.Message)
		}
	}
	env.attach("plain-1")
	st, e := failure(t, env.get(ctx, "/v1/devices/plain-1/stream"))
	if st != 503 || e.Code != CodeCaptureUnavailable || e.Details["reason"] != capture.ReasonUnsupportedPlatform || !strings.Contains(e.Message, "unsupported-platform") {
		t.Fatalf("no capture: %d %+v", st, e)
	}
	env.attach("perm-1")
	st, e = failure(t, env.get(ctx, "/v1/devices/perm-1/stream"))
	if st != 503 || e.Code != CodeCaptureUnavailable || e.Details["reason"] != capture.ReasonCameraPermission {
		t.Fatalf("camera permission: %d %+v", st, e)
	}
	// The Go client reports the same.
	_, err := env.c.Stream(ctx, "perm-1", StreamOptions{}, io.Discard)
	var de *Error
	if !errors.As(err, &de) || de.Code != CodeCaptureUnavailable || de.Details["reason"] != capture.ReasonCameraPermission || de.ExitCode() != 4 {
		t.Fatalf("client: %v", err)
	}
	// A failed source before its first frames: CAPTURE_UNAVAILABLE with
	// the source's reason.
	src := newStepSource()
	src.fail <- &capture.Unavailable{Reason: capture.ReasonScrcpyFailed, Message: "server died"}
	src.silent = true
	env.source("ctl-fail", src)
	env.attach("ctl-fail")
	if st, e := failure(t, env.get(ctx, "/v1/devices/ctl-fail/stream")); st != 503 || e.Details["reason"] != capture.ReasonScrcpyFailed {
		t.Fatalf("failed source: %d %+v", st, e)
	}
	if env.holds() != 0 {
		t.Fatalf("idle holds left: %d", env.holds())
	}
}

// U14
func TestStream_HeadersAndFlush(t *testing.T) {
	env := newStreamEnv(t)
	src := newStepSource()
	env.source("ctl-1", src)
	env.attach("ctl-1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src.next <- true
	resp := env.get(ctx, "/v1/devices/ctl-1/stream?fps=20")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	h := resp.Header
	if h.Get("Content-Type") != "video/mp4" || h.Get("Cache-Control") != "no-store" || h.Get("X-Content-Type-Options") != "nosniff" ||
		h.Get(HeaderStreamFPS) != "20" || h.Get(HeaderStreamAudio) != "false" || !strings.HasPrefix(h.Get(HeaderStreamCodecs), "avc3.") {
		t.Fatalf("headers: %v", h)
	}
	if h.Get("Content-Length") != "" || resp.ContentLength != -1 || len(resp.TransferEncoding) == 0 || resp.TransferEncoding[0] != "chunked" {
		t.Fatalf("not chunked: length %d %v", resp.ContentLength, resp.TransferEncoding)
	}
	// The init segment and the first fragment arrive before the source
	// produces anything more: each write is flushed.
	got := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 16<<10)
		var all []byte
		for !bytes.Contains(all, []byte("mdat")) {
			n, err := resp.Body.Read(buf)
			all = append(all, buf[:n]...)
			if err != nil {
				break
			}
		}
		got <- all
	}()
	select {
	case b := <-got:
		if len(b) < 8 || string(b[4:8]) != "ftyp" || !bytes.Contains(b, []byte("moov")) || !bytes.Contains(b, []byte("moof")) {
			t.Fatalf("first bytes: % x", b[:min(len(b), 32)])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the init segment was not flushed")
	}
	cancel()

	// A source that picks its own rate says so.
	dev := newStepSource()
	dev.fpsControl = false
	env.source("ctl-2", dev)
	env.attach("ctl-2")
	dev.next <- true
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	resp2 := env.get(ctx2, "/v1/devices/ctl-2/stream")
	defer resp2.Body.Close()
	if resp2.Header.Get(HeaderStreamFPS) != "device" {
		t.Fatalf("fps header %q", resp2.Header.Get(HeaderStreamFPS))
	}
}

// openStream starts a stream on id and waits for its first fragment.
func (e *streamEnv) openStream(ctx context.Context, id string, src *stepSource) *http.Response {
	e.t.Helper()
	src.next <- true
	resp := e.get(ctx, "/v1/devices/"+id+"/stream")
	if resp.StatusCode != 200 {
		e.t.Fatalf("stream %s: %d", id, resp.StatusCode)
	}
	return resp
}

// drained reports whether body ends (EOF or an error) within 5 s.
func drained(body io.Reader) bool { return drainedWithin(body, 5*time.Second) }

// drainedWithin is drained with its own limit: a lagging stream first
// delivers everything already queued for it, which takes a while under -race
// on a busy machine.
func drainedWithin(body io.Reader, limit time.Duration) bool {
	done := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, body); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(limit):
		return false
	}
}

// U15
func TestStream_EndConditions(t *testing.T) {
	env := newStreamEnv(t)

	// A step runs while a stream is open: the stream never takes the
	// device's busy lock.
	src := newStepSource()
	env.source("ctl-busy", src)
	env.attach("ctl-busy")
	ctx, cancel := context.WithCancel(context.Background())
	resp := env.openStream(ctx, "ctl-busy", src)
	if env.holds() != 1 {
		t.Fatalf("an open stream holds the idle timer once, got %d", env.holds())
	}
	a := env.s.getAttached("ctl-busy")
	if !a.busy.TryLock() {
		t.Fatal("the stream holds the busy lock")
	}
	a.busy.Unlock()
	if _, err := env.c.Command(context.Background(), "ctl-busy", "tapOn", "X", false); err != nil {
		t.Fatalf("a step beside the stream: %v", err)
	}
	// The client leaves: the handler ends and releases its hold.
	cancel()
	resp.Body.Close()
	waitFor(t, func() bool { return env.holds() == 0 })

	// Detach ends the body.
	resp = env.openStream(context.Background(), "ctl-busy", src)
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = env.c.Detach(context.Background(), "ctl-busy")
	}()
	if !drained(resp.Body) {
		t.Fatal("detach did not end the stream")
	}
	resp.Body.Close()
	waitFor(t, func() bool { return env.holds() == 0 })

	// The source failing ends the body.
	fail := newStepSource()
	env.source("ctl-src", fail)
	env.attach("ctl-src")
	resp = env.openStream(context.Background(), "ctl-src", fail)
	fail.fail <- errors.New("boom")
	if !drained(resp.Body) {
		t.Fatal("a source failure did not end the stream")
	}
	resp.Body.Close()

	// A client that stops reading falls behind and its stream ends.
	env.source("ctl-lag", burstSource{})
	env.attach("ctl-lag")
	resp = env.get(context.Background(), "/v1/devices/ctl-lag/stream")
	time.Sleep(500 * time.Millisecond)
	if !drainedWithin(resp.Body, 30*time.Second) {
		t.Fatal("a lagging client's stream did not end")
	}
	resp.Body.Close()
	waitFor(t, func() bool { return env.holds() == 0 })

	// Shutdown ends a running stream.
	sd := newStepSource()
	env.source("ctl-sd", sd)
	env.attach("ctl-sd")
	resp = env.openStream(context.Background(), "ctl-sd", sd)
	go env.s.Shutdown("test")
	if !drained(resp.Body) {
		t.Fatal("shutdown did not end the stream")
	}
	resp.Body.Close()
	<-env.s.Done()
	waitFor(t, func() bool { return env.holds() == 0 })
}

// U16
func TestStream_NoFrames(t *testing.T) {
	old := streamFirstFrameWait
	streamFirstFrameWait = 200 * time.Millisecond
	defer func() { streamFirstFrameWait = old }()
	env := newStreamEnv(t)
	src := newStepSource()
	src.silent = true
	env.source("ctl-quiet", src)
	env.attach("ctl-quiet")
	st, e := failure(t, env.get(context.Background(), "/v1/devices/ctl-quiet/stream"))
	if st != 503 || e.Code != CodeCaptureUnavailable || e.Details["reason"] != capture.ReasonNoFrames {
		t.Fatalf("no frames: %d %+v", st, e)
	}
	// A detach while it waits answers DEVICE_NOT_ATTACHED.
	streamFirstFrameWait = 5 * time.Second
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = env.c.Detach(context.Background(), "ctl-quiet")
	}()
	if st, e := failure(t, env.get(context.Background(), "/v1/devices/ctl-quiet/stream")); st != 404 || e.Code != CodeDeviceNotAttached {
		t.Fatalf("detached while waiting: %d %+v", st, e)
	}
}

// U24
func TestStream_CaptureEvents(t *testing.T) {
	env := newStreamEnv(t)
	src := newStepSource()
	env.source("ctl-ev", src)
	env.attach("ctl-ev")
	env.attach("perm-ev")
	env.attach("plain-ev")
	resp := env.openStream(context.Background(), "ctl-ev", src)
	src.fail <- &capture.Unavailable{Reason: capture.ReasonScrcpyFailed}
	drained(resp.Body)
	resp.Body.Close()
	var got []string
	waitFor(t, func() bool {
		got = nil
		for _, ev := range env.s.events.Recent("", 0) {
			if ev.Type == "device.capture" {
				got = append(got, ev.Device+":"+ev.Message+":"+ev.Error)
			}
		}
		return len(got) >= 3
	})
	want := []string{"perm-ev:unavailable:camera-permission", "ctl-ev:capturing:", "ctl-ev:unavailable:scrcpy-failed"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("device.capture events %v, want %v", got, want)
	}
	// Detaching stops the events: the next session change is not published.
	_ = env.c.Detach(context.Background(), "ctl-ev")
	n := len(got)
	time.Sleep(50 * time.Millisecond)
	for _, ev := range env.s.events.Recent("", 0) {
		if ev.Type == "device.capture" {
			n--
		}
	}
	if n != 0 {
		t.Fatal("device.capture published after detach")
	}
}

// The mock driver's own capture streams through the route with audio.
func TestStream_MockDriverAudio(t *testing.T) {
	env := newStreamEnv(t)
	env.attach("mock-a")
	var buf bytes.Buffer
	var started StreamInfo
	info, err := env.c.Stream(context.Background(), "mock-a", StreamOptions{FPS: 30, Audio: true, Duration: 1500 * time.Millisecond,
		OnStart: func(i StreamInfo) { started = i }}, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if info != started || info.FPS != "30" || !info.Audio || !strings.Contains(info.Codecs, "mp4a.40.2") {
		t.Fatalf("info %+v started %+v", info, started)
	}
	if buf.Len() < 1000 || string(buf.Bytes()[4:8]) != "ftyp" {
		t.Fatalf("body: %d bytes", buf.Len())
	}
}

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

// slowWrites makes a stream's write give up after d for the test.
func slowWrites(t *testing.T, d time.Duration) {
	old := streamWriteTimeout
	streamWriteTimeout = d
	t.Cleanup(func() { streamWriteTimeout = old })
}

// Review 2.harness-review.1: a client that keeps its connection open but
// stops reading cannot hold the handler: a write that does not drain
// within streamWriteTimeout fails, the stream ends, and its idle hold is
// released, while the connection is still open.
func TestStream_StalledClientEnds(t *testing.T) {
	slowWrites(t, 200*time.Millisecond)
	env := newStreamEnv(t)
	env.source("ctl-stall", burstSource{})
	env.attach("ctl-stall")
	stalledStream(t, env.url, "/v1/devices/ctl-stall/stream", nil)
	waitStalled(t, "the stream to end", func() bool { return env.holds() == 0 })
	if writeFailure(os.ErrDeadlineExceeded) != "client too slow: a write did not drain within 200ms" ||
		writeFailure(io.ErrClosedPipe) != "write: io: read/write on closed pipe" {
		t.Error("writeFailure reasons")
	}
}

// Review 2.harness-review.1: under a hub lease, a stalled client's write
// held the lease gate, so the lease's teardown waited on it and the device
// stayed "being released". The write now fails, the work under the lease
// unwinds, and the device is free again. The release comes while the
// write is blocked (its socket buffers fill in a few milliseconds), before
// the write's deadline.
func TestLeases_StalledStreamFreesTheDevice(t *testing.T) {
	slowWrites(t, 1500*time.Millisecond)
	env := newHubEnv(t, Config{})
	env.s.deps.NewDriver = func(ctx context.Context, id string, cfg AttachConfig) (core.Driver, DriverInfo, func(), error) {
		sess := capture.NewSession(burstSource{})
		return &capDriver{Driver: mock.New(mock.Config{Platform: "mock", DeviceID: id}), sess: sess},
			DriverInfo{DriverName: "mock", Device: report.Device{ID: id}, Platform: "mock", Kind: "device"}, sess.Close, nil
	}
	res := env.lease(env.remote, LeaseRequest{Device: "mock-1", Attach: &AttachConfig{Platform: "mock"}})
	stalledStream(t, env.remote.BaseURL(), "/v1/devices/mock-1/stream",
		map[string]string{"Authorization": "Bearer t", HeaderLease: res.Lease.ID})
	time.Sleep(500 * time.Millisecond)
	if _, err := env.remote.ReleaseLease(context.Background(), res.Lease.ID); err != nil {
		t.Fatal(err)
	}
	waitStalled(t, "the device to be released", func() bool { return !env.s.leases.releasing(DeviceInfo{ID: "mock-1"}) })
	env.lease(env.local, LeaseRequest{Device: "mock-1"})
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
