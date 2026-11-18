//go:build darwin

package quicktime

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/capture"
	"github.com/devicelab-dev/maestro-runner/pkg/capture/fake"
)

// deviceBehaviour is what the fake helper does for one device.
type deviceBehaviour struct {
	unavailable string // answer unavailable with this reason
	silent      bool   // never open the device connection
	foundOnly   bool   // send device-found, then nothing
	match       string // "serial" (default) or "name"
	frames      int    // video frames to send (default 30), then idle
	floatAudio  bool   // send non-interleaved 32-bit float PCM, as some iPhone attaches do, instead of s16
	brokenAudio bool   // with floatAudio, cut every audio sample short of a whole frame
}

// fakeHelper speaks protocol 1 like MaestroDCapture: a control connection,
// then a connection per opened device sending the fake clip and PCM.
type fakeHelper struct {
	t          *testing.T
	permission string        // "" granted, "wait" (until grant), "deny"
	grant      chan struct{} // closed to grant a "wait" permission
	noConnect  int32         // launches (1-based) that never connect
	protocol   int           // hello protocol (default 1)
	behaviour  func(udid string) deviceBehaviour

	launches atomic.Int32
	ops      chan controlOp

	mu    sync.Mutex
	ctrl  []net.Conn
	conns map[string]net.Conn
	// launchOf is the launch that opened each device connection, so a
	// helper that exits closes only its own (a late exit of the previous
	// launch must not close the next launch's connections).
	launchOf map[string]int
}

func newFakeHelper(t *testing.T) *fakeHelper {
	return &fakeHelper{t: t, grant: make(chan struct{}), ops: make(chan controlOp, 64), conns: map[string]net.Conn{}, launchOf: map[string]int{},
		behaviour: func(string) deviceBehaviour { return deviceBehaviour{} }}
}

// launch is the Host's launch seam.
func (f *fakeHelper) launch(app, socket string) error {
	n := f.launches.Add(1)
	if n <= f.noConnect {
		return nil
	}
	go f.run(socket, int(n))
	return nil
}

func (f *fakeHelper) run(socket string, pid int) {
	c, err := net.Dial("unix", socket)
	if err != nil {
		f.t.Errorf("fake helper: %v", err)
		return
	}
	f.mu.Lock()
	f.ctrl = append(f.ctrl, c)
	f.mu.Unlock()
	proto := f.protocol
	if proto == 0 {
		proto = ProtocolVersion
	}
	_ = writeFrame(c, statusFrame(status{State: stateHello, Protocol: proto, PID: pid, Version: "fake"}))
	switch f.permission {
	case "wait":
		_ = writeFrame(c, statusFrame(status{State: stateWaitingPermission}))
		<-f.grant
	case "deny":
		_ = writeFrame(c, statusFrame(status{State: stateUnavailable, Reason: capture.ReasonCameraPermission, Message: "denied"}))
		_ = c.Close()
		return
	}
	_ = writeFrame(c, statusFrame(status{State: stateReady}))
	sc := bufio.NewScanner(c)
	for sc.Scan() {
		var op controlOp
		if json.Unmarshal(sc.Bytes(), &op) != nil {
			continue
		}
		select {
		case f.ops <- op:
		default:
		}
		switch op.Op {
		case "open":
			go f.device(socket, op.UDID, pid)
		case "close":
			f.closeDevice(op.UDID)
		}
	}
	// The control connection closed: the helper exits.
	f.mu.Lock()
	for udid, dc := range f.conns {
		if f.launchOf[udid] == pid {
			_ = dc.Close()
			delete(f.conns, udid)
		}
	}
	f.mu.Unlock()
}

func (f *fakeHelper) closeDevice(udid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if dc := f.conns[udid]; dc != nil {
		_ = dc.Close()
		delete(f.conns, udid)
	}
}

// kill ends the helper: every connection closes.
func (f *fakeHelper) kill() {
	f.mu.Lock()
	ctrl := f.ctrl
	f.ctrl = nil
	f.mu.Unlock()
	for _, c := range ctrl {
		_ = c.Close()
	}
}

func (f *fakeHelper) device(socket, udid string, launch int) {
	b := f.behaviour(udid)
	if b.silent {
		return
	}
	c, err := net.Dial("unix", socket)
	if err != nil {
		return
	}
	f.mu.Lock()
	f.conns[udid] = c
	f.launchOf[udid] = launch
	f.mu.Unlock()
	send := func(fr frame) bool { return writeFrame(c, fr) == nil }
	send(statusFrame(status{State: stateHello, UDID: udid}))
	if b.unavailable != "" {
		send(statusFrame(status{State: stateUnavailable, Reason: b.unavailable, Message: "fake " + b.unavailable}))
		_ = c.Close()
		return
	}
	send(statusFrame(status{State: stateDeviceFound, Message: "fake device"}))
	if b.foundOnly {
		return
	}
	format, frames, err := fake.ParseClip(fake.Clip())
	if err != nil {
		f.t.Error(err)
		return
	}
	match := b.match
	if match == "" {
		match = "serial"
	}
	n := b.frames
	if n == 0 {
		n = 30
	}
	pcm, bits, aflags := fake.PCM(), 16, uint32(lpcmFlags)
	if b.floatAudio {
		pcm, bits, aflags = fake.FloatPCM(), 32, floatFlags|asbdIsNonInterleaved
	}
	audioChunk := 48000 / 60 * 2 * bits / 8 // one 60 fps frame of 48 kHz stereo
	if !send(frame{typ: frameAudioConfig, payload: audioConfigPayload(48000, 2, bits, aflags)}) ||
		!send(frame{typ: frameVideoConfig, payload: videoConfigPayload(format)}) ||
		!send(statusFrame(status{State: stateCapturing, Match: match})) {
		return
	}
	for k := 0; k < n; k++ {
		fr := frames[k%len(frames)]
		pts := time.Duration(k) * time.Second / 60
		flags := uint8(0)
		if fr.Key {
			flags = flagSync
		}
		off := (k * audioChunk) % (len(pcm) - audioChunk)
		if !send(frame{typ: frameVideoSample, flags: flags, pts: pts, payload: fr.Data}) ||
			!send(frame{typ: frameAudioSample, pts: pts, payload: audioPayload(pcm[off:off+audioChunk], b.floatAudio, bits/8)[:audioChunk-b2i(b.brokenAudio)]}) {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// waitOp waits for a control op.
func (f *fakeHelper) waitOp(t *testing.T, op, udid string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case o := <-f.ops:
			if o.Op == op && o.UDID == udid {
				return
			}
		case <-deadline:
			t.Fatalf("no %s %s on the control connection", op, udid)
		}
	}
}

// testHost is a Host on a short socket directory, launching the fake
// helper, with the waits shortened.
func testHost(t *testing.T, f *fakeHelper) *Host {
	t.Helper()
	tmp, err := os.MkdirTemp("/tmp", "qt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmp) })
	shortWaits(t, 2*time.Second, 5*time.Second)
	h := newHost(hostConfig{
		app:            func() (string, error) { return "/fake/MaestroDCapture.app", nil },
		launch:         f.launch,
		tmpDir:         func() string { return tmp },
		now:            time.Now,
		sleep:          func(time.Duration) {},
		connectTimeout: 2 * time.Second,
	})
	t.Cleanup(func() {
		f.kill()
		h.mu.Lock()
		if h.ln != nil {
			h.ln.Close()
		}
		h.mu.Unlock()
	})
	return h
}

func shortWaits(t *testing.T, ready, extended time.Duration) {
	oldReady, oldExt := readyWait, extendedWait
	readyWait, extendedWait = ready, extended
	t.Cleanup(func() { readyWait, extendedWait = oldReady, oldExt })
}

// audioPayload is an interleaved stereo chunk as the helper sends it: as is,
// or split into its two planes (all left samples, then all right) when the
// device delivers non-interleaved audio.
func audioPayload(chunk []byte, planar bool, width int) []byte {
	if !planar {
		return chunk
	}
	frames := len(chunk) / (2 * width)
	out := make([]byte, len(chunk))
	for i := 0; i < frames; i++ {
		for c := 0; c < 2; c++ {
			copy(out[(c*frames+i)*width:], chunk[(2*i+c)*width:(2*i+c+1)*width])
		}
	}
	return out
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
