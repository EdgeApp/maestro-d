package scrcpy

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/capture"
)

const fullArgs33 = "scid=0000002a log_level=info tunnel_forward=true control=false cleanup=false " +
	"video_codec=h264 max_fps=15 video_codec_options=i-frame-interval:int=1,repeat-previous-frame-after:long=100000 " +
	"audio=true audio_codec=aac audio_source=playback audio_dup=true " +
	"send_device_meta=false send_stream_meta=true send_frame_meta=true"

// U30: the server's options by SDK: Android 13+ always captures playback
// audio and keeps the speaker on; 11–12 captures audio only when the first
// subscriber asked; older Androids capture none.
func TestServerArgsAndAudioPlan(t *testing.T) {
	s34 := New(&fakeDevice{}, Config{SDK: 34})
	if got := s34.serverArgs(42, 15, s34.PlansAudio(capture.Options{})); got != fullArgs33 {
		t.Errorf("SDK 34:\n got %s\nwant %s", got, fullArgs33)
	}
	if !New(&fakeDevice{}, Config{}).PlansAudio(capture.Options{}) {
		t.Error("unknown SDK does not plan audio")
	}
	s31 := New(&fakeDevice{}, Config{SDK: 31})
	if !s31.PlansAudio(capture.Options{Audio: true}) || s31.PlansAudio(capture.Options{}) {
		t.Error("SDK 31 audio plan does not follow the first subscriber")
	}
	want31 := "audio=true audio_codec=aac send_device_meta=false"
	if got := s31.serverArgs(1, 60, true); !strings.Contains(got, want31) || !strings.Contains(got, "max_fps=60") || !strings.HasPrefix(got, "scid=00000001 ") {
		t.Errorf("SDK 31 with audio: %s", got)
	}
	if got := s31.serverArgs(1, 3, false); !strings.Contains(got, "audio=false audio_codec=aac send_device_meta=false") {
		t.Errorf("SDK 31 without audio: %s", got)
	}
	// Q10: repeated frames come no faster than the requested rate: 1/fps
	// below 10 fps, scrcpy's own 100 ms above it.
	for fps, want := range map[int]string{3: "333333", 5: "200000", 10: "100000", 15: "100000", 60: "100000"} {
		opt := "video_codec_options=i-frame-interval:int=1,repeat-previous-frame-after:long=" + want + " "
		if got := s34.serverArgs(1, fps, true); !strings.Contains(got, "max_fps="+strconv.Itoa(fps)+" "+opt) {
			t.Errorf("fps %d: %s, want %s", fps, got, opt)
		}
	}
	if repeatFrameAfter(0) != 100_000 {
		t.Error("fps 0 repeat interval")
	}
	s29 := New(&fakeDevice{}, Config{SDK: 29})
	if s29.PlansAudio(capture.Options{Audio: true}) {
		t.Error("SDK 29 plans audio")
	}
	if !strings.Contains(s29.serverArgs(1, 15, false), "audio=false audio_codec=aac send_device_meta=false") {
		t.Error("SDK 29 args")
	}
	if s34.Mode() != capture.OnDemand || !s34.FPSControl() {
		t.Error("mode or fps control")
	}
	if !strings.HasSuffix(New(&fakeDevice{}, Config{}).server, filepath.Join("drivers", "android", ServerFile)) {
		t.Error("default server path")
	}
	for i := 0; i < 50; i++ {
		if randomSCID() > 0x7fffffff {
			t.Fatal("scid wider than 31 bits")
		}
	}
}

// U30: on a no-audio Android 11–12 run, a later subscriber asking for
// audio gets Audio false through the Session.
func TestSession_LaterAudioSubscriberOnSDK31(t *testing.T) {
	dev := &fakeDevice{serial: "S31", serve: serveStreams(syntheticVideo(t, 5), nil, true)}
	srv := useFakeServer(t, "wait")
	src, _ := newTestSource(t, dev, 31)
	sess := capture.NewSession(src)
	defer sess.Close()
	first, err := sess.Subscribe(capture.Options{FPS: 15})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	later, err := sess.Subscribe(capture.Options{FPS: 60, Audio: true})
	if err != nil {
		t.Fatal(err)
	}
	defer later.Close()
	if later.Options().Audio || later.Options().FPS != 15 {
		t.Errorf("later subscriber options %+v", later.Options())
	}
	waitFor(t, "the server start", func() bool { return len(srv.calls()) == 1 })
	if argv := strings.Join(srv.calls()[0], " "); !strings.Contains(argv, "audio=false") {
		t.Errorf("argv %s", argv)
	}
}

// U30: the jar is checked against the pinned sha256, pushed only when the
// device's copy differs, and not asked about again for that device.
func TestEnsureServer(t *testing.T) {
	dev := &fakeDevice{serial: "A", shellOut: "deadbeef  " + RemoteJar + "\n"}
	src, _ := newTestSource(t, dev, 34)
	if err := src.ensureServer(); err != nil {
		t.Fatal(err)
	}
	if len(dev.pushes) != 1 || dev.pushes[0] != serverJar+" -> "+RemoteJar {
		t.Errorf("pushes %v", dev.pushes)
	}
	if len(dev.shells) != 1 || dev.shells[0] != "sha256sum "+RemoteJar+" 2>/dev/null" {
		t.Errorf("shells %v", dev.shells)
	}
	if err := src.ensureServer(); err != nil || len(dev.shells) != 1 || len(dev.pushes) != 1 {
		t.Errorf("second check asked the device again: %v %v %v", err, dev.shells, dev.pushes)
	}

	same := &fakeDevice{serial: "B", shellOut: ServerSHA256 + "  " + RemoteJar + "\n"}
	src, _ = newTestSource(t, same, 34)
	if err := src.ensureServer(); err != nil || len(same.pushes) != 0 {
		t.Errorf("matching device pushed: %v %v", err, same.pushes)
	}

	// No sha256sum on the device: push.
	old := &fakeDevice{serial: "C", shellErr: errors.New("sha256sum: not found")}
	src, _ = newTestSource(t, old, 34)
	if err := src.ensureServer(); err != nil || len(old.pushes) != 1 {
		t.Errorf("device without sha256sum: %v %v", err, old.pushes)
	}

	failing := &fakeDevice{serial: "D", pushErr: errors.New("no space")}
	src, _ = newTestSource(t, failing, 34)
	if err := src.ensureServer(); capture.ReasonOf(err, "") != capture.ReasonScrcpyFailed || !strings.Contains(err.Error(), "no space") {
		t.Errorf("push failure: %v", err)
	}

	wrong := filepath.Join(t.TempDir(), ServerFile)
	if err := os.WriteFile(wrong, []byte("not the jar"), 0o644); err != nil {
		t.Fatal(err)
	}
	src = New(&fakeDevice{serial: "E"}, Config{SDK: 34, Server: wrong})
	if err := src.ensureServer(); capture.ReasonOf(err, "") != capture.ReasonScrcpyFailed || !strings.Contains(err.Error(), "want "+ServerSHA256) {
		t.Errorf("sha mismatch: %v", err)
	}
	src = New(&fakeDevice{serial: "F"}, Config{SDK: 34, Server: filepath.Join(t.TempDir(), "missing")})
	if err := src.Run(context.Background(), capture.Options{}, func(capture.Packet) {}); capture.ReasonOf(err, "") != capture.ReasonScrcpyFailed {
		t.Errorf("missing jar: %v", err)
	}
}

// slowDevice is a fakeDevice whose sha256sum blocks until release closes
// (a wireless-adb device, or one busy with a first push).
type slowDevice struct {
	*fakeDevice
	entered chan struct{}
	release chan struct{}
}

func (d *slowDevice) Shell(cmd string) (string, error) {
	d.entered <- struct{}{}
	<-d.release
	return d.fakeDevice.Shell(cmd)
}

// Review 2.harness-review.8: the jar check and push lock their device
// only. A slow device does not hold up another device's capture start,
// while a second start on the slow device waits for the first and then
// asks nothing again.
func TestEnsureServer_PerDeviceLock(t *testing.T) {
	resetPushed(t)
	slow := &slowDevice{fakeDevice: &fakeDevice{serial: "SLOW", shellOut: ServerSHA256 + "  " + RemoteJar + "\n"},
		entered: make(chan struct{}, 2), release: make(chan struct{})}
	fast := &fakeDevice{serial: "FAST", shellOut: ServerSHA256 + "  " + RemoteJar + "\n"}
	slowSrc := New(slow, Config{SDK: 34, Server: serverJar})
	done := make(chan error, 2)
	go func() { done <- slowSrc.ensureServer() }()
	<-slow.entered
	go func() { done <- slowSrc.ensureServer() }() // the same device: waits

	fastDone := make(chan error, 1)
	go func() { fastDone <- New(fast, Config{SDK: 34, Server: serverJar}).ensureServer() }()
	select {
	case err := <-fastDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a slow device's check held up another device")
	}
	select {
	case <-slow.entered:
		t.Fatal("two checks ran at once on one device")
	case err := <-done:
		t.Fatalf("a start on the slow device returned before its check: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(slow.release)
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if len(slow.shells) != 1 {
		t.Errorf("the slow device was asked %d times, want once", len(slow.shells))
	}
	// forgetServer makes the next start ask again.
	slowSrc.forgetServer()
	slow.release = make(chan struct{})
	close(slow.release)
	if err := slowSrc.ensureServer(); err != nil || len(slow.shells) != 2 {
		t.Errorf("after forgetServer: %v, %d checks", err, len(slow.shells))
	}
}

// jarDevice is a fakeDevice whose jar can disappear: sha256sum answers the
// pinned sum only while the device holds the jar, and a push puts it back.
type jarDevice struct {
	*fakeDevice
	has atomic.Bool
}

func (d *jarDevice) Shell(cmd string) (string, error) {
	_, _ = d.fakeDevice.Shell(cmd)
	if d.has.Load() {
		return ServerSHA256 + "  " + RemoteJar + "\n", nil
	}
	return "", errors.New("exit status 1")
}

func (d *jarDevice) Push(local, remote string) error {
	err := d.fakeDevice.Push(local, remote)
	d.has.Store(true)
	return err
}

// Review 1.harness-review.1: a run that fails forgets that the device
// holds the jar. The jar is lost between two runs (the emulator wiped);
// the run that follows fails as app_process does without it, and the next
// run checks the device again, pushes, and streams.
func TestRun_FailedRunRechecksTheJar(t *testing.T) {
	dev := &jarDevice{fakeDevice: &fakeDevice{serial: "emulator-5554"}}
	dev.has.Store(true)
	dev.serve = func(i int, c net.Conn) {
		if !dev.has.Load() {
			c.Close()
			return
		}
		serveStreams(syntheticVideo(t, 5), nil, true)(i, c)
	}
	prev := commandContext
	commandContext = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		if !dev.has.Load() {
			return exec.CommandContext(ctx, "/bin/sh", "-c",
				`echo "java.lang.ClassNotFoundException: com.genymobile.scrcpy.Server" >&2; exit 1`)
		}
		return exec.CommandContext(ctx, "/bin/sh", "-c", "sleep 30")
	}
	t.Cleanup(func() { commandContext = prev })
	src, _ := newTestSource(t, dev, 29)

	stream := func() error {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var c collector
		done := make(chan error, 1)
		go func() { done <- src.Run(ctx, capture.Options{}, c.emit) }()
		deadline := time.After(30 * time.Second)
		for c.count(capture.Video) < 5 {
			select {
			case err := <-done:
				return err
			case <-deadline:
				t.Fatal("no video")
			case <-time.After(10 * time.Millisecond):
			}
		}
		cancel()
		return <-done
	}
	if err := stream(); err != nil {
		t.Fatalf("first run: %v", err)
	}
	dev.has.Store(false)
	if err := stream(); capture.ReasonOf(err, "") != capture.ReasonScrcpyFailed || !strings.Contains(err.Error(), "ClassNotFoundException") {
		t.Fatalf("run without the jar: %v", err)
	}
	if err := stream(); err != nil {
		t.Fatalf("run after the failure: %v", err)
	}
	dev.mu.Lock()
	defer dev.mu.Unlock()
	if len(dev.shells) != 2 || len(dev.pushes) != 1 {
		t.Errorf("shells %v, pushes %v: want the jar checked on the first run and again after the failure, and pushed once", dev.shells, dev.pushes)
	}
}

// U30, U31: a full run: the forward, the adb shell argv, the connect loop
// through connections the forward closes before the server listens, the
// video and audio packets, and on cancel the killed shell process group
// and the removed forward.
func TestRun_StreamsAndStops(t *testing.T) {
	video, audio := syntheticVideo(t, 30), syntheticAudio(20)
	serve := serveStreams(video, audio, true)
	dev := &fakeDevice{serial: "PIXEL", shellOut: ServerSHA256}
	dev.serve = func(i int, c net.Conn) {
		if i < 3 { // the server is not listening yet
			c.Close()
			return
		}
		serve(i-3, c)
	}
	srv := useFakeServer(t, `echo "[server] INFO: Device: [Google] Pixel 8"; printf 'partial'; wait`)
	src, logs := newTestSource(t, dev, 34)
	ctx, cancel := context.WithCancel(context.Background())
	var c collector
	done := make(chan error, 1)
	go func() { done <- src.Run(ctx, capture.Options{FPS: 15}, c.emit) }()
	waitFor(t, "all packets", func() bool { return c.count(capture.Video) == 30 && c.count(capture.Audio) == 20 })
	if c.count(capture.VideoConfig) != 1 || c.count(capture.AudioConfig) != 1 {
		t.Errorf("%d video configs, %d audio configs", c.count(capture.VideoConfig), c.count(capture.AudioConfig))
	}
	argv := srv.calls()[0]
	if len(argv) != 5 || argv[0] != "/fake/adb" || strings.Join(argv[1:4], " ") != "-s PIXEL shell" ||
		!strings.HasPrefix(argv[4], "CLASSPATH=/data/local/tmp/maestro-d-scrcpy-server.jar app_process / com.genymobile.scrcpy.Server 5.0.1 scid=") ||
		!strings.HasSuffix(argv[4], fullArgs33[len("scid=0000002a"):]) {
		t.Errorf("argv %q", argv)
	}
	scid := strings.Fields(argv[4])[5][len("scid="):]
	if len(dev.forwards) != 1 || !strings.HasSuffix(dev.forwards[0], " localabstract:scrcpy_"+scid) {
		t.Errorf("forwards %v (scid %s)", dev.forwards, scid)
	}
	waitFor(t, "the server's log line", func() bool {
		return strings.Contains(logs.text(), "[scrcpy PIXEL] [server] INFO: Device: [Google] Pixel 8")
	})
	pids := srv.pids(t)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after cancel: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if len(dev.removed) != 1 || !strings.HasPrefix(dev.forwards[0], "tcp:") {
		t.Errorf("forward not removed: %v", dev.removed)
	}
	for _, pid := range pids {
		waitFor(t, "the shell's process group to die", func() bool { return syscall.Kill(pid, 0) == syscall.ESRCH })
	}
	if !strings.Contains(logs.text(), "starting scrcpy-server 5.0.1 (max_fps=15 audio=true)") {
		t.Errorf("logs:\n%s", logs.text())
	}
}

// U31: a forward that never reaches a listening server fails after the
// connect timeout with scrcpy-failed; a server that exits before it listens
// fails at once with its last line.
func TestRun_ConnectFailures(t *testing.T) {
	defer func(d time.Duration) { connectTimeout = d }(connectTimeout)
	connectTimeout = 400 * time.Millisecond
	dev := &fakeDevice{serial: "X", shellOut: ServerSHA256, serve: func(_ int, c net.Conn) { c.Close() }}
	useFakeServer(t, "wait")
	src, _ := newTestSource(t, dev, 34)
	start := time.Now()
	err := src.Run(context.Background(), capture.Options{}, func(capture.Packet) {})
	if capture.ReasonOf(err, "") != capture.ReasonScrcpyFailed || !strings.Contains(err.Error(), "no connection within 400ms") {
		t.Fatalf("timeout: %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
	if len(dev.removed) != 1 {
		t.Error("forward not removed after a failed connect")
	}

	connectTimeout = 5 * time.Second
	useFakeServer(t, `echo "[server] ERROR: Could not open video stream" >&2; exit 1`)
	src, _ = newTestSource(t, dev, 34)
	start = time.Now()
	err = src.Run(context.Background(), capture.Options{}, func(capture.Packet) {})
	if capture.ReasonOf(err, "") != capture.ReasonScrcpyFailed || !strings.Contains(err.Error(), "exited before it listened") ||
		!strings.Contains(err.Error(), "Could not open video stream") {
		t.Fatalf("early exit: %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("early exit took %v", time.Since(start))
	}

	// Cancelled while connecting: nil, promptly.
	hold := &fakeDevice{serial: "Y", shellOut: ServerSHA256, serve: func(_ int, c net.Conn) {
		time.Sleep(3 * time.Second)
		c.Close()
	}}
	useFakeServer(t, "wait")
	src, _ = newTestSource(t, hold, 34)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start = time.Now()
	if err := src.Run(ctx, capture.Options{}, func(capture.Packet) {}); err != nil || time.Since(start) > 2*time.Second {
		t.Errorf("cancel while connecting: %v after %v", err, time.Since(start))
	}
}

// U32: below SDK 21 the source answers android-too-old; a stream that ends
// unexpectedly fails with scrcpy-failed and the server's last log line.
func TestRun_Failures(t *testing.T) {
	src, _ := newTestSource(t, &fakeDevice{}, 20)
	err := src.Run(context.Background(), capture.Options{}, func(capture.Packet) {})
	if capture.ReasonOf(err, "") != capture.ReasonAndroidTooOld || !strings.Contains(err.Error(), "SDK 20") {
		t.Errorf("SDK 20: %v", err)
	}
	src, _ = newTestSource(t, &fakeDevice{}, 34)
	if err := src.Run(context.Background(), capture.Options{FPS: 2}, nil); err == nil || !strings.Contains(err.Error(), "fps") {
		t.Errorf("fps 2: %v", err)
	}

	defer func(d time.Duration) { exitWait = d }(exitWait)
	exitWait = 200 * time.Millisecond
	dev := &fakeDevice{serial: "EOF", shellOut: ServerSHA256, serve: serveStreams(syntheticVideo(t, 3), syntheticAudio(2), false)}
	useFakeServer(t, `echo "[server] INFO: Device: x"; echo "[server] ERROR: Encoding error: boom" >&2; wait`)
	src, _ = newTestSource(t, dev, 34)
	var c collector
	err = src.Run(context.Background(), capture.Options{}, c.emit)
	if capture.ReasonOf(err, "") != capture.ReasonScrcpyFailed || !strings.Contains(err.Error(), "stopped (EOF)") ||
		!strings.Contains(err.Error(), "its last line: [server] ERROR: Encoding error: boom") {
		t.Errorf("unexpected EOF: %v", err)
	}
	if c.count(capture.Video) != 3 {
		t.Errorf("%d frames before the EOF", c.count(capture.Video))
	}

	// Silent server: "no output".
	useFakeServer(t, "wait")
	src, _ = newTestSource(t, &fakeDevice{serial: "EOF2", shellOut: ServerSHA256, serve: serveStreams(syntheticVideo(t, 1), nil, false)}, 29)
	if err := src.Run(context.Background(), capture.Options{}, func(capture.Packet) {}); err == nil || !strings.Contains(err.Error(), "its last line: no output") {
		t.Errorf("silent server: %v", err)
	}

	src, _ = newTestSource(t, &fakeDevice{serial: "FWD", shellOut: ServerSHA256, fwdErr: errors.New("cannot bind")}, 34)
	if err := src.Run(context.Background(), capture.Options{}, nil); capture.ReasonOf(err, "") != capture.ReasonScrcpyFailed || !strings.Contains(err.Error(), "adb forward: cannot bind") {
		t.Errorf("forward failure: %v", err)
	}

	prev := commandContext
	commandContext = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, filepath.Join(t.TempDir(), "no-adb"))
	}
	defer func() { commandContext = prev }()
	src, _ = newTestSource(t, &fakeDevice{serial: "START", shellOut: ServerSHA256, serve: func(int, net.Conn) {}}, 34)
	if err := src.Run(context.Background(), capture.Options{}, nil); capture.ReasonOf(err, "") != capture.ReasonScrcpyFailed || !strings.Contains(err.Error(), "start scrcpy-server") {
		t.Errorf("start failure: %v", err)
	}
}

// U29: audio the device disables (codec id 0) or that breaks: the source
// logs why, emits an AudioConfig without a format so subscribers stop
// waiting, and the video goes on.
func TestRun_NoAudio(t *testing.T) {
	cases := map[string]struct {
		audio  []byte
		log    string
		config bool // an AudioConfig without a format is emitted
	}{
		"disabled": {audio: (&streamWriter{}).u32(codecDisabled).Bytes(), log: "no audio: scrcpy: the device disabled audio capture", config: true},
		"error":    {audio: (&streamWriter{}).u32(codecError).Bytes(), log: "no audio: scrcpy: the device's audio capture failed", config: true},
		"broken":   {audio: (&streamWriter{}).u32(0x6f707573).Bytes(), log: "audio ended: scrcpy: audio codec id"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dev := &fakeDevice{serial: "N", shellOut: ServerSHA256}
			dev.serve = serveStreams(syntheticVideo(t, 10), tc.audio, true)
			useFakeServer(t, "wait")
			src, logs := newTestSource(t, dev, 34)
			ctx, cancel := context.WithCancel(context.Background())
			var c collector
			done := make(chan error, 1)
			go func() { done <- src.Run(ctx, capture.Options{}, c.emit) }()
			waitFor(t, "the video", func() bool { return c.count(capture.Video) == 10 })
			waitFor(t, "the log line", func() bool { return strings.Contains(logs.text(), tc.log) })
			cancel()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			var formatless int
			for _, p := range c.packets() {
				if p.Kind == capture.AudioConfig && p.Audio == nil {
					formatless++
				}
			}
			if (formatless == 1) != tc.config {
				t.Errorf("%d format-less audio configs", formatless)
			}
		})
	}
}

// Review 1.harness-review.5: the audio socket's dial is retried like the
// video socket's. The server accepts it before it streams anything, so a
// run whose audio socket never connects fails scrcpy-failed (the session
// starts it again for the next subscriber) instead of waiting for video
// that never comes.
func TestRun_AudioSocketDial(t *testing.T) {
	defer func(d time.Duration) { connectTimeout = d }(connectTimeout)
	connectTimeout = 2 * time.Second
	video, audio := syntheticVideo(t, 10), syntheticAudio(5)

	// The forward refuses connections for 300 ms after the video socket,
	// then accepts the audio socket.
	dev := &fakeDevice{serial: "LATE", shellOut: ServerSHA256}
	serve := serveStreams(video, audio, true)
	dev.serve = func(i int, c net.Conn) {
		if i == 0 {
			dev.mu.Lock()
			at := dev.l.Addr().String()
			dev.l.Close()
			dev.mu.Unlock()
			go func() {
				time.Sleep(300 * time.Millisecond)
				l, err := net.Listen("tcp", at)
				if err != nil {
					t.Errorf("listen again: %v", err)
					return
				}
				dev.mu.Lock()
				dev.l = l
				dev.mu.Unlock()
				if a, err := l.Accept(); err == nil {
					serve(1, a)
				}
			}()
		}
		serve(i, c)
	}
	useFakeServer(t, "wait")
	src, _ := newTestSource(t, dev, 34)
	ctx, cancel := context.WithCancel(context.Background())
	var c collector
	done := make(chan error, 1)
	go func() { done <- src.Run(ctx, capture.Options{}, c.emit) }()
	waitFor(t, "video and audio", func() bool { return c.count(capture.Video) == 10 && c.count(capture.Audio) == 5 })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("late audio socket: %v", err)
	}

	// The audio socket never connects: scrcpy-failed after the connect
	// timeout, with no video emitted.
	connectTimeout = 400 * time.Millisecond
	gone := &fakeDevice{serial: "NOAUDIO", shellOut: ServerSHA256}
	gone.serve = func(i int, c net.Conn) {
		if i == 0 {
			gone.mu.Lock()
			gone.l.Close()
			gone.mu.Unlock()
		}
		serveStreams(video, nil, true)(i, c)
	}
	useFakeServer(t, `echo "[server] INFO: Device: x"; wait`)
	src, _ = newTestSource(t, gone, 34)
	var none collector
	start := time.Now()
	err := src.Run(context.Background(), capture.Options{}, none.emit)
	if capture.ReasonOf(err, "") != capture.ReasonScrcpyFailed || !strings.Contains(err.Error(), "did not connect the audio socket (no connection within 400ms") {
		t.Fatalf("no audio socket: %v", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("took %v", took)
	}
	if none.count(capture.Video) != 0 || none.count(capture.AudioConfig) != 0 {
		t.Errorf("emitted %+v", none.packets())
	}

	// The server exits while the audio socket is dialled: at once.
	connectTimeout = 5 * time.Second
	useFakeServer(t, `sleep 0.3; echo "[server] ERROR: Could not open audio stream" >&2; exit 1`)
	src, _ = newTestSource(t, gone, 34)
	start = time.Now()
	err = src.Run(context.Background(), capture.Options{}, func(capture.Packet) {})
	if capture.ReasonOf(err, "") != capture.ReasonScrcpyFailed || !strings.Contains(err.Error(), "the server exited") ||
		!strings.Contains(err.Error(), "Could not open audio stream") || time.Since(start) > 3*time.Second {
		t.Fatalf("server exit while dialling audio: %v after %v", err, time.Since(start))
	}

	// Cancelled while dialling the audio socket: nil.
	useFakeServer(t, "wait")
	src, _ = newTestSource(t, gone, 34)
	cctx, ccancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer ccancel()
	if err := src.Run(cctx, capture.Options{}, func(capture.Packet) {}); err != nil {
		t.Errorf("cancel while dialling audio: %v", err)
	}
}

func TestServerLog(t *testing.T) {
	q := &quiet{}
	l := &serverLog{prefix: "[p] ", logf: q.logf}
	if l.Last() != "" {
		t.Error("empty log has a last line")
	}
	_, _ = l.Write([]byte("one\r\n\n  \ntw"))
	_, _ = l.Write([]byte("o\nthr"))
	if q.text() != "[p] one\n[p] two" {
		t.Errorf("logged %q", q.text())
	}
	if l.Last() != "thr" {
		t.Errorf("Last = %q", l.Last())
	}
	_, _ = l.Write([]byte("ee\n"))
	if l.Last() != "three" {
		t.Errorf("Last = %q", l.Last())
	}
}
