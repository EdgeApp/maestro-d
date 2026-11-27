package cdp

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/flow"
	"github.com/go-rod/rod/lib/proto"
)

// testJPEG is a w×h JPEG with a white square at x.
func testJPEG(t *testing.T, w, h, x int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for py := 0; py < h; py++ {
		for px := 0; px < w; px++ {
			c := color.RGBA{0, 0, 80, 255}
			if px >= x && px < x+20 && py >= h/2-10 && py < h/2+10 {
				c = color.RGBA{255, 255, 255, 255}
			}
			img.Set(px, py, c)
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// fakeFrames is a frameSource that sends its frames, spaced by gap with
// timestamps from start, as soon as it starts.
type fakeFrames struct {
	frames   [][]byte
	gap      time.Duration
	startErr error
	stopErr  error
	gotW     int
	gotH     int
	stopped  bool
}

func (f *fakeFrames) start(w, h int, onFrame func([]byte, time.Time)) (func() error, error) {
	f.gotW, f.gotH = w, h
	if f.startErr != nil {
		return nil, f.startErr
	}
	at := time.Now()
	for _, fr := range f.frames {
		onFrame(fr, at)
		at = at.Add(f.gap)
	}
	return func() error {
		f.stopped = true
		return f.stopErr
	}, nil
}

// needFFmpeg skips a test that encodes when this machine has no FFmpeg.
func needFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := findFFmpeg(); err != nil {
		t.Skip(err)
	}
}

// probe returns ffprobe's duration in seconds and frame count of an MP4.
func probe(t *testing.T, path string) (float64, int) {
	t.Helper()
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("no ffprobe")
	}
	out, err := exec.Command(ffprobe, "-v", "error", "-count_frames", "-select_streams", "v:0",
		"-show_entries", "format=duration:stream=nb_read_frames,codec_name,width,height", "-of", "default=nw=1", path).CombinedOutput()
	if err != nil {
		t.Fatalf("ffprobe: %v\n%s", err, out)
	}
	var dur float64
	var frames int
	for _, line := range strings.Split(string(out), "\n") {
		k, v, _ := strings.Cut(line, "=")
		switch k {
		case "duration":
			dur, _ = strconv.ParseFloat(v, 64)
		case "nb_read_frames":
			frames, _ = strconv.Atoi(v)
		}
	}
	return dur, frames
}

func TestFrameRecording_EncodesFramesWithTheirTiming(t *testing.T) {
	needFFmpeg(t)
	var frames [][]byte
	// 60 frames 15 ms apart: closer than FFmpeg's default 40 ms tick.
	for i := 0; i < 60; i++ {
		frames = append(frames, testJPEG(t, 320, 240, i*5))
	}
	src := &fakeFrames{frames: frames, gap: 15 * time.Millisecond}
	d := &Driver{viewportW: 321, viewportH: 240, frames: src}
	if err := d.StartFrameRecording(); err != nil {
		t.Fatal(err)
	}
	if src.gotW != 321 || src.gotH != 240 {
		t.Fatalf("screencast size %dx%d", src.gotW, src.gotH)
	}
	if err := d.StartFrameRecording(); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second start: %v", err)
	}
	dir := d.rec.dir
	// The frames span 0.885 s from the first; the last lasts until the stop.
	time.Sleep(1200 * time.Millisecond)
	dest := filepath.Join(t.TempDir(), "web.mp4")
	stats, err := d.StopFrameRecording(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !src.stopped || stats.Frames != 60 || stats.Duration < 1100*time.Millisecond || stats.Duration > 2*time.Second {
		t.Fatalf("stopped %v, stats %+v", src.stopped, stats)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the frames' directory is left: %v", err)
	}
	dur, n := probe(t, dest)
	if dur < 1.0 || dur > 2.1 || n < 60 {
		t.Fatalf("ffprobe: duration %.2f s, %d frames", dur, n)
	}
	if _, err := d.StopFrameRecording(dest); err == nil || err.Error() != "no recording is running" {
		t.Fatalf("stop again: %v", err)
	}
}

func TestFrameRecording_NoFFmpeg(t *testing.T) {
	oldLook, oldPaths := lookPath, ffmpegPaths
	t.Cleanup(func() { lookPath, ffmpegPaths = oldLook, oldPaths })
	lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	ffmpegPaths = []string{filepath.Join(t.TempDir(), "ffmpeg"), t.TempDir()}
	d := &Driver{viewportW: 100, viewportH: 100, frames: &fakeFrames{}}
	err := d.StartFrameRecording()
	if !errors.Is(err, errNoFFmpeg) || !strings.Contains(err.Error(), "FFmpeg") {
		t.Fatalf("start: %v", err)
	}
	if d.rec != nil {
		t.Fatal("a recording started")
	}
	// FFmpeg removed while recording: the stop names it.
	lookPath = oldLook
	needFFmpeg(t)
	d.frames = &fakeFrames{frames: [][]byte{testJPEG(t, 100, 100, 0)}}
	if err := d.StartFrameRecording(); err != nil {
		t.Fatal(err)
	}
	lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	if _, err := d.StopFrameRecording(filepath.Join(t.TempDir(), "x.mp4")); !errors.Is(err, errNoFFmpeg) {
		t.Fatalf("stop: %v", err)
	}
}

func TestFindFFmpeg_KnownPaths(t *testing.T) {
	oldLook, oldPaths := lookPath, ffmpegPaths
	t.Cleanup(func() { lookPath, ffmpegPaths = oldLook, oldPaths })
	lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	known := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(known, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	ffmpegPaths = []string{t.TempDir(), known}
	if p, err := findFFmpeg(); err != nil || p != known {
		t.Fatalf("findFFmpeg = %q, %v", p, err)
	}
	lookPath = func(string) (string, error) { return "/on/path/ffmpeg", nil }
	if p, err := findFFmpeg(); err != nil || p != "/on/path/ffmpeg" {
		t.Fatalf("findFFmpeg = %q, %v", p, err)
	}
}

// withFFmpeg makes findFFmpeg answer path.
func withFFmpeg(t *testing.T, path string) {
	t.Helper()
	old := lookPath
	t.Cleanup(func() { lookPath = old })
	lookPath = func(string) (string, error) { return path, nil }
}

func TestFrameRecording_Failures(t *testing.T) {
	withFFmpeg(t, "/usr/bin/false")
	dest := filepath.Join(t.TempDir(), "x.mp4")
	frame := testJPEG(t, 50, 50, 0)

	d := &Driver{viewportW: 50, viewportH: 50, frames: &fakeFrames{startErr: errors.New("no page")}}
	if err := d.StartFrameRecording(); err == nil || !strings.Contains(err.Error(), "screencast: no page") || d.rec != nil {
		t.Fatalf("start error: %v", err)
	}

	for _, tc := range []struct {
		name string
		src  *fakeFrames
		want string
	}{
		{"no frames", &fakeFrames{}, "the browser sent no screencast frames"},
		{"no frames, stop failed", &fakeFrames{stopErr: errors.New("gone")}, "stop the browser's screencast: gone"},
		{"ffmpeg fails", &fakeFrames{frames: [][]byte{frame}}, "ffmpeg: exit status 1"},
	} {
		d := &Driver{viewportW: 50, viewportH: 50, frames: tc.src}
		if err := d.StartFrameRecording(); err != nil {
			t.Fatal(err)
		}
		if _, err := d.StopFrameRecording(dest); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
			t.Fatalf("%s: the partial file is left", tc.name)
		}
	}

	// A frame that cannot be written fails the stop.
	d = &Driver{viewportW: 50, viewportH: 50, frames: &fakeFrames{}}
	if err := d.StartFrameRecording(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(d.rec.dir); err != nil {
		t.Fatal(err)
	}
	d.rec.add(frame, time.Now())
	d.rec.add(frame, time.Now())
	if _, err := d.StopFrameRecording(dest); err == nil || !strings.Contains(err.Error(), "write a frame") {
		t.Fatalf("write error: %v", err)
	}

	// The concat script cannot be written: its directory is gone.
	d = &Driver{viewportW: 50, viewportH: 50, frames: &fakeFrames{}}
	if err := d.StartFrameRecording(); err != nil {
		t.Fatal(err)
	}
	d.rec.frames = []recordedFrame{{file: "frame-000001.jpg", at: time.Now()}}
	if err := os.Remove(d.rec.dir); err != nil {
		t.Fatal(err)
	}
	if _, err := d.StopFrameRecording(dest); err == nil {
		t.Fatal("stop without a frames directory succeeded")
	}
}

func TestFrameRecording_DiscardedOnClose(t *testing.T) {
	withFFmpeg(t, "/usr/bin/false")
	src := &fakeFrames{frames: [][]byte{testJPEG(t, 50, 50, 0)}}
	d := &Driver{viewportW: 50, viewportH: 50, frames: src}
	d.discardRecording() // none running
	if err := d.StartFrameRecording(); err != nil {
		t.Fatal(err)
	}
	dir := d.rec.dir
	d.discardRecording()
	if _, err := os.Stat(dir); !src.stopped || d.rec != nil || !os.IsNotExist(err) {
		t.Fatalf("stopped %v, rec %v, dir %v", src.stopped, d.rec, err)
	}
}

func TestConcatList(t *testing.T) {
	t0 := time.Unix(1000, 0)
	frames := []recordedFrame{{"a.jpg", t0}, {"b.jpg", t0.Add(250 * time.Millisecond)}, {"c.jpg", t0.Add(250 * time.Millisecond)}}
	list, total := concatList(frames, t0.Add(time.Second))
	o := frameOption
	want := "ffconcat version 1.0\nfile 'a.jpg'\n" + o + "duration 0.250000\nfile 'b.jpg'\n" + o + "duration 0.001000\nfile 'c.jpg'\n" + o + "duration 0.750000\nfile 'c.jpg'\n" + o
	if list != want || total != 1001*time.Millisecond {
		t.Fatalf("list %q, total %v", list, total)
	}
}

func TestFrameTime(t *testing.T) {
	now := time.Unix(2000, 0)
	swap := proto.TimeSinceEpoch(1999.5)
	for _, tc := range []struct {
		m    *proto.PageScreencastFrameMetadata
		want time.Time
	}{
		{nil, now},
		{&proto.PageScreencastFrameMetadata{}, now},
		{&proto.PageScreencastFrameMetadata{Timestamp: swap}, time.Unix(1999, 5e8)},
		{&proto.PageScreencastFrameMetadata{Timestamp: 1000}, now},
		{&proto.PageScreencastFrameMetadata{Timestamp: 3000}, now},
	} {
		if got := frameTime(tc.m, now); !got.Equal(tc.want) {
			t.Errorf("frameTime(%+v) = %v, want %v", tc.m, got, tc.want)
		}
	}
}

func TestRecordingStepsRunInTheExecutor(t *testing.T) {
	d := &Driver{}
	for _, step := range []flow.Step{&flow.StartRecordingStep{BaseStep: flow.BaseStep{StepType: flow.StepStartRecording}}, &flow.StopRecordingStep{BaseStep: flow.BaseStep{StepType: flow.StepStopRecording}}} {
		res := d.Execute(step)
		if res.Success || !strings.Contains(res.Message, "runs in the executor, through the browser's screencast") {
			t.Fatalf("%T: %+v", step, res)
		}
	}
}

// The page's own screencast: a real browser records a moving square.
func TestFrameRecording_RealBrowser(t *testing.T) {
	needFFmpeg(t)
	ts := newTestServer()
	defer ts.Close()
	d := newTestDriver(t, ts.URL+"/moving")
	defer d.Close()
	if err := d.StartFrameRecording(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	dest := filepath.Join(t.TempDir(), "page.mp4")
	stats, err := d.StopFrameRecording(dest)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Frames < 5 || stats.Duration < time.Second || stats.Duration > 2*time.Second {
		t.Fatalf("stats %+v", stats)
	}
	if dur, n := probe(t, dest); dur < 1.0 || n < 5 {
		t.Fatalf("ffprobe: duration %.2f s, %d frames", dur, n)
	}

	// A closed page cannot start a screencast.
	_ = d.page.Close()
	if err := d.StartFrameRecording(); err == nil {
		t.Fatal("started on a closed page")
	}
}
