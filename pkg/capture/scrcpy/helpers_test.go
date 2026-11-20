package scrcpy

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
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
	"github.com/devicelab-dev/maestro-runner/pkg/capture/h264"
)

// serverJar is the committed pinned jar, which the tests verify and push.
var serverJar = filepath.Join("..", "..", "..", "drivers", "android", ServerFile)

// Recorded fixtures (TestRecordFixture): the Pixel 8's video and audio
// sockets, from the codec id on.
func readFixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("fixtures", name))
	if err != nil {
		t.Fatalf("fixture %s: %v (record it with TestRecordFixture: MAESTRO_D_RECORD_SCRCPY_FIXTURE=1 ANDROID_SERIAL=<serial>)", name, err)
	}
	return b
}

// streamWriter builds scrcpy socket bytes.
type streamWriter struct{ bytes.Buffer }

func (w *streamWriter) u32(v uint32) *streamWriter {
	_ = binary.Write(&w.Buffer, binary.BigEndian, v)
	return w
}

func (w *streamWriter) session(width, height int) *streamWriter {
	w.u32(1 << 31).u32(uint32(width)).u32(uint32(height))
	return w
}

func (w *streamWriter) packet(pts time.Duration, config, key bool, data []byte) *streamWriter {
	v := uint64(pts / time.Microsecond)
	if config {
		v = flagConfig
	} else if key {
		v |= flagKey
	}
	_ = binary.Write(&w.Buffer, binary.BigEndian, v)
	w.u32(uint32(len(data)))
	w.Write(data)
	return w
}

// syntheticVideo is a video socket built from the fake clip: codec id, a
// session packet, the config packet, then n frames at 15 fps.
func syntheticVideo(t testing.TB, n int) []byte {
	t.Helper()
	format, frames, err := fake.ParseClip(fake.Clip())
	if err != nil {
		t.Fatal(err)
	}
	w := &streamWriter{}
	w.u32(codecH264).session(format.Width, format.Height)
	w.packet(0, true, false, h264.JoinAnnexB(append(append([][]byte{}, format.SPS...), format.PPS...)))
	for k := 0; k < n; k++ {
		f := frames[k%len(frames)]
		nalus, err := h264.SplitAVCC(f.Data)
		if err != nil {
			t.Fatal(err)
		}
		w.packet(time.Second+time.Duration(k)*time.Second/15, false, f.Key, h264.JoinAnnexB(nalus))
	}
	return w.Bytes()
}

// syntheticAudio is an audio socket of the fake AAC frames.
func syntheticAudio(n int) []byte {
	w := &streamWriter{}
	w.u32(codecAAC)
	w.packet(0, true, false, fake.ASC())
	frames := fake.AACFrames()
	for j := 0; j < n; j++ {
		w.packet(time.Second+time.Duration(j)*1024*time.Second/48000, false, false, frames[j%len(frames)])
	}
	return w.Bytes()
}

// collector gathers emitted packets.
type collector struct {
	mu sync.Mutex
	ps []capture.Packet
}

func (c *collector) emit(p capture.Packet) {
	c.mu.Lock()
	c.ps = append(c.ps, p)
	c.mu.Unlock()
}

func (c *collector) packets() []capture.Packet {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]capture.Packet(nil), c.ps...)
}

func (c *collector) count(k capture.Kind) int {
	n := 0
	for _, p := range c.packets() {
		if p.Kind == k {
			n++
		}
	}
	return n
}

// fakeDevice is a Device whose adb forward is a local listener that plays
// the scrcpy server: each accepted connection goes to serve, with its
// index in that forward (0 is the video socket, 1 the audio socket).
type fakeDevice struct {
	t        *testing.T
	serial   string
	shellOut string
	shellErr error
	pushErr  error
	fwdErr   error
	serve    func(i int, c net.Conn)

	mu       sync.Mutex
	shells   []string
	pushes   []string
	forwards []string
	removed  []int
	l        net.Listener
	conns    int
}

func (d *fakeDevice) Serial() string  { return d.serial }
func (d *fakeDevice) ADBPath() string { return "/fake/adb" }

func (d *fakeDevice) Shell(cmd string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.shells = append(d.shells, cmd)
	return d.shellOut, d.shellErr
}

func (d *fakeDevice) Push(local, remote string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pushes = append(d.pushes, local+" -> "+remote)
	return d.pushErr
}

func (d *fakeDevice) ForwardTCPToAbstractSocket(port int, name string) error {
	if d.fwdErr != nil {
		return d.fwdErr
	}
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return err
	}
	d.mu.Lock()
	d.forwards = append(d.forwards, fmt.Sprintf("tcp:%d localabstract:%s", port, name))
	d.l, d.conns = l, 0
	d.mu.Unlock()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			d.mu.Lock()
			i := d.conns
			d.conns++
			d.mu.Unlock()
			go d.serve(i, c)
		}
	}()
	return nil
}

func (d *fakeDevice) RemoveTCPForward(port int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.removed = append(d.removed, port)
	if d.l != nil {
		d.l.Close()
	}
	return nil
}

// serveStreams answers the video socket with the dummy byte and video,
// and the audio socket with audio; each connection then stays open until
// the client closes it (hold) or closes at once.
func serveStreams(video, audio []byte, hold bool) func(int, net.Conn) {
	return func(i int, c net.Conn) {
		defer c.Close()
		switch i {
		case 0:
			_, _ = c.Write([]byte{0})
			_, _ = c.Write(video)
		case 1:
			_, _ = c.Write(audio)
		}
		if hold {
			_, _ = io.Copy(io.Discard, c)
		}
	}
}

// fakeServer replaces commandContext with a shell script standing in for
// `adb shell … app_process …`. It records the argv; the script writes its
// pid, and the pid of a child it starts, to pidFile.
type fakeServer struct {
	mu      sync.Mutex
	argv    [][]string
	pidFile string
}

func useFakeServer(t *testing.T, script string) *fakeServer {
	t.Helper()
	fs := &fakeServer{pidFile: filepath.Join(t.TempDir(), "pids")}
	prev := commandContext
	commandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		fs.mu.Lock()
		fs.argv = append(fs.argv, append([]string{name}, args...))
		fs.mu.Unlock()
		full := "sleep 30 & echo $$ $! > " + fs.pidFile + "; " + script
		return exec.CommandContext(ctx, "/bin/sh", "-c", full)
	}
	t.Cleanup(func() { commandContext = prev })
	return fs
}

func (fs *fakeServer) calls() [][]string {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return append([][]string(nil), fs.argv...)
}

// pids are the script's pid and its child's.
func (fs *fakeServer) pids(t *testing.T) []int {
	t.Helper()
	b, err := os.ReadFile(fs.pidFile)
	if err != nil {
		t.Fatal(err)
	}
	var out []int
	for _, f := range strings.Fields(string(b)) {
		n, err := strconv.Atoi(f)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

// resetPushed forgets which devices hold the jar.
func resetPushed(t *testing.T) {
	t.Helper()
	pushedMu.Lock()
	pushed = map[string]*deviceJar{}
	pushedMu.Unlock()
}

// quiet is a logf that records lines.
type quiet struct {
	mu    sync.Mutex
	lines []string
}

func (q *quiet) logf(format string, args ...any) {
	q.mu.Lock()
	q.lines = append(q.lines, fmt.Sprintf(format, args...))
	q.mu.Unlock()
}

func (q *quiet) text() string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return strings.Join(q.lines, "\n")
}

func newTestSource(t *testing.T, dev Device, sdk int) (*Source, *quiet) {
	t.Helper()
	resetPushed(t)
	q := &quiet{}
	s := New(dev, Config{SDK: sdk, Server: serverJar})
	s.logf = q.logf
	return s, q
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	// Generous: CI runs many packages at once under -race.
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
