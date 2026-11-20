package probetest_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/capture/fake"
	"github.com/devicelab-dev/maestro-runner/pkg/capture/internal/probetest"
)

func TestProbe(t *testing.T) {
	p := filepath.Join(t.TempDir(), "clip.h264")
	if err := os.WriteFile(p, fake.RotatedClip(), 0o644); err != nil {
		t.Fatal(err)
	}
	r := probetest.Probe(t, p)
	s, ok := r.Find("h264")
	if !ok || s.Width != 360 || s.Height != 640 || s.FrameCount() != 15 {
		t.Errorf("streams = %+v", r.Streams)
	}
	if _, ok := r.Find("aac"); ok {
		t.Error("found aac in a video clip")
	}
	if r.Duration() != 0 { // a raw H.264 stream has no container duration
		t.Errorf("duration %q", r.Format.Duration)
	}
}
