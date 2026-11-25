package daemon

// stream.go: live screen streams (GET /v1/devices/{id}/stream), the
// device.capture events, and the viewer page (GET /v1/viewer) that plays a
// stream in a browser.

import (
	_ "embed"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/capture"
	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/logger"
)

// Stream response headers.
const (
	HeaderStreamFPS    = "X-Maestro-Fps"
	HeaderStreamAudio  = "X-Maestro-Audio"
	HeaderStreamCodecs = "X-Maestro-Codecs"
)

// streamFirstFrameWait is how long the stream route waits for a capture's
// first frames before it answers CAPTURE_UNAVAILABLE{no-frames}.
var streamFirstFrameWait = 10 * time.Second

// StreamWriteTimeout bounds each write of a stream's body: a client that
// stops reading (its connection open, its socket buffers full) fails the
// write instead of blocking the handler, which then ends the stream. A
// hub relaying a peer's stream gives its own writes the same bound.
const StreamWriteTimeout = 10 * time.Second

// streamWriteTimeout is StreamWriteTimeout, shortened by tests.
var streamWriteTimeout = StreamWriteTimeout

// unavailableErr is CAPTURE_UNAVAILABLE for err, with details.reason (the
// reason of the *capture.Unavailable in err's chain, else fallback).
func unavailableErr(err error, fallback string) *Error {
	reason := capture.ReasonOf(err, fallback)
	msg := ""
	var u *capture.Unavailable
	switch {
	case errors.As(err, &u):
		msg = u.Error()
	case err != nil:
		msg = (&capture.Unavailable{Reason: reason, Message: err.Error()}).Error()
	default:
		msg = (&capture.Unavailable{Reason: reason}).Error()
	}
	return Errorf(CodeCaptureUnavailable, "%s", msg).WithDetails("reason", reason)
}

// captureOf returns the capture session of an attached device's driver, or
// CAPTURE_UNAVAILABLE naming why it has none.
func (a *attached) captureOf() (*capture.Session, *Error) {
	c, ok := core.Unwrap(a.session.Driver()).(core.Capturer)
	if !ok {
		driver := a.info.Driver
		if driver == "" {
			driver = "this"
		}
		return nil, unavailableErr(&capture.Unavailable{Reason: capture.ReasonUnsupportedPlatform,
			Message: fmt.Sprintf("device %s (%s driver) has no screen capture", a.id, driver)}, "")
	}
	sess, err := c.Capture()
	if err != nil || sess == nil {
		return nil, unavailableErr(err, capture.ReasonUnsupportedPlatform)
	}
	return sess, nil
}

// watchCapture publishes the device.capture events of a device that has
// screen capture: its state when it changes (capturing, unavailable,
// lost; an idle stop is not published), or unavailable once when its
// driver answers that it has none. The returned func stops it.
func (s *Server) watchCapture(id string, d core.Driver) func() {
	c, ok := core.Unwrap(d).(core.Capturer)
	if !ok {
		return func() {}
	}
	sess, err := c.Capture()
	if err != nil || sess == nil {
		s.events.Publish(Event{Type: "device.capture", Device: id, Message: capture.StateUnavailable.String(),
			Error: capture.ReasonOf(err, capture.ReasonUnsupportedPlatform)})
		return func() {}
	}
	return sess.OnState(func(st capture.State, reason string) {
		switch st {
		case capture.StateCapturing, capture.StateUnavailable, capture.StateLost:
			s.events.Publish(Event{Type: "device.capture", Device: id, Message: st.String(), Error: reason})
		}
	})
}

// handleStream serves GET /v1/devices/{id}/stream?fps=&audio=: the device's
// screen as fragmented MP4 (chunked), the init segment then one fragment
// per packet, until the client leaves, the device is detached, the
// subscriber falls behind, a write cannot drain within streamWriteTimeout
// (the client stopped reading) or the capture fails. It never takes the
// device's busy lock, so steps run beside it.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	a := s.requireAttached(w, r)
	if a == nil {
		return
	}
	q := r.URL.Query()
	fps, err := capture.ParseFPS(q.Get("fps"))
	if err != nil {
		WriteError(w, Errorf(CodeUsage, "%v", err).WithDetails("field", "fps"))
		return
	}
	audio := false
	if v := q.Get("audio"); v != "" {
		if audio, err = strconv.ParseBool(v); err != nil {
			WriteError(w, Errorf(CodeUsage, "audio must be true or false, got %q", v).WithDetails("field", "audio"))
			return
		}
	}
	sess, e := a.captureOf()
	if e != nil {
		WriteError(w, e)
		return
	}
	sub, err := sess.Subscribe(capture.Options{FPS: fps, Audio: audio})
	if err != nil {
		WriteError(w, unavailableErr(err, capture.ReasonCaptureLost))
		return
	}
	defer sub.Close()
	s.idle.Hold()
	defer s.idle.Release()

	ctx, base := r.Context(), a.baseCtx()
	mux := capture.NewMux(sub.Options())
	first := time.NewTimer(streamFirstFrameWait)
	defer first.Stop()
	var head []capture.Chunk
	for !mux.Started() {
		select {
		case p, ok := <-sub.Packets():
			if !ok {
				WriteError(w, unavailableErr(sub.Err(), capture.ReasonCaptureLost))
				return
			}
			chunks, err := mux.Push(p)
			if err != nil {
				logger.Debug("stream %s: %v", a.id, err)
			}
			head = append(head, chunks...)
		case <-first.C:
			WriteError(w, unavailableErr(&capture.Unavailable{Reason: capture.ReasonNoFrames,
				Message: fmt.Sprintf("no frames from the screen capture of %s within %v", a.id, streamFirstFrameWait)}, ""))
			return
		case <-ctx.Done():
			return
		case <-base.Done():
			WriteError(w, Errorf(CodeDeviceNotAttached, "device %s was detached", a.id))
			return
		}
	}

	h := w.Header()
	h.Set("Content-Type", "video/mp4")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	if sess.FPSControl() {
		h.Set(HeaderStreamFPS, strconv.Itoa(sub.Options().FPS))
	} else {
		h.Set(HeaderStreamFPS, "device")
	}
	h.Set(HeaderStreamAudio, strconv.FormatBool(mux.HasAudio()))
	h.Set(HeaderStreamCodecs, mux.Codecs())
	w.WriteHeader(http.StatusOK)
	// Each write gets its own deadline (through leaseGate.Unwrap under a
	// lease), so a client that stops reading cannot hold the handler, its
	// idle hold or its lease. The deadline is cleared at the end, for the
	// connection's next request.
	rc := http.NewResponseController(w)
	defer func() { _ = rc.SetWriteDeadline(time.Time{}) }()
	write := func(chunks []capture.Chunk) error {
		for _, c := range chunks {
			if err := rc.SetWriteDeadline(time.Now().Add(streamWriteTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
				return err
			}
			if _, err := w.Write(c.Data); err != nil {
				return err
			}
			if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
				return err
			}
		}
		return nil
	}
	logger.Info("stream %s: started (%s fps=%s audio=%t)", a.id, mux.Codecs(), h.Get(HeaderStreamFPS), mux.HasAudio())
	why := "client left"
	defer func() { logger.Info("stream %s: ended (%s)", a.id, why) }()
	if err := write(head); err != nil {
		why = writeFailure(err)
		return
	}
	for {
		select {
		case p, ok := <-sub.Packets():
			if !ok {
				why = fmt.Sprint(sub.Err())
				return
			}
			chunks, err := mux.Push(p)
			if err != nil {
				logger.Debug("stream %s: %v", a.id, err)
			}
			if err := write(chunks); err != nil {
				why = writeFailure(err)
				return
			}
		case <-ctx.Done():
			return
		case <-base.Done():
			why = "device detached"
			return
		}
	}
}

// writeFailure is the logged reason a stream ended on a failed write.
func writeFailure(err error) string {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return fmt.Sprintf("client too slow: a write did not drain within %v", streamWriteTimeout)
	}
	return "write: " + err.Error()
}

//go:embed viewer.html
var viewerHTML []byte

// IsViewerRequest reports whether r asks for the viewer page, which carries
// no device data and is served without a token, by the daemon and by a
// hub's router: GET /v1/viewer exactly, no prefix.
func IsViewerRequest(r *http.Request) bool {
	return r.Method == http.MethodGet && r.URL.Path == "/v1/viewer"
}

// handleViewer serves the viewer page. It reads the device, token and
// stream options from its URL fragment, which never reaches a server.
func (s *Server) handleViewer(w http.ResponseWriter, _ *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Length", strconv.Itoa(len(viewerHTML)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(viewerHTML)
}
