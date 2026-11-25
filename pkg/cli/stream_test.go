package cli

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
	"github.com/devicelab-dev/maestro-runner/pkg/driver/mock"
	"github.com/devicelab-dev/maestro-runner/pkg/report"
)

// wholeBoxes reports whether b is a sequence of complete MP4 boxes
// starting with ftyp, every moof followed by its mdat.
func wholeBoxes(b []byte) bool {
	if len(b) < 8 || string(b[4:8]) != "ftyp" {
		return false
	}
	prev := ""
	for len(b) > 0 {
		if len(b) < 8 {
			return false
		}
		n := int(binary.BigEndian.Uint32(b))
		if n < 8 || n > len(b) {
			return false
		}
		typ := string(b[4:8])
		if prev == "moof" && typ != "mdat" {
			return false
		}
		prev, b = typ, b[n:]
	}
	return prev != "moof"
}

// streamDaemon is an in-process daemon with an attached mock device, and
// the stream command's seams pointed at it; out and errOut collect the
// command's stdout and stderr.
func streamDaemon(t *testing.T) (out, errOut *bytes.Buffer) {
	t.Helper()
	t.Setenv(daemon.EnvHome, t.TempDir())
	s := daemon.New(daemon.Config{Name: "stream-cli", ReportsDir: t.TempDir()}, daemon.Deps{
		Version: "test",
		NewDriver: func(ctx context.Context, id string, cfg daemon.AttachConfig) (core.Driver, daemon.DriverInfo, func(), error) {
			d := mock.New(mock.Config{Platform: "mock", DeviceID: id})
			return d, daemon.DriverInfo{DriverName: "mock", Device: report.Device{ID: id}, Platform: "mock"}, d.Close, nil
		},
		ListDevices: func(string) []daemon.DeviceInfo { return nil },
	})
	hs := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		hs.Close()
		s.Shutdown("test")
		<-s.Done()
	})
	c := daemon.NewHTTPClient(hs.URL, "", nil)
	if _, err := c.Attach(context.Background(), "mock-1", daemon.AttachConfig{Platform: "mock"}); err != nil {
		t.Fatal(err)
	}
	out, errOut = &bytes.Buffer{}, &bytes.Buffer{}
	oldTTY, oldOut, oldErr, oldSession := stdoutIsTerminal, streamStdout, streamStderr, streamSession
	t.Cleanup(func() {
		stdoutIsTerminal, streamStdout, streamStderr, streamSession = oldTTY, oldOut, oldErr, oldSession
	})
	stdoutIsTerminal = func() bool { return false }
	streamStdout = func() io.Writer { return out }
	streamStderr = func() io.Writer { return errOut }
	streamSession = func(ctx context.Context, o *daemonOpts) (*daemon.Client, string, error) { return c, o.Device, nil }
	return out, errOut
}

func streamOptsFor(t *testing.T, args ...string) *daemonOpts {
	t.Helper()
	o, p, err := parseDaemonArgs(args, streamFlags, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Positional) != 0 || len(p.Step) != 0 {
		t.Fatalf("leftovers: %+v", p)
	}
	return o
}

// U23
func TestStream_Flags(t *testing.T) {
	o := streamOptsFor(t, "--device", "mock-1", "--fps", "30", "--audio", "--duration", "4", "-o", "x.mp4")
	if o.Device != "mock-1" || o.stream != (streamOpts{fps: 30, audio: true, duration: 4}) || o.outPath != "x.mp4" {
		t.Fatalf("opts %+v %+v", o, o.stream)
	}
	if o = streamOptsFor(t, "--out", "-"); o.outPath != "-" || o.stream != (streamOpts{}) {
		t.Fatalf("defaults %+v", o.stream)
	}
}

func usageErr(t *testing.T, err error, sub string) {
	t.Helper()
	var de *daemon.Error
	if !errors.As(err, &de) || de.Code != daemon.CodeUsage || !strings.Contains(de.Message, sub) {
		t.Fatalf("want USAGE %q, got %v", sub, err)
	}
}

func TestStream_Stdout(t *testing.T) {
	for _, args := range [][]string{{"--duration", "1"}, {"--duration", "1", "-o", "-"}} {
		out, errOut := streamDaemon(t)
		o := streamOptsFor(t, append([]string{"--device", "mock-1"}, args...)...)
		start := time.Now()
		if err := runStream(context.Background(), o); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("--duration 1 took %v", d)
		}
		if !wholeBoxes(out.Bytes()) || out.Len() < 2000 {
			t.Fatalf("%v: stdout is not a whole stream (%d bytes)", args, out.Len())
		}
		if got := errOut.String(); !strings.HasPrefix(got, "stream: avc3.640016 fps=15 audio=false") {
			t.Fatalf("stderr %q", got)
		}
	}
}

func TestStream_Refusals(t *testing.T) {
	out, _ := streamDaemon(t)
	called := false
	streamSession = func(context.Context, *daemonOpts) (*daemon.Client, string, error) {
		called = true
		return nil, "", errors.New("no session expected")
	}
	stdoutIsTerminal = func() bool { return true }
	usageErr(t, runStream(context.Background(), streamOptsFor(t)), "stdout is a terminal; redirect it or pass -o <file>")
	usageErr(t, runStream(context.Background(), streamOptsFor(t, "-o", "-")), "stdout is a terminal")
	stdoutIsTerminal = func() bool { return false }
	usageErr(t, runStream(context.Background(), streamOptsFor(t, "--fps", "2")), "3 to 60")
	usageErr(t, runStream(context.Background(), streamOptsFor(t, "--fps", "61")), "3 to 60")
	usageErr(t, runStream(context.Background(), streamOptsFor(t, "--duration", "-1")), "negative")
	if called || out.Len() != 0 {
		t.Fatal("a refused stream reached the daemon or wrote")
	}
	// A session error is returned as is.
	if err := runStream(context.Background(), streamOptsFor(t, "-o", "x.mp4")); err == nil || err.Error() != "no session expected" {
		t.Fatalf("session error: %v", err)
	}
}

func TestStream_FileAndInterrupt(t *testing.T) {
	out, errOut := streamDaemon(t)
	dir := t.TempDir()
	// Ctrl-C (the command's context ending) stops it: exit 0, a whole file.
	file := filepath.Join(dir, "a.mp4")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(1500*time.Millisecond, cancel)
	if err := runStream(ctx, streamOptsFor(t, "--device", "mock-1", "--fps", "30", "--audio", "-o", file)); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file)
	if err != nil || !wholeBoxes(data) || len(data) < 2000 {
		t.Fatalf("file: %d bytes %v", len(data), err)
	}
	if !strings.Contains(out.String(), file) || !strings.Contains(errOut.String(), "fps=30 audio=true") {
		t.Fatalf("stdout %q stderr %q", out.String(), errOut.String())
	}
	out.Reset()
	if err := runStream(context.Background(), streamOptsFor(t, "--device", "mock-1", "--duration", "1", "--json", "-o", file)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"path"`) || !strings.Contains(out.String(), `"bytes"`) {
		t.Fatalf("--json: %s", out.String())
	}
	usageErr(t, runStream(context.Background(), streamOptsFor(t, "--device", "mock-1", "-o", filepath.Join(dir, "no", "such", "x.mp4"))), "no such")
	// A refused stream answers the daemon's error and makes no file.
	var de *daemon.Error
	refused := filepath.Join(dir, "refused.mp4")
	if err := runStream(context.Background(), streamOptsFor(t, "--device", "mock-9", "-o", refused)); !errors.As(err, &de) || de.Code != daemon.CodeDeviceNotAttached {
		t.Fatalf("unattached: %v", err)
	}
	if _, err := os.Stat(refused); !os.IsNotExist(err) {
		t.Fatalf("a refused stream left %s (%v)", refused, err)
	}
}
