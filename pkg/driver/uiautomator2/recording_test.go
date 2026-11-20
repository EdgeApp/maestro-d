package uiautomator2

// maestro-d: the driver's screen capture (recording.go), U33.

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/capture"
	"github.com/devicelab-dev/maestro-runner/pkg/capture/fake"
	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
)

// Without a session the driver answers unsupported-platform, and the
// recording steps that reach it fail saying so; with one, Capture returns
// that one session every time and --record records through it.
func TestCapture_SessionAndRecorder(t *testing.T) {
	d := New(&MockUIA2Client{}, &core.PlatformInfo{Platform: "android"}, nil)
	var _ core.Capturer = d
	var _ core.ScreenRecorder = d
	if _, err := d.Capture(); capture.ReasonOf(err, "") != capture.ReasonUnsupportedPlatform {
		t.Fatalf("Capture without a session: %v", err)
	}
	for _, step := range []flow.Step{&flow.StartRecordingStep{Path: "x"}, &flow.StopRecordingStep{}} {
		res := d.Execute(step)
		if res.Success || !strings.Contains(res.Message, "needs screen capture") || !strings.Contains(res.Message, "unsupported-platform") {
			t.Errorf("%T reaching the driver: %+v", step, res)
		}
	}
	if err := d.StartScreenRecording(); capture.ReasonOf(err, "") != capture.ReasonUnsupportedPlatform {
		t.Errorf("StartScreenRecording without a session: %v", err)
	}

	sess := capture.NewSession(fake.New())
	defer sess.Close()
	d.SetCapture(sess)
	for i := 0; i < 2; i++ {
		if got, err := d.Capture(); err != nil || got != sess {
			t.Fatalf("Capture = %p, %v; want the session %p", got, err, sess)
		}
	}
	if err := d.StartScreenRecording(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)
	dest := filepath.Join(t.TempDir(), "recording.mp4")
	if err := d.StopScreenRecording(dest); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 64 || string(data[4:8]) != "ftyp" || !bytes.Contains(data[len(data)-64:], []byte("mfro")) {
		t.Fatalf("not a finished fragmented MP4 (%d bytes)", len(data))
	}
	if err := d.StopScreenRecording(dest); err == nil {
		t.Error("a second stop succeeded")
	}
	if res := d.Execute(&flow.StopRecordingStep{}); res.Success || !strings.Contains(res.Message, "runs in the executor") {
		t.Errorf("stopRecording reaching a driver with capture: %+v", res)
	}
}

// No string in the driver's code runs screenrecord any more.
func TestNoScreenrecordLeft(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pkgs {
		for name, f := range p.Files {
			ast.Inspect(f, func(n ast.Node) bool {
				if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING && strings.Contains(lit.Value, "screenrecord") {
					t.Errorf("%s:%d: %s", name, fset.Position(lit.Pos()).Line, lit.Value)
				}
				return true
			})
		}
	}
}
