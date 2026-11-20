package executor

// maestro-d: U34. An Android driver reaches the executor wrapped by
// flutter.Wrap, as CreateAndroidDriver returns it; the recording steps
// still go through its capture session, never through the wrapper.

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/capture"
	"github.com/devicelab-dev/maestro-runner/pkg/capture/fake"
	"github.com/devicelab-dev/maestro-runner/pkg/core"
	uia2driver "github.com/devicelab-dev/maestro-runner/pkg/driver/uiautomator2"
	"github.com/devicelab-dev/maestro-runner/pkg/flutter"
)

func TestRecording_AndroidDriverThroughFlutterWrap(t *testing.T) {
	inner := uia2driver.New(nil, &core.PlatformInfo{Platform: "android", DeviceID: "SERIAL"}, nil)
	sess := capture.NewSession(fake.New())
	defer sess.Close()
	inner.SetCapture(sess)
	wrapped := flutter.Wrap(inner, nil, nil, "com.example", "")
	if _, ok := wrapped.(core.Capturer); ok {
		t.Fatal("the Flutter wrapper forwards Capturer; the unwrap is untested")
	}
	dir := t.TempDir()
	s, err := NewSession(context.Background(), wrapped, RunnerConfig{OutputDir: dir, Artifacts: ArtifactOnFailure}, SessionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	out := s.Execute(ctx, mustStep(t, "startRecording: /sdcard/android"))
	if !out.Success || out.Data != "recordings/android.mp4" {
		t.Fatalf("start: %+v %v", out.Error, out.Data)
	}
	time.Sleep(1200 * time.Millisecond)
	out = s.Execute(ctx, mustStep(t, "stopRecording"))
	if !out.Success || out.Data != "recordings/android.mp4" {
		t.Fatalf("stop: %+v %v", out.Error, out.Data)
	}
	checkMP4(t, filepath.Join(dir, "recordings", "android.mp4"))
}
