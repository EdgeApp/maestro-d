package cli

// stream.go: `maestro-d stream`, a device's live screen as fragmented MP4
// on stdout or in a file.

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/urfave/cli/v2"

	"github.com/devicelab-dev/maestro-runner/pkg/capture"
	"github.com/devicelab-dev/maestro-runner/pkg/cli/common"
	"github.com/devicelab-dev/maestro-runner/pkg/daemon"
)

// streamOpts are stream's switches.
type streamOpts struct {
	fps      int
	audio    bool
	duration int // seconds; 0 = until interrupted or the stream ends
}

var streamFlags = common.FlagSet(daemonSelectFlags, daemonSpawnFlags, common.One(deviceFlag), attachFlags, targetFlags, []dflag{
	common.IntFlag("fps", "Requested frame rate, 3 to 60 (default 15): a ceiling on Android, advisory on iOS", nil,
		func(o *daemonOpts, v int) { o.stream.fps = v }),
	common.BoolFlag("audio", "Include the device's audio when it has any", nil, func(o *daemonOpts, v bool) { o.stream.audio = v }),
	common.IntFlag("duration", "Stop after this many seconds (default: until Ctrl-C or the stream ends)", nil,
		func(o *daemonOpts, v int) { o.stream.duration = v }),
	common.StringFlag("out", "Write the stream to this file ('-' or none: stdout)", nil, func(o *daemonOpts, v string) { o.outPath = v }, "o"),
})

var daemonStreamCommand = &cli.Command{
	Name:  "stream",
	Usage: "Stream the device's screen as fragmented MP4 (stdout or -o FILE)",
	Description: common.FlagHelp(`  maestro-d stream --duration 10 -o screen.mp4
  maestro-d stream --fps 30 --audio > screen.mp4
  maestro-d stream | ffplay -
Ctrl-C (or --duration) ends the stream cleanly: the file ends on a whole
fragment and plays. The stream's codecs, frame rate and audio go to stderr.`, streamFlags),
	SkipFlagParsing: true,
	Action: withArgs(streamFlags, func(ctx context.Context, o *daemonOpts, args []string) error {
		if len(args) != 0 {
			return daemon.Errorf(daemon.CodeUsage, "usage: maestro-d stream [--device ID] [--fps N] [--audio] [--duration S] [-o FILE|-]")
		}
		return runStream(ctx, o)
	}),
}

// Seams tests replace: whether stdout is a terminal, the stream's stdout
// and stderr, and how the command reaches the device.
var (
	stdoutIsTerminal = func() bool {
		fi, err := os.Stdout.Stat()
		return err == nil && fi.Mode()&os.ModeCharDevice != 0
	}
	streamStdout  = func() io.Writer { return os.Stdout }
	streamStderr  = func() io.Writer { return os.Stderr }
	streamSession = session
)

func runStream(ctx context.Context, o *daemonOpts) error {
	if o.stream.fps != 0 {
		if _, err := (capture.Options{FPS: o.stream.fps}).Normalize(); err != nil {
			return daemon.Errorf(daemon.CodeUsage, "--%v", err).WithDetails("field", "fps")
		}
	}
	if o.stream.duration < 0 {
		return daemon.Errorf(daemon.CodeUsage, "--duration must not be negative")
	}
	toStdout := o.outPath == "" || o.outPath == "-"
	if toStdout && stdoutIsTerminal() {
		return daemon.Errorf(daemon.CodeUsage, "stdout is a terminal; redirect it or pass -o <file>")
	}
	client, id, err := streamSession(ctx, o)
	if err != nil {
		return err
	}
	// The file is made with the stream's first bytes, so a refused stream
	// leaves no empty file behind (nor truncates an existing one).
	var lf *lazyFile
	w := streamStdout()
	if !toStdout {
		lf = &lazyFile{path: o.outPath}
		defer lf.close()
		w = lf
	}
	cw := &countWriter{w: w}
	_, err = client.Stream(ctx, id, daemon.StreamOptions{
		FPS:      o.stream.fps,
		Audio:    o.stream.audio,
		Duration: time.Duration(o.stream.duration) * time.Second,
		OnStart: func(info daemon.StreamInfo) {
			fmt.Fprintf(streamStderr(), "stream: %s fps=%s audio=%t\n", info.Codecs, info.FPS, info.Audio)
		},
	}, cw)
	if err != nil {
		return err
	}
	if lf == nil {
		return nil
	}
	if err := lf.close(); err != nil {
		return daemon.WrapErr(daemon.CodeUsage, err)
	}
	abs, _ := filepath.Abs(o.outPath)
	if o.JSON {
		common.PrintJSON(streamStdout(), map[string]any{"ok": true, "exitCode": 0, "path": abs, "bytes": cw.n})
	} else {
		fmt.Fprintln(streamStdout(), abs)
	}
	return nil
}

// countWriter counts the bytes written through it.
type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// lazyFile creates its file on the first write.
type lazyFile struct {
	path string
	f    *os.File
}

func (l *lazyFile) Write(p []byte) (int, error) {
	if l.f == nil {
		f, err := os.Create(l.path)
		if err != nil {
			return 0, err
		}
		l.f = f
	}
	return l.f.Write(p)
}

// close closes the file once; it is a no-op when nothing was written.
func (l *lazyFile) close() error {
	if l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	return f.Close()
}
