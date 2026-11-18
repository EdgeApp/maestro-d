package capture_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/capture"
	"github.com/devicelab-dev/maestro-runner/pkg/capture/internal/probetest"
)

func TestDriverCapture(t *testing.T) {
	var c capture.DriverCapture
	if _, err := c.Session(); capture.ReasonOf(err, "") != capture.ReasonUnsupportedPlatform {
		t.Fatalf("Session without one: %v", err)
	}
	if err := c.StartRecording(); capture.ReasonOf(err, "") != capture.ReasonUnsupportedPlatform {
		t.Fatalf("StartRecording without a session: %v", err)
	}
	if err := c.StopRecording(filepath.Join(t.TempDir(), "x.mp4")); err == nil || !strings.Contains(err.Error(), "no screen recording") {
		t.Fatalf("StopRecording with none: %v", err)
	}

	src, s := newScripted(30)
	defer s.Close()
	c.Set(s)
	if got, err := c.Session(); err != nil || got != s {
		t.Fatalf("Session = %v, %v", got, err)
	}
	if err := c.StartRecording(); err != nil {
		t.Fatal(err)
	}
	if err := c.StartRecording(); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second StartRecording: %v", err)
	}
	wait(t, src.emitted)
	dest := filepath.Join(t.TempDir(), "rec.mp4")
	if err := c.StopRecording(dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatal(err)
	}
	res := probetest.Probe(t, dest)
	if _, ok := res.Find("h264"); !ok {
		t.Errorf("no h264 in %+v", res.Streams)
	}

	// A recorder that cannot be made (the temp directory is a file).
	tmp := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(tmp, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmp)
	if err := c.StartRecording(); err == nil {
		t.Fatal("StartRecording with an unusable temp directory succeeded")
	} else if errors.Is(err, capture.ErrClosed) {
		t.Fatal(err)
	}
}
