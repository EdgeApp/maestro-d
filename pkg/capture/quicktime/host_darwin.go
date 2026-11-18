//go:build darwin

package quicktime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/capture"
	"github.com/devicelab-dev/maestro-runner/pkg/logger"
)

// Waits a device's capture start is allowed (variables so the tests can
// shorten them).
var (
	// readyWait is how long Device.Wait waits for capturing once the
	// helper runs and is not waiting for Camera approval.
	readyWait = 15 * time.Second
	// extendedWait caps the time Wait spends while the helper is being
	// built and launched, or waits for the user to approve Camera access.
	extendedWait = 120 * time.Second
	// handshakeWait bounds a new connection's hello.
	handshakeWait = 10 * time.Second
	// writeWait bounds a control line's write.
	writeWait = 5 * time.Second
)

// hostConfig is what a Host needs from the system; the tests replace it.
type hostConfig struct {
	// app returns the helper app to launch, building it on first use.
	app func() (string, error)
	// launch starts the helper app with the socket path.
	launch func(app, socket string) error
	// tmpDir is where the socket directory goes (os.TempDir).
	tmpDir func() string
	// now and sleep are the clock of the quick-relaunch rule.
	now   func() time.Time
	sleep func(time.Duration)
	// connectTimeout is how long a launched helper has to connect.
	connectTimeout time.Duration
}

// Host owns a process's capture helper: one helper per maestro-d process,
// launched with the first device opened and kept until the process exits
// (or the helper goes), shared by every device the process captures.
type Host struct {
	cfg hostConfig

	mu       sync.Mutex
	ln       net.Listener
	dir      string // the socket directory (holds last-launch)
	sockPath string
	helper   *helper            // the current helper, nil when none runs
	devices  map[string]*source // open devices by UDID
	changed  chan struct{}      // closed and replaced on every state change
}

// helper is one launched helper process, as the Host sees it.
type helper struct {
	ctrl              net.Conn // the control connection, once it said hello
	connected         chan struct{}
	pid               int
	version           string
	waitingPermission bool
	denied            error // the camera-permission answer it gave before exiting
	gone              bool
	pending           []*source // opens sent once the control connection is up
}

// launching reports whether the helper is still being built or launched.
func (hp *helper) launching() bool { return hp.ctrl == nil }

var (
	defaultOnce sync.Once
	defaultHost *Host
)

// Default is the process-wide Host.
func Default() *Host {
	defaultOnce.Do(func() {
		defaultHost = newHost(hostConfig{
			app:            ensureHelperApp,
			launch:         launchHelper,
			tmpDir:         os.TempDir,
			now:            time.Now,
			sleep:          time.Sleep,
			connectTimeout: 10 * time.Second,
		})
	})
	return defaultHost
}

func newHost(cfg hostConfig) *Host {
	return &Host{cfg: cfg, devices: map[string]*source{}, changed: make(chan struct{})}
}

// Open starts capturing a device on the process's helper. name is the
// device's name, which the helper matches when the capture device's
// unique ID is not the UDID.
func Open(udid, name string) *Device { return Default().Open(udid, name) }

// Device is one device's always-on capture.
type Device struct {
	src  *source
	sess *capture.Session
	err  error
}

// Open starts capturing udid: it registers the device, launches the
// helper when none runs, and starts the device's always-on session.
// Device.Wait then says whether it captures.
func (h *Host) Open(udid, name string) *Device {
	h.mu.Lock()
	if old := h.devices[udid]; old != nil {
		h.mu.Unlock()
		return &Device{err: &capture.Unavailable{Reason: capture.ReasonHelperFailed,
			Message: fmt.Sprintf("device %s is already being captured by this process", udid)}}
	}
	s := &source{h: h, udid: udid, name: name, connCh: make(chan net.Conn, 1), failed: make(chan struct{})}
	h.devices[udid] = s
	h.mu.Unlock()
	sess := capture.NewSession(s)
	if err := sess.Start(capture.Options{FPS: capture.DefaultFPS, Audio: true}); err != nil {
		h.closeSource(s)
		return &Device{err: err}
	}
	return &Device{src: s, sess: sess}
}

// Session is the device's always-on capture session, or nil when Open
// failed outright.
func (d *Device) Session() *capture.Session { return d.sess }

// Close ends the device's capture: the helper stops its capture session
// and closes the device's connection. The helper keeps running.
func (d *Device) Close() {
	if d.sess != nil {
		d.sess.Close()
	}
	if d.src != nil {
		d.src.h.closeSource(d.src)
	}
}

// Wait blocks until the device captures or its capture start ends. It
// waits readyWait, plus up to extendedWait while the helper is being
// built and launched or waits for Camera approval.
func (d *Device) Wait(ctx context.Context) Result {
	if d.err != nil {
		return Result{Err: d.err}
	}
	res := d.src.wait(ctx)
	if res.Err != nil && d.sess != nil {
		// The source's Run returns the failure a moment after wait sees it;
		// answer only once the session has recorded it, so its State and
		// Subscribe agree with the result.
		select {
		case <-d.sess.RunDone():
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
	return res
}

// source is one device's capture.Source on the Host. Its state is
// guarded by the Host's mutex.
type source struct {
	h          *Host
	udid, name string

	helper    *helper
	conn      net.Conn
	connCh    chan net.Conn // the device connection, once the helper opened it
	found     bool
	capturing bool
	at        time.Time
	match     string
	err       error         // terminal
	failed    chan struct{} // closed when err is set
	closed    bool
}

func (*source) Mode() capture.Mode { return capture.AlwaysOn }

// FPSControl is false: the device sends about 60 fps whatever is asked.
func (*source) FPSControl() bool { return false }

// failLocked records the source's terminal error once.
func (s *source) failLocked(err error) {
	if s.err != nil {
		return
	}
	s.err = err
	close(s.failed)
	if s.conn != nil {
		_ = s.conn.Close()
	}
	s.h.notifyLocked()
}

func (s *source) fail(err error) error {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	s.failLocked(err)
	return s.err
}

// Run opens the device on the helper, then turns its frames into
// packets until ctx ends or the capture fails.
func (s *source) Run(ctx context.Context, _ capture.Options, emit func(capture.Packet)) error {
	defer s.h.closeSource(s)
	if err := s.h.request(s); err != nil {
		return s.fail(err)
	}
	var conn net.Conn
	select {
	case conn = <-s.connCh:
	case <-s.failed:
		return s.fail(nil)
	case <-ctx.Done():
		return nil
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	audioOff := false
	var dec decoder
	for {
		f, err := readFrame(conn)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return s.ended(err)
		}
		if f.typ == frameStatus {
			if err := s.onStatus(f.payload); err != nil {
				return s.fail(err)
			}
			continue
		}
		if audioOff && (f.typ == frameAudioConfig || f.typ == frameAudioSample) {
			continue
		}
		p, ok, err := dec.packet(f)
		switch {
		case err != nil && (f.typ == frameAudioConfig || f.typ == frameAudioSample):
			logger.Warn("capture %s: audio dropped: %v", s.udid, err)
			audioOff = true
		case err != nil:
			return s.fail(&capture.Unavailable{Reason: capture.ReasonHelperFailed, Message: err.Error()})
		case ok:
			if p.Kind == capture.AudioConfig {
				logger.Info("capture %s: audio lpcm %s", s.udid, describeAudio(p.Audio, dec.planar))
			}
			emit(p)
		}
	}
}

// onStatus handles a status on the device's connection.
func (s *source) onStatus(payload []byte) error {
	st, err := parseStatus(payload)
	if err != nil {
		return &capture.Unavailable{Reason: capture.ReasonHelperFailed, Message: err.Error()}
	}
	h := s.h
	switch st.State {
	case stateDeviceFound:
		h.mu.Lock()
		s.found, s.at = true, h.cfg.now()
		h.notifyLocked()
		h.mu.Unlock()
		logger.Info("capture %s: device found (%s)", s.udid, st.Message)
	case stateCapturing:
		h.mu.Lock()
		s.found, s.capturing, s.at, s.match = true, true, h.cfg.now(), st.Match
		h.notifyLocked()
		h.mu.Unlock()
		logger.Info("capture %s: capturing (match: %s)", s.udid, st.Match)
	case stateUnavailable:
		reason := st.Reason
		if reason == "" {
			reason = capture.ReasonHelperFailed
		}
		return &capture.Unavailable{Reason: reason, Message: st.Message}
	}
	return nil
}

// ended answers a device connection that closed: the Host's reason when
// it gave one (the helper went), capture-lost after capturing began,
// else helper-failed.
func (s *source) ended(cause error) error {
	s.h.mu.Lock()
	defer s.h.mu.Unlock()
	if s.err == nil {
		msg := ""
		if cause != nil && !errors.Is(cause, io.EOF) {
			msg = ": " + cause.Error()
		}
		if s.capturing {
			s.failLocked(&capture.Unavailable{Reason: capture.ReasonCaptureLost,
				Message: fmt.Sprintf("the capture of %s stopped%s", s.udid, msg)})
		} else {
			s.failLocked(&capture.Unavailable{Reason: capture.ReasonHelperFailed,
				Message: fmt.Sprintf("the capture helper closed %s's connection%s", s.udid, msg)})
		}
	}
	return s.err
}

func (s *source) wait(ctx context.Context) Result {
	h := s.h
	start := h.cfg.now()
	var extra time.Duration
	var extraSince time.Time
	logged := false
	for {
		h.mu.Lock()
		res := Result{Found: s.found, At: s.at, Match: s.match, Err: s.err}
		done := s.capturing || s.err != nil
		hp := s.helper
		perm := hp != nil && !hp.gone && hp.waitingPermission
		ext := perm || hp == nil || (!hp.gone && hp.launching())
		ch := h.changed
		h.mu.Unlock()
		if done {
			return res
		}
		now := h.cfg.now()
		if ext && extraSince.IsZero() {
			extraSince = now
		} else if !ext && !extraSince.IsZero() {
			extra += now.Sub(extraSince)
			extraSince = time.Time{}
		}
		if perm && !logged {
			logger.Warn("approve camera access for MaestroDCapture on this Mac")
			logged = true
		}
		spent := extra
		if !extraSince.IsZero() {
			spent += now.Sub(extraSince)
		}
		var remaining time.Duration
		if ext {
			remaining = extendedWait - spent
		} else {
			remaining = readyWait - (now.Sub(start) - spent)
		}
		if remaining <= 0 {
			res.Err = s.timeout(perm, ext)
			return res
		}
		t := time.NewTimer(remaining)
		select {
		case <-ch:
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			res.Err = &capture.Unavailable{Reason: capture.ReasonHelperFailed, Message: ctx.Err().Error()}
			return res
		}
		t.Stop()
	}
}

func (s *source) timeout(perm, ext bool) error {
	switch {
	case perm:
		return &capture.Unavailable{Reason: capture.ReasonCameraPermission,
			Message: fmt.Sprintf("camera access for MaestroDCapture was not approved within %s", extendedWait)}
	case ext:
		return &capture.Unavailable{Reason: capture.ReasonHelperFailed,
			Message: fmt.Sprintf("the capture helper did not start within %s", extendedWait)}
	}
	return &capture.Unavailable{Reason: capture.ReasonHelperFailed,
		Message: fmt.Sprintf("%s did not start capturing within %s", s.udid, readyWait)}
}

// notifyLocked wakes every Wait.
func (h *Host) notifyLocked() {
	close(h.changed)
	h.changed = make(chan struct{})
}

// request asks the helper to open s, launching a helper when none runs.
func (h *Host) request(s *source) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s.closed {
		return nil
	}
	if err := h.listenLocked(); err != nil {
		return &capture.Unavailable{Reason: capture.ReasonHelperFailed, Message: "capture socket: " + err.Error()}
	}
	hp := h.helper
	if hp == nil {
		hp = &helper{connected: make(chan struct{})}
		h.helper = hp
		go h.launch(hp)
	}
	s.helper = hp
	if hp.launching() {
		hp.pending = append(hp.pending, s)
	} else {
		h.sendLocked(hp, controlOp{Op: "open", UDID: s.udid, Name: s.name})
	}
	h.notifyLocked()
	return nil
}

// closeSource forgets s and tells its helper to stop capturing it.
func (h *Host) closeSource(s *source) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if h.devices[s.udid] == s {
		delete(h.devices, s.udid)
	}
	if hp := s.helper; hp != nil {
		for i, p := range hp.pending {
			if p == s {
				hp.pending = append(hp.pending[:i], hp.pending[i+1:]...)
				break
			}
		}
		if !hp.gone && !hp.launching() {
			h.sendLocked(hp, controlOp{Op: "close", UDID: s.udid})
		}
	}
	if s.conn != nil {
		_ = s.conn.Close()
	}
}

// sendLocked writes one control line.
func (h *Host) sendLocked(hp *helper, op controlOp) {
	b, _ := json.Marshal(op)
	_ = hp.ctrl.SetWriteDeadline(time.Now().Add(writeWait))
	if _, err := hp.ctrl.Write(append(b, '\n')); err != nil {
		logger.Warn("capture helper: %s %s: %v", op.Op, op.UDID, err)
	}
}

// helperFailed fails every device waiting on hp and forgets it.
func (h *Host) helperFailed(hp *helper, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	hp.gone = true
	if h.helper == hp {
		h.helper = nil
	}
	for _, s := range h.devices {
		if s.helper == hp {
			s.failLocked(err)
		}
	}
	if hp.ctrl != nil {
		_ = hp.ctrl.Close()
	}
	h.notifyLocked()
	logger.Warn("capture helper: %v", err)
}

// serve accepts the helper's connections.
func (h *Host) serve(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go h.handshake(c)
	}
}

// handshake reads a new connection's hello: the control connection says
// its protocol, a device connection its UDID.
func (h *Host) handshake(c net.Conn) {
	_ = c.SetReadDeadline(time.Now().Add(handshakeWait))
	f, err := readFrame(c)
	_ = c.SetReadDeadline(time.Time{})
	var st status
	if err == nil && f.typ == frameStatus {
		st, err = parseStatus(f.payload)
	}
	if err != nil || f.typ != frameStatus || st.State != stateHello {
		_ = c.Close()
		return
	}
	if st.UDID == "" {
		h.control(c, st)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.devices[st.UDID]
	if s == nil || s.closed || s.conn != nil || s.err != nil || s.helper == nil || s.helper.gone {
		_ = c.Close()
		return
	}
	s.conn = c
	s.helper.waitingPermission = false
	s.connCh <- c
	h.notifyLocked()
}

// control adopts a helper's control connection.
func (h *Host) control(c net.Conn, st status) {
	if st.Protocol != ProtocolVersion {
		logger.Warn("capture helper speaks protocol %d, want %d", st.Protocol, ProtocolVersion)
		_ = c.Close()
		return
	}
	h.mu.Lock()
	hp := h.helper
	if hp == nil || hp.gone || !hp.launching() {
		h.mu.Unlock()
		_ = c.Close()
		return
	}
	hp.ctrl, hp.pid, hp.version = c, st.PID, st.Version
	close(hp.connected)
	for _, s := range hp.pending {
		h.sendLocked(hp, controlOp{Op: "open", UDID: s.udid, Name: s.name})
	}
	hp.pending = nil
	h.notifyLocked()
	h.mu.Unlock()
	logger.Info("capture helper pid %d (version %s) connected", st.PID, st.Version)
	go h.readControl(hp)
}

// readControl follows the helper's control statuses until it exits.
func (h *Host) readControl(hp *helper) {
	for {
		f, err := readFrame(hp.ctrl)
		if err != nil {
			break
		}
		if f.typ != frameStatus {
			continue
		}
		st, err := parseStatus(f.payload)
		if err != nil {
			continue
		}
		h.mu.Lock()
		switch st.State {
		case stateWaitingPermission:
			hp.waitingPermission = true
		case stateReady:
			hp.waitingPermission = false
		case stateUnavailable:
			hp.waitingPermission = false
			reason := st.Reason
			if reason == "" {
				reason = capture.ReasonHelperFailed
			}
			err := &capture.Unavailable{Reason: reason, Message: st.Message}
			if reason == capture.ReasonCameraPermission {
				hp.denied = err
			}
			for _, s := range h.devices {
				if s.helper == hp {
					s.failLocked(err)
				}
			}
		}
		h.notifyLocked()
		h.mu.Unlock()
	}
	h.mu.Lock()
	hp.gone = true
	if h.helper == hp {
		h.helper = nil
	}
	lost := error(&capture.Unavailable{Reason: capture.ReasonCaptureLost, Message: "the capture helper exited"})
	if hp.denied != nil {
		lost = hp.denied
	}
	for _, s := range h.devices {
		if s.helper == hp {
			s.failLocked(lost)
		}
	}
	_ = hp.ctrl.Close()
	h.notifyLocked()
	h.mu.Unlock()
	logger.Warn("capture helper pid %d exited", hp.pid)
}
