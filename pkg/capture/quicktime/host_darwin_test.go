//go:build darwin

package quicktime

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/capture"
	"github.com/devicelab-dev/maestro-runner/pkg/capture/fake"
	"github.com/devicelab-dev/maestro-runner/pkg/capture/internal/probetest"
	"github.com/devicelab-dev/maestro-runner/pkg/logger"
)

// logTo sends the package logger to a temp file for the test and returns a
// reader of what it wrote.
func logTo(t *testing.T) func() string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "daemon.log")
	if err := logger.Init(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(logger.Close)
	return func() string {
		b, _ := os.ReadFile(path)
		return string(b)
	}
}

func waitState(t *testing.T, s *capture.Session, want capture.State) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st, reason := s.State(); st == want {
			return reason
		}
		time.Sleep(5 * time.Millisecond)
	}
	st, reason := s.State()
	t.Fatalf("state %v (%s), want %v", st, reason, want)
	return ""
}

func mustCapture(t *testing.T, d *Device) Result {
	t.Helper()
	res := d.Wait(context.Background())
	if res.Err != nil {
		t.Fatalf("wait: %v", res.Err)
	}
	if !res.Found || res.At.IsZero() {
		t.Fatalf("capturing without found: %+v", res)
	}
	return res
}

// firstPacket subscribes and returns once a packet arrives.
func receives(t *testing.T, s *capture.Session) {
	t.Helper()
	sub, err := s.Subscribe(capture.Options{Audio: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	select {
	case _, ok := <-sub.Packets():
		if !ok {
			t.Fatalf("subscriber ended: %v", sub.Err())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no packet")
	}
}

// U40: a fake helper speaking protocol 1 drives a whole session to a file
// ffprobe reads as h264 with ipcm audio; non-interleaved float samples (Q9)
// come out interleaved as fpcm, read as pcm_f32le.
func TestHost_CaptureToMP4(t *testing.T) {
	t.Run("ipcm", func(t *testing.T) { captureToMP4(t, false, "pcm_s16le", "ipcm") })
	t.Run("fpcm", func(t *testing.T) { captureToMP4(t, true, "pcm_f32le", "fpcm") })
}

// An audio sample the decoder cannot interleave drops the device's audio
// for the attachment, with a warning, and the video keeps flowing.
func TestHost_BrokenAudioSampleDropsAudio(t *testing.T) {
	f := newFakeHelper(t)
	f.behaviour = func(string) deviceBehaviour { return deviceBehaviour{frames: 90, floatAudio: true, brokenAudio: true} }
	h := testHost(t, f)
	log := logTo(t)
	d := h.Open("00008110-EXAMPLE000000001", "iPhone13Pro")
	defer d.Close()
	mustCapture(t, d)
	sub, err := d.Session().Subscribe(capture.Options{Audio: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	deadline := time.After(5 * time.Second)
	videos := 0
	for videos < 30 {
		select {
		case p, ok := <-sub.Packets():
			if !ok {
				t.Fatalf("subscriber ended: %v", sub.Err())
			}
			if p.Kind == capture.Audio {
				t.Fatal("an audio sample got through")
			}
			if p.Kind == capture.Video {
				videos++
			}
		case <-deadline:
			t.Fatalf("%d video packets in 5 s:\n%s", videos, log())
		}
	}
	if n := strings.Count(log(), "audio dropped: capture helper audio sample of 6399 bytes does not hold whole 2-channel frames"); n != 1 {
		t.Errorf("audio dropped logged %d times:\n%s", n, log())
	}
}

func captureToMP4(t *testing.T, floatAudio bool, codec, tag string) {
	f := newFakeHelper(t)
	f.behaviour = func(string) deviceBehaviour { return deviceBehaviour{frames: 90, floatAudio: floatAudio} }
	h := testHost(t, f)
	log := logTo(t)
	d := h.Open("00008110-EXAMPLE000000001", "iPhone13Pro")
	defer d.Close()
	res := mustCapture(t, d)
	if res.Match != "serial" {
		t.Fatalf("match %q", res.Match)
	}
	// The Host logs the capturing status, once; step 0 adds no second line.
	if n := strings.Count(log(), "capture 00008110-EXAMPLE000000001: capturing (match: serial)"); n != 1 {
		t.Fatalf("capturing logged %d times:\n%s", n, log())
	}
	f.waitOp(t, "open", "00008110-EXAMPLE000000001")
	sess := d.Session()
	if sess.Mode() != capture.AlwaysOn || sess.FPSControl() {
		t.Fatal("iOS capture is always-on and picks its own rate")
	}
	r, err := capture.NewRecorder(sess, t.TempDir(), capture.Options{Audio: true})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	out := filepath.Join(t.TempDir(), "iphone.mp4")
	if err := r.Stop(out); err != nil {
		t.Fatal(err)
	}
	pr := probetest.Probe(t, out)
	v, ok := pr.Find("h264")
	if !ok || v.Width != 640 || v.Height != 360 || v.FrameCount() < 1 {
		t.Fatalf("video: %+v", pr.Streams)
	}
	a, ok := pr.Find(codec)
	if !ok || a.CodecTag != tag || a.SampleRate != "48000" || a.Channels != 2 {
		t.Fatalf("audio: %+v", pr.Streams)
	}
	wantLog := "audio lpcm 48000 Hz, 2 ch, 16-bit integer, interleaved"
	if floatAudio {
		wantLog = "audio lpcm 48000 Hz, 2 ch, 32-bit float, non-interleaved, interleaved here"
		// The interleaved samples are the fixture's own, bit for bit: the
		// first 20 ms of the file's audio equal the fixture's.
		// The recording's first audio packet, interleaved here, is one of
		// the helper's chunks of the fixture, bit for bit.
		raw, err := exec.Command(filepath.Join(filepath.Dir(probetest.Binary(t)), "ffmpeg"),
			"-v", "error", "-i", out, "-map", "0:a", "-c", "copy", "-f", "f32le", "-").Output()
		looped := append(append([]byte(nil), fake.FloatPCM()...), fake.FloatPCM()...)
		if err != nil || len(raw) < 6400 || !bytes.Contains(looped, raw[:6400]) {
			t.Errorf("the fpcm track is not the fixture's samples interleaved (%d bytes, %v)", len(raw), err)
		}
	}
	if !strings.Contains(log(), wantLog) {
		t.Errorf("no %q in the log:\n%s", wantLog, log())
	}
}

// U40, U37: two devices share one helper; closing one leaves the other
// capturing; the helper exiting marks both lost; the next open launches a
// new helper and captures.
func TestHost_SharedHelperAndLoss(t *testing.T) {
	f := newFakeHelper(t)
	f.behaviour = func(udid string) deviceBehaviour {
		if udid == "B" {
			return deviceBehaviour{match: "name", frames: 100000}
		}
		return deviceBehaviour{frames: 100000}
	}
	h := testHost(t, f)
	a := h.Open("A", "Phone A")
	b := h.Open("B", "Phone B")
	mustCapture(t, a)
	if res := mustCapture(t, b); res.Match != "name" {
		t.Fatalf("B match %q", res.Match)
	}
	if n := f.launches.Load(); n != 1 {
		t.Fatalf("%d helper launches for two devices, want 1", n)
	}
	if dup := h.Open("A", "Phone A"); capture.ReasonOf(dup.Wait(context.Background()).Err, "") != capture.ReasonHelperFailed || dup.Session() != nil {
		t.Fatal("a second capture of one device in one process is refused")
	} else {
		dup.Close()
	}

	a.Close()
	f.waitOp(t, "close", "A")
	receives(t, b.Session())
	if st, _ := b.Session().State(); st != capture.StateCapturing {
		t.Fatalf("B after A closed: %v", st)
	}

	c := h.Open("C", "Phone C")
	mustCapture(t, c)
	f.kill()
	for _, d := range []*Device{b, c} {
		if reason := waitState(t, d.Session(), capture.StateLost); reason != capture.ReasonCaptureLost {
			t.Fatalf("lost reason %q", reason)
		}
	}
	b.Close()
	c.Close()

	d := h.Open("D", "Phone D")
	defer d.Close()
	mustCapture(t, d)
	if n := f.launches.Load(); n != 2 {
		t.Fatalf("%d launches after the helper went, want 2", n)
	}
}

// U39: a device connection that ends after capturing is capture-lost;
// before capturing, helper-failed.
func TestHost_DeviceConnectionEnds(t *testing.T) {
	f := newFakeHelper(t)
	f.behaviour = func(udid string) deviceBehaviour {
		if udid == "found" {
			return deviceBehaviour{foundOnly: true}
		}
		return deviceBehaviour{frames: 100000}
	}
	h := testHost(t, f)
	d := h.Open("A", "A")
	defer d.Close()
	mustCapture(t, d)
	f.closeDevice("A")
	if reason := waitState(t, d.Session(), capture.StateLost); reason != capture.ReasonCaptureLost {
		t.Fatalf("reason %q", reason)
	}
	if !strings.Contains(d.Session().LastError().Error(), "stopped") {
		t.Fatalf("last error %v", d.Session().LastError())
	}

	e := h.Open("found", "found")
	defer e.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.mu.Lock()
		found := e.src.found
		h.mu.Unlock()
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("device-found not seen")
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.closeDevice("found")
	res := e.Wait(context.Background())
	if capture.ReasonOf(res.Err, "") != capture.ReasonHelperFailed || !res.Found {
		t.Fatalf("closed before capturing: %+v", res)
	}
}

// U39: the helper's unavailable reasons pass through.
func TestHost_UnavailableReasons(t *testing.T) {
	for _, reason := range []string{capture.ReasonNoCaptureDevice, capture.ReasonAmbiguousDeviceName} {
		f := newFakeHelper(t)
		f.behaviour = func(string) deviceBehaviour { return deviceBehaviour{unavailable: reason} }
		h := testHost(t, f)
		d := h.Open("A", "A")
		res := d.Wait(context.Background())
		var u *capture.Unavailable
		if !errors.As(res.Err, &u) || u.Reason != reason || res.Found {
			t.Fatalf("%s: %+v", reason, res)
		}
		if st, r := d.Session().State(); st != capture.StateUnavailable || r != reason {
			t.Fatalf("%s: session %v %s", reason, st, r)
		}
		if _, err := d.Session().Subscribe(capture.Options{}); capture.ReasonOf(err, "") != reason {
			t.Fatalf("%s: subscribe %v", reason, err)
		}
		d.Close()
		d.Close()
	}
}

// U39: waiting-permission extends the wait beyond readyWait until access
// is granted.
func TestHost_WaitingPermissionExtends(t *testing.T) {
	f := newFakeHelper(t)
	f.permission = "wait"
	h := testHost(t, f)
	shortWaits(t, 100*time.Millisecond, 3*time.Second)
	d := h.Open("A", "A")
	defer d.Close()
	time.AfterFunc(400*time.Millisecond, func() { close(f.grant) })
	start := time.Now()
	mustCapture(t, d)
	if time.Since(start) < 350*time.Millisecond {
		t.Fatal("captured before access was granted")
	}
}

// U39: a prompt nobody answers ends the wait with camera-permission.
func TestHost_WaitingPermissionTimesOut(t *testing.T) {
	f := newFakeHelper(t)
	f.permission = "wait"
	h := testHost(t, f)
	shortWaits(t, 50*time.Millisecond, 300*time.Millisecond)
	log := logTo(t)
	d := h.Open("A", "A")
	res := d.Wait(context.Background())
	if capture.ReasonOf(res.Err, "") != capture.ReasonCameraPermission || !strings.Contains(res.Err.Error(), "camera access for MaestroDCapture was not approved") {
		t.Fatalf("%+v", res)
	}
	// Both name the helper as macOS shows it in the prompt and in Privacy & Security.
	if !strings.Contains(log(), "approve camera access for MaestroDCapture on this Mac") {
		t.Fatalf("log:\n%s", log())
	}
	d.Close()
	close(f.grant)
}

// U39: denied access is camera-permission for every device, and the next
// open launches a new helper that answers the same.
func TestHost_CameraDenied(t *testing.T) {
	f := newFakeHelper(t)
	f.permission = "deny"
	h := testHost(t, f)
	for i := 1; i <= 2; i++ {
		d := h.Open("A", "A")
		res := d.Wait(context.Background())
		if capture.ReasonOf(res.Err, "") != capture.ReasonCameraPermission {
			t.Fatalf("open %d: %+v", i, res)
		}
		d.Close()
		deadline := time.Now().Add(5 * time.Second)
		for {
			h.mu.Lock()
			gone := h.helper == nil
			h.mu.Unlock()
			if gone {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the denied helper was not forgotten")
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	if n := f.launches.Load(); n != 2 {
		t.Fatalf("launches %d", n)
	}
}

// U39: a device the helper never opens times out after readyWait.
func TestHost_NoCaptureWithinReadyWait(t *testing.T) {
	f := newFakeHelper(t)
	f.behaviour = func(string) deviceBehaviour { return deviceBehaviour{silent: true} }
	h := testHost(t, f)
	shortWaits(t, 150*time.Millisecond, 3*time.Second)
	d := h.Open("A", "A")
	defer d.Close()
	res := d.Wait(context.Background())
	if capture.ReasonOf(res.Err, "") != capture.ReasonHelperFailed || !strings.Contains(res.Err.Error(), "did not start capturing") {
		t.Fatalf("%+v", res)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e := h.Open("B", "B")
	defer e.Close()
	if res := e.Wait(ctx); capture.ReasonOf(res.Err, "") != capture.ReasonHelperFailed {
		t.Fatalf("cancelled wait: %+v", res)
	}
}

// U37: the open(1) command line.
func TestLaunchArgs(t *testing.T) {
	got := strings.Join(launchArgs("/c/MaestroDCapture.app", "/t/maestro-d-cap-501/42.sock"), " ")
	want := "-n -g -j -a /c/MaestroDCapture.app --args --socket /t/maestro-d-cap-501/42.sock --protocol 1"
	if got != want {
		t.Fatalf("got %q", got)
	}
	var calls [][]string
	old := runCommand
	defer func() { runCommand = old }()
	runCommand = func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		if len(calls) == 2 {
			return []byte("LSOpenURLsWithRole() failed"), errors.New("exit status 1")
		}
		return nil, nil
	}
	if err := launchHelper("/c/App.app", "/s.sock"); err != nil || calls[0][0] != "open" || calls[0][5] != "/c/App.app" {
		t.Fatalf("launch: %v %v", err, calls)
	}
	if err := launchHelper("/c/App.app", "/s.sock"); err == nil || !strings.Contains(err.Error(), "LSOpenURLsWithRole") {
		t.Fatalf("failed launch: %v", err)
	}
}

// U37: the socket path comes from the uid and pid, with a /tmp fallback
// when TMPDIR makes it too long.
func TestSocketPath(t *testing.T) {
	dir, path := socketPath("/var/tmp", 501, 4242)
	if dir != "/var/tmp/maestro-d-cap-501" || path != "/var/tmp/maestro-d-cap-501/4242.sock" {
		t.Fatalf("%s %s", dir, path)
	}
	long := "/" + strings.Repeat("x", 90)
	dir, path = socketPath(long, 501, 4242)
	if dir != "/tmp/maestro-d-cap-501" || path != "/tmp/maestro-d-cap-501/4242.sock" {
		t.Fatalf("fallback %s %s", dir, path)
	}
}

// U37: the listener's directory is 0700, its socket 0600.
func TestListenModes(t *testing.T) {
	f := newFakeHelper(t)
	h := testHost(t, f)
	h.mu.Lock()
	err := h.listenLocked()
	again := h.listenLocked()
	h.mu.Unlock()
	if err != nil || again != nil {
		t.Fatal(err, again)
	}
	if !strings.HasSuffix(h.sockPath, "/"+strconv.Itoa(os.Getpid())+".sock") {
		t.Fatalf("socket %s", h.sockPath)
	}
	di, _ := os.Stat(h.dir)
	si, _ := os.Stat(h.sockPath)
	if di.Mode().Perm() != 0o700 || si.Mode().Perm() != 0o600 {
		t.Fatalf("modes %v %v", di.Mode(), si.Mode())
	}
	// Unknown peers and bad hellos are dropped.
	for _, hello := range []frame{{typ: frameVideoSample}, statusFrame(status{State: stateReady}), statusFrame(status{State: stateHello, UDID: "nobody"})} {
		c, err := net.Dial("unix", h.sockPath)
		if err != nil {
			t.Fatal(err)
		}
		_ = writeFrame(c, hello)
		buf := make([]byte, 1)
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := c.Read(buf); err == nil {
			t.Fatal("connection kept")
		}
		c.Close()
	}
}

// U37: a helper that does not connect in time is helper-failed; a stray
// control connection afterwards is dropped.
func TestHost_HelperNeverConnects(t *testing.T) {
	f := newFakeHelper(t)
	f.noConnect = 1
	h := testHost(t, f)
	h.cfg.connectTimeout = 100 * time.Millisecond
	d := h.Open("A", "A")
	defer d.Close()
	res := d.Wait(context.Background())
	if capture.ReasonOf(res.Err, "") != capture.ReasonHelperFailed || !strings.Contains(res.Err.Error(), "did not connect") {
		t.Fatalf("%+v", res)
	}
	if n := f.launches.Load(); n != 1 {
		t.Fatalf("launches %d (no recent launch, so no retry)", n)
	}
	c, err := net.Dial("unix", h.sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = writeFrame(c, statusFrame(status{State: stateHello, Protocol: 1, PID: 9}))
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("a control connection with no helper waiting is kept")
	}
}

// U37: a launch less than 10 s after the previous one that does not
// connect waits out the rest of those 10 s and relaunches exactly once.
func TestHost_QuickRelaunch(t *testing.T) {
	f := newFakeHelper(t)
	f.noConnect = 1
	h := testHost(t, f)
	shortWaits(t, 2*time.Second, 30*time.Second) // the fake clock jumps 7 s
	h.cfg.connectTimeout = 100 * time.Millisecond
	base := time.Unix(1_800_000_000, 0)
	var mu sync.Mutex
	clock := base
	var slept []time.Duration
	h.cfg.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	h.cfg.sleep = func(d time.Duration) { mu.Lock(); slept = append(slept, d); clock = clock.Add(d); mu.Unlock() }
	h.mu.Lock()
	if err := h.listenLocked(); err != nil {
		t.Fatal(err)
	}
	h.mu.Unlock()
	writeLastLaunch(h.dir, base.Add(-3*time.Second))

	d := h.Open("A", "A")
	defer d.Close()
	mustCapture(t, d)
	if n := f.launches.Load(); n != 2 {
		t.Fatalf("launches %d, want a single relaunch", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(slept) != 1 || slept[0] != 7*time.Second {
		t.Fatalf("waited %v, want the 7 s left of the window", slept)
	}
	if got := readLastLaunch(h.dir); !got.Equal(base.Add(7 * time.Second)) {
		t.Fatalf("last-launch %v", got)
	}
}

// U37: when the relaunch does not connect either, the device fails.
func TestHost_QuickRelaunchOnce(t *testing.T) {
	f := newFakeHelper(t)
	f.noConnect = 5
	h := testHost(t, f)
	h.cfg.connectTimeout = 50 * time.Millisecond
	h.mu.Lock()
	_ = h.listenLocked()
	h.mu.Unlock()
	writeLastLaunch(h.dir, time.Now())
	d := h.Open("A", "A")
	defer d.Close()
	if res := d.Wait(context.Background()); capture.ReasonOf(res.Err, "") != capture.ReasonHelperFailed {
		t.Fatalf("%+v", res)
	}
	if n := f.launches.Load(); n != 2 {
		t.Fatalf("launches %d", n)
	}
}

func TestHost_LaunchAndBuildFailures(t *testing.T) {
	f := newFakeHelper(t)
	h := testHost(t, f)
	h.cfg.app = func() (string, error) { return "", errors.New("swiftc: boom") }
	d := h.Open("A", "A")
	if res := d.Wait(context.Background()); capture.ReasonOf(res.Err, "") != capture.ReasonHelperFailed || !strings.Contains(res.Err.Error(), "boom") {
		t.Fatalf("build: %+v", res)
	}
	d.Close()
	h.cfg.app = func() (string, error) { return "/x.app", nil }
	h.cfg.launch = func(string, string) error { return errors.New("open failed") }
	d = h.Open("A", "A")
	if res := d.Wait(context.Background()); capture.ReasonOf(res.Err, "") != capture.ReasonHelperFailed || !strings.Contains(res.Err.Error(), "open failed") {
		t.Fatalf("launch: %+v", res)
	}
	d.Close()
}

// A helper of another protocol version is refused.
func TestHost_ProtocolMismatch(t *testing.T) {
	f := newFakeHelper(t)
	f.protocol = 2
	h := testHost(t, f)
	h.cfg.connectTimeout = 200 * time.Millisecond
	d := h.Open("A", "A")
	defer d.Close()
	if res := d.Wait(context.Background()); capture.ReasonOf(res.Err, "") != capture.ReasonHelperFailed {
		t.Fatalf("%+v", res)
	}
}

// A device closed before its helper connected is never opened.
func TestHost_CloseWhileLaunching(t *testing.T) {
	f := newFakeHelper(t)
	release := make(chan struct{})
	h := testHost(t, f)
	h.cfg.app = func() (string, error) { <-release; return "/x.app", nil }
	a := h.Open("A", "A")
	b := h.Open("B", "B")
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.mu.Lock()
		n := 0
		if h.helper != nil {
			n = len(h.helper.pending)
		}
		h.mu.Unlock()
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("opens not queued")
		}
		time.Sleep(5 * time.Millisecond)
	}
	a.Close()
	close(release)
	mustCapture(t, b)
	b.Close()
	f.waitOp(t, "open", "B")
	select {
	case op := <-f.ops:
		if op.UDID == "A" {
			t.Fatalf("closed device opened: %+v", op)
		}
	default:
	}
}

func TestLastLaunchFile(t *testing.T) {
	dir := t.TempDir()
	if !readLastLaunch(dir).IsZero() {
		t.Fatal("no file is zero")
	}
	os.WriteFile(filepath.Join(dir, lastLaunchFile), []byte("junk"), 0o600)
	if !readLastLaunch(dir).IsZero() {
		t.Fatal("junk is zero")
	}
	at := time.Unix(1_800_000_000, 5)
	writeLastLaunch(dir, at)
	if !readLastLaunch(dir).Equal(at) {
		t.Fatal("round trip")
	}
	writeLastLaunch(filepath.Join(dir, "missing"), at) // no directory: ignored
}

func TestDefaultHost(t *testing.T) {
	if Default() != Default() || Default().cfg.connectTimeout != 10*time.Second {
		t.Fatal("one process-wide host")
	}
}
