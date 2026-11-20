package wda

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/capture"
	"github.com/devicelab-dev/maestro-runner/pkg/capture/fake"
	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

// iosSource is an always-on source like the QuickTime one: it loops the
// fake clip at about 60 fps with LPCM audio until cancelled, or fails
// when told to.
type iosSource struct {
	fail chan error
}

func (*iosSource) Mode() capture.Mode { return capture.AlwaysOn }
func (*iosSource) FPSControl() bool   { return false }

func (s *iosSource) Run(ctx context.Context, _ capture.Options, emit func(capture.Packet)) error {
	format, frames, err := fake.ParseClip(fake.Clip())
	if err != nil {
		return err
	}
	emit(capture.Packet{Kind: capture.VideoConfig, Video: format})
	emit(capture.Packet{Kind: capture.AudioConfig, Audio: &capture.AudioFormat{Codec: capture.CodecLPCM, SampleRate: 48000, Channels: 2, BitsPerSample: 16, LittleEndian: true}})
	pcm := fake.PCM()
	t := time.NewTicker(time.Second / 60)
	defer t.Stop()
	for k := 0; ; k++ {
		pts := time.Duration(k) * time.Second / 60
		fr := frames[k%len(frames)]
		emit(capture.Packet{Kind: capture.Video, PTS: pts, Key: fr.Key, Data: fr.Data})
		emit(capture.Packet{Kind: capture.Audio, PTS: pts, Data: pcm[:3200]})
		select {
		case <-ctx.Done():
			return nil
		case err := <-s.fail:
			return err
		case <-t.C:
		}
	}
}

func physicalDriver(t *testing.T, url string) (*Driver, *iosSource, *capture.Session) {
	t.Helper()
	d := NewDriver(NewClientURL(url), &core.PlatformInfo{Platform: "ios", DeviceID: "phone"}, "phone")
	src := &iosSource{fail: make(chan error, 1)}
	sess := capture.NewSession(src)
	if err := sess.Start(capture.Options{Audio: true}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sess.Close)
	d.SetCapture(sess, nil)
	return d, src, sess
}

// U43: Capture answers by platform and by the capture CreateIOSDriver
// handed over.
func TestCapture_Answers(t *testing.T) {
	reason := func(d *Driver) string {
		s, err := d.Capture()
		if s != nil {
			return "session"
		}
		return capture.ReasonOf(err, "")
	}
	tv := NewDriver(nil, &core.PlatformInfo{}, "tv")
	tv.SetTVMode(true)
	sim := NewDriver(nil, &core.PlatformInfo{IsSimulator: true}, "sim")
	off := NewDriver(nil, &core.PlatformInfo{}, "phone")
	failed := NewDriver(nil, &core.PlatformInfo{}, "phone")
	failed.SetCapture(nil, &capture.Unavailable{Reason: capture.ReasonCameraPermission})
	for d, want := range map[*Driver]string{
		tv:     capture.ReasonUnsupportedPlatform,
		sim:    capture.ReasonSimulator,
		off:    capture.ReasonUnsupportedPlatform,
		failed: capture.ReasonCameraPermission,
	} {
		if got := reason(d); got != want {
			t.Errorf("%s: %s, want %s", d.udid, got, want)
		}
	}
	var c core.Capturer = off
	_ = c

	d, _, sess := physicalDriver(t, "http://127.0.0.1:1")
	if s, err := d.Capture(); err != nil || s != sess {
		t.Fatalf("physical: %v %v", s, err)
	}
}

// U43: a physical device records --record through its capture session;
// a simulator still records through simctl.
func TestCapture_PhysicalScreenRecorder(t *testing.T) {
	d, _, _ := physicalDriver(t, "http://127.0.0.1:1")
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir) // where capture.DriverCapture writes until the stop
	if err := d.StopScreenRecording(filepath.Join(dir, "none.mp4")); err == nil || err.Error() != "no screen recording is running" {
		t.Fatalf("stop before a start: %v", err)
	}
	if err := d.StartScreenRecording(); err != nil {
		t.Fatal(err)
	}
	if tmp, _ := filepath.Glob(filepath.Join(dir, ".maestro-d-recording-*.mp4")); len(tmp) != 1 {
		t.Fatalf("temporary recording files in TMPDIR: %v", tmp)
	}
	// Review 2.harness-review.9: the same text as every capture driver's.
	if err := d.StartScreenRecording(); err == nil || err.Error() != "a screen recording is already running" {
		t.Fatalf("second start: %v", err)
	}
	time.Sleep(400 * time.Millisecond)
	out := filepath.Join(t.TempDir(), "recording.mp4")
	if err := d.StopScreenRecording(out); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(out); err != nil || fi.Size() < 1000 {
		t.Fatalf("recording: %v %v", fi, err)
	}
	if b, _ := os.ReadFile(out); !strings.Contains(string(b[:64]), "ftyp") || !strings.Contains(string(b), "ipcm") {
		t.Fatal("not an MP4 with an ipcm track")
	}
	if err := d.StopScreenRecording(out); err == nil {
		t.Fatal("stop without a recording")
	}

	off := NewDriver(nil, &core.PlatformInfo{}, "phone")
	if err := off.StartScreenRecording(); capture.ReasonOf(err, "") != capture.ReasonUnsupportedPlatform {
		t.Fatalf("no capture: %v", err)
	}

	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "xcrun"), []byte("#!/bin/sh\necho simctl-called >&2\nexit 1\n"), 0o755)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	sim := NewDriver(nil, &core.PlatformInfo{IsSimulator: true}, "sim")
	sim.SetCapture(nil, nil)
	if err := sim.StartScreenRecording(); err == nil || !strings.Contains(err.Error(), "recordVideo") {
		t.Fatalf("simulator must use simctl: %v", err)
	}
}

// U44: once the capture is lost, a step that fails with a network error
// carries the re-attach hint; other failures do not.
func TestCapture_LostHint(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	url := server.URL
	server.Close() // every request is refused
	d, src, sess := physicalDriver(t, url)

	before := d.Execute(&flow.PressKeyStep{Key: "home"})
	if before.Success || strings.Contains(before.Message, captureLostHint) {
		t.Fatalf("before the loss: %+v", before)
	}

	src.fail <- &capture.Unavailable{Reason: capture.ReasonCaptureLost, Message: "the capture helper exited"}
	deadline := time.Now().Add(5 * time.Second)
	for !d.captureLost.Load() {
		if time.Now().After(deadline) {
			t.Fatal("captureLost not set")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if st, reason := sess.State(); st != capture.StateLost || reason != capture.ReasonCaptureLost {
		t.Fatalf("session %v %s", st, reason)
	}
	if _, err := d.Capture(); capture.ReasonOf(err, "") != capture.ReasonCaptureLost {
		t.Fatalf("Capture after the loss: %v", err)
	}

	res := d.Execute(&flow.PressKeyStep{Key: "home"})
	if res.Success || !strings.Contains(res.Message, captureLostHint) || !strings.Contains(res.Error.Error(), captureLostHint) {
		t.Fatalf("network failure: %+v", res)
	}
	other := d.Execute(&flow.StartRecordingStep{})
	if other.Success || strings.Contains(other.Message, captureLostHint) {
		t.Fatalf("non-network failure: %+v", other)
	}

	// Once is enough, and a message-less failure gets the hint alone.
	r := &core.CommandResult{Message: "x (" + captureLostHint + ")", Error: errors.New("connection refused")}
	d.addCaptureLostHint(r)
	if strings.Count(r.Message, captureLostHint) != 1 {
		t.Fatal(r.Message)
	}
	r = &core.CommandResult{Error: &net.OpError{Op: "dial", Err: errors.New("x")}}
	d.addCaptureLostHint(r)
	if r.Message != captureLostHint {
		t.Fatal(r.Message)
	}
	r = &core.CommandResult{Message: "read: EOF"}
	d.addCaptureLostHint(r)
	if !strings.Contains(r.Error.Error(), captureLostHint) {
		t.Fatal(r)
	}
	d.addCaptureLostHint(nil)
}

func TestIsNetworkError(t *testing.T) {
	for _, r := range []*core.CommandResult{
		{Error: syscall.ECONNREFUSED},
		{Error: &net.OpError{Op: "read", Err: syscall.ECONNRESET}},
		{Message: "Post http://x: dial tcp 127.0.0.1:8100: connect: connection refused"},
		{Message: "unexpected EOF"},
		{Error: errors.New("write: broken pipe")},
	} {
		if !isNetworkError(r) {
			t.Errorf("not network: %+v", r)
		}
	}
	for _, r := range []*core.CommandResult{
		{Message: "Element 'Geoff' not found"},
		{Error: errors.New("no visible element")},
	} {
		if isNetworkError(r) {
			t.Errorf("network: %+v", r)
		}
	}
}

// SetCapture again replaces the session and its listener.
func TestSetCaptureReplaces(t *testing.T) {
	d, _, _ := physicalDriver(t, "http://127.0.0.1:1")
	d.SetCapture(nil, &capture.Unavailable{Reason: capture.ReasonHelperFailed})
	if _, err := d.Capture(); capture.ReasonOf(err, "") != capture.ReasonHelperFailed {
		t.Fatal(err)
	}
}
