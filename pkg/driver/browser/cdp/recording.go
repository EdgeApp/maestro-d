package cdp

// maestro-d addition. This file is not part of upstream maestro-runner.
// Recording the browser's screen (core.FrameRecorder). startRecording
// starts the page's CDP screencast (Page.startScreencast) at the viewport's
// size and writes each JPEG frame to a temporary directory with the time it
// was drawn; stopRecording stops it and has FFmpeg encode the frames into
// an MP4, each shown for as long as it was on screen. The executor runs the
// steps (pkg/executor/recording.go) and names the file. See
// docs/daemon/streaming.md#recording-the-browser.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/core"
	"github.com/devicelab-dev/maestro-runner/pkg/flow"
	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// screencastQuality is the JPEG quality the browser encodes frames at.
const screencastQuality = 80

// minFrameDuration is the shortest time a frame is shown, so two frames
// with the same timestamp still both play.
const minFrameDuration = time.Millisecond

// frameSource is where a recording's frames come from: the page's
// screencast, or a fake in tests.
type frameSource interface {
	// start begins sending frames of at most w×h pixels to onFrame, each
	// with the time it was drawn, until the returned stop is called.
	start(w, h int, onFrame func(jpeg []byte, at time.Time)) (stop func() error, err error)
}

// pageScreencast is the frameSource of a page: its CDP screencast.
type pageScreencast struct{ page *rod.Page }

func (p pageScreencast) start(w, h int, onFrame func([]byte, time.Time)) (func() error, error) {
	ctx, cancel := context.WithCancel(context.Background())
	page := p.page.Context(ctx)
	wait := page.EachEvent(func(e *proto.PageScreencastFrame) {
		onFrame(e.Data, frameTime(e.Metadata, time.Now()))
		// The browser sends the next frame only after this one is acked.
		_ = proto.PageScreencastFrameAck{SessionID: e.SessionID}.Call(page)
	})
	done := make(chan struct{})
	go func() {
		wait()
		close(done)
	}()
	q, every := screencastQuality, 1
	err := proto.PageStartScreencast{
		Format: proto.PageStartScreencastFormatJpeg, Quality: &q,
		MaxWidth: &w, MaxHeight: &h, EveryNthFrame: &every,
	}.Call(p.page)
	if err != nil {
		cancel()
		<-done
		return nil, err
	}
	return func() error {
		err := proto.PageStopScreencast{}.Call(p.page)
		cancel()
		<-done
		return err
	}, nil
}

// frameTime is when a screencast frame was drawn: its swap timestamp, or
// now (when it arrived) when it has none or one more than a second away,
// a clock the recording's end could not be measured against.
func frameTime(m *proto.PageScreencastFrameMetadata, now time.Time) time.Time {
	if m == nil || m.Timestamp <= 0 {
		return now
	}
	at := m.Timestamp.Time()
	if d := now.Sub(at); d < -time.Second || d > time.Second {
		return now
	}
	return at
}

// recordedFrame is one frame on disk and the time it was drawn.
type recordedFrame struct {
	file string
	at   time.Time
}

// webRecording is a running recording of the browser.
type webRecording struct {
	dir  string // the frames' temporary directory
	w, h int    // the video's size: the viewport's, rounded down to even
	stop func() error

	mu     sync.Mutex
	frames []recordedFrame
	err    error // the first frame that could not be written
}

// add writes a frame to the recording's directory.
func (r *webRecording) add(jpeg []byte, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return
	}
	name := fmt.Sprintf("frame-%06d.jpg", len(r.frames)+1)
	if err := os.WriteFile(filepath.Join(r.dir, name), jpeg, 0o600); err != nil {
		r.err = err
		return
	}
	r.frames = append(r.frames, recordedFrame{file: name, at: at})
}

// lookPath finds a program on PATH; tests replace it.
var lookPath = exec.LookPath

// ffmpegPaths are where FFmpeg is looked for when it is not on PATH: a
// daemon started by launchd has a short PATH without Homebrew.
var ffmpegPaths = []string{"/opt/homebrew/bin/ffmpeg", "/usr/local/bin/ffmpeg", "/usr/bin/ffmpeg"}

// errNoFFmpeg is the error when no FFmpeg is found.
var errNoFFmpeg = errors.New("recording the browser needs FFmpeg, which was not found on PATH or in " +
	strings.Join(ffmpegPaths, ", ") + ": install FFmpeg (brew install ffmpeg, or apt-get install ffmpeg)")

// findFFmpeg returns the FFmpeg to encode with: the one on PATH, else the
// first of ffmpegPaths that exists.
func findFFmpeg() (string, error) {
	if p, err := lookPath("ffmpeg"); err == nil {
		return p, nil
	}
	for _, p := range ffmpegPaths {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	return "", errNoFFmpeg
}

// frameSource returns the source a recording takes its frames from.
func (d *Driver) frameSource() frameSource {
	if d.frames != nil {
		return d.frames
	}
	return pageScreencast{page: d.page}
}

// StartFrameRecording implements core.FrameRecorder: it starts the page's
// screencast at the viewport's size. It fails when a recording already
// runs or no FFmpeg is found, so a recording that cannot be encoded never
// starts.
func (d *Driver) StartFrameRecording() error {
	d.recMu.Lock()
	defer d.recMu.Unlock()
	if d.rec != nil {
		return errors.New("a recording is already running")
	}
	if _, err := findFFmpeg(); err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "maestro-d-web-recording-")
	if err != nil {
		return err
	}
	r := &webRecording{dir: dir, w: max(2, d.viewportW&^1), h: max(2, d.viewportH&^1)}
	stop, err := d.frameSource().start(d.viewportW, d.viewportH, r.add)
	if err != nil {
		_ = os.RemoveAll(dir)
		return fmt.Errorf("start the browser's screencast: %w", err)
	}
	r.stop = stop
	d.rec = r
	return nil
}

// StopFrameRecording implements core.FrameRecorder: it stops the
// screencast and encodes the frames into an MP4 at dest.
func (d *Driver) StopFrameRecording(dest string) (core.RecordingStats, error) {
	d.recMu.Lock()
	r := d.rec
	d.rec = nil
	d.recMu.Unlock()
	if r == nil {
		return core.RecordingStats{}, errors.New("no recording is running")
	}
	end := time.Now()
	stopErr := r.stop()
	defer func() { _ = os.RemoveAll(r.dir) }()
	r.mu.Lock()
	frames, werr := r.frames, r.err
	r.mu.Unlock()
	switch {
	case werr != nil:
		return core.RecordingStats{}, fmt.Errorf("write a frame: %w", werr)
	case len(frames) == 0 && stopErr != nil:
		return core.RecordingStats{}, fmt.Errorf("stop the browser's screencast: %w", stopErr)
	case len(frames) == 0:
		return core.RecordingStats{}, errors.New("the browser sent no screencast frames")
	}
	ffmpeg, err := findFFmpeg()
	if err != nil {
		return core.RecordingStats{}, err
	}
	list, total := concatList(frames, end)
	listPath := filepath.Join(r.dir, "frames.ffconcat")
	if err := os.WriteFile(listPath, []byte(list), 0o600); err != nil {
		return core.RecordingStats{}, err
	}
	if err := encodeMP4(ffmpeg, listPath, dest, r.w, r.h); err != nil {
		return core.RecordingStats{}, err
	}
	return core.RecordingStats{Duration: total, Frames: len(frames)}, nil
}

// frameOption sets each frame's image demuxer to a millisecond time base:
// at its default of 25 fps FFmpeg would round each frame's time to 40 ms
// and drop the frames that land on the same tick.
const frameOption = "option framerate 1000\n"

// concatList is the FFmpeg concat script that shows each frame until the
// next one was drawn, and the last until end, and the video's length.
func concatList(frames []recordedFrame, end time.Time) (string, time.Duration) {
	var b strings.Builder
	b.WriteString("ffconcat version 1.0\n")
	var total time.Duration
	for i, f := range frames {
		next := end
		if i+1 < len(frames) {
			next = frames[i+1].at
		}
		dur := max(next.Sub(f.at), minFrameDuration)
		total += dur
		fmt.Fprintf(&b, "file '%s'\n%sduration %.6f\n", f.file, frameOption, dur.Seconds())
	}
	// The concat demuxer drops the last entry's duration unless the file
	// is listed once more.
	fmt.Fprintf(&b, "file '%s'\n%s", frames[len(frames)-1].file, frameOption)
	return b.String(), total
}

// encodeMP4 runs FFmpeg on a concat script of JPEG frames: an H.264 MP4
// (FFmpeg's default MP4 encoder) of w×h, frames scaled to fit and padded,
// with the script's frame timing (MP4 keeps variable frame rates). There
// are no B-frames: with them FFmpeg reorders a variable-rate stream's
// decode times and the MP4 loses most of its length.
func encodeMP4(ffmpeg, list, dest string, w, h int) error {
	vf := fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2,format=yuv420p", w, h, w, h)
	tmp := dest + ".part"
	cmd := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "concat", "-safe", "0", "-i", list,
		"-vf", vf, "-bf", "0", "-movflags", "+faststart", "-f", "mp4", tmp)
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("ffmpeg: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return os.Rename(tmp, dest)
}

// discardRecording stops a recording left running when the driver closes,
// without encoding it.
func (d *Driver) discardRecording() {
	d.recMu.Lock()
	r := d.rec
	d.rec = nil
	d.recMu.Unlock()
	if r != nil {
		_ = r.stop()
		_ = os.RemoveAll(r.dir)
	}
}

// recordingStep answers startRecording and stopRecording when they reach
// the driver: the executor runs them through core.FrameRecorder.
func recordingStep(step flow.Step) *core.CommandResult {
	err := fmt.Errorf("%s runs in the executor, through the browser's screencast", step.Type())
	return errorResult(err, err.Error())
}
