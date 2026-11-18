package capture_test

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/capture"
	"github.com/devicelab-dev/maestro-runner/pkg/capture/fake"
	"github.com/devicelab-dev/maestro-runner/pkg/capture/internal/probetest"
)

// scripted emits the clip's 45 frames at 15 fps (and the AAC fixture on its
// own clock when audio is asked for) at once, signals emitted, then waits
// for ctx, or ends with failAfter.
type scripted struct {
	frames    int // video frames to emit (deltas after the clip's first frame when > 45)
	emitted   chan struct{}
	failAfter error
}

func (s *scripted) Mode() capture.Mode { return capture.OnDemand }
func (s *scripted) FPSControl() bool   { return true }

func (s *scripted) Run(ctx context.Context, opts capture.Options, emit func(capture.Packet)) error {
	format, frames, err := fake.ParseClip(fake.Clip())
	if err != nil {
		return err
	}
	emit(capture.Packet{Kind: capture.VideoConfig, Video: format})
	aac := fake.AACFrames()
	if opts.Audio {
		emit(capture.Packet{Kind: capture.AudioConfig, Audio: &capture.AudioFormat{Codec: capture.CodecAAC, SampleRate: 48000, Channels: 2, ASC: fake.ASC()}})
	}
	base := 4 * time.Second // device clock: not zero
	j := 0
	for k := 0; k < s.frames; k++ {
		pts := time.Duration(k) * time.Second / 15
		for opts.Audio {
			apts := time.Duration(j) * 1024 * time.Second / 48000
			if apts > pts {
				break
			}
			emit(capture.Packet{Kind: capture.Audio, PTS: base + apts, Data: aac[j%len(aac)]})
			j++
		}
		f := frames[k%len(frames)]
		emit(capture.Packet{Kind: capture.Video, PTS: base + pts, Key: f.Key, Data: f.Data})
	}
	close(s.emitted)
	if s.failAfter != nil {
		return s.failAfter
	}
	<-ctx.Done()
	return nil
}

// silent never emits.
type silent struct{}

func (silent) Mode() capture.Mode { return capture.OnDemand }
func (silent) FPSControl() bool   { return true }
func (silent) Run(ctx context.Context, _ capture.Options, _ func(capture.Packet)) error {
	<-ctx.Done()
	return nil
}

func newScripted(frames int) (*scripted, *capture.Session) {
	src := &scripted{frames: frames, emitted: make(chan struct{})}
	return src, capture.NewSession(src)
}

func wait(t *testing.T, c <-chan struct{}) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out")
	}
}

// mfraEntries reads the tfra of track from the end of a finished file.
func mfraEntries(t *testing.T, file []byte, track uint32) [][2]uint64 {
	t.Helper()
	size := binary.BigEndian.Uint32(file[len(file)-4:])
	mfra := file[len(file)-int(size):]
	if string(mfra[4:8]) != "mfra" {
		t.Fatalf("no mfra at the end (%q)", mfra[4:8])
	}
	for off := 8; off < len(mfra); {
		n := int(binary.BigEndian.Uint32(mfra[off:]))
		if string(mfra[off+4:off+8]) == "tfra" && binary.BigEndian.Uint32(mfra[off+12:]) == track {
			count := int(binary.BigEndian.Uint32(mfra[off+20:]))
			var out [][2]uint64
			for i := 0; i < count; i++ {
				e := mfra[off+24+i*19:]
				out = append(out, [2]uint64{binary.BigEndian.Uint64(e), binary.BigEndian.Uint64(e[8:])})
			}
			return out
		}
		off += n
	}
	t.Fatalf("no tfra for track %d", track)
	return nil
}

// tfdtAt reads the tfdt of the moof at off.
func tfdtAt(t *testing.T, file []byte, off uint64) uint64 {
	t.Helper()
	if string(file[off+4:off+8]) != "moof" {
		t.Fatalf("no moof at %d", off)
	}
	// moof: mfhd (16 bytes), then traf: header 8, tfhd 16, tfdt (12-byte header).
	return binary.BigEndian.Uint64(file[off+8+16+8+16+12:])
}

// U11: the recorder writes a temp file in tmpDir and renames it to Stop's
// destination; the result passes ffprobe; mehd holds the duration and mfra
// lists every keyframe and audio fragment at its moof; Stop twice is a
// no-op.
func TestRecorder_FinalizesAndRenames(t *testing.T) {
	src, s := newScripted(45)
	defer s.Close()
	tmp := t.TempDir()
	r, err := capture.NewRecorder(s, tmp, capture.Options{FPS: 15, Audio: true})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, src.emitted)
	if r.Err() != nil {
		t.Fatalf("Err while recording: %v", r.Err())
	}
	dest := filepath.Join(t.TempDir(), "out", "clip.mp4")
	if err := r.Stop(dest); err != nil {
		t.Fatal(err)
	}
	if err := r.Stop(dest); err != nil {
		t.Errorf("second Stop: %v", err)
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("tmpDir still holds %v", left)
	}
	file, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}

	// mehd: version 1 fragment_duration in movie timescale (ms). The last of
	// 45 frames at 15 fps starts at 2.933 s and lasts 1/15 s: 3000 ms.
	moovAt := int(binary.BigEndian.Uint32(file))
	mehd := indexOf(file[:moovAt+int(binary.BigEndian.Uint32(file[moovAt:]))], "mehd")
	if got := binary.BigEndian.Uint64(file[mehd+12:]); got != 3000 {
		t.Errorf("mehd duration = %d ms, want 3000", got)
	}
	video := mfraEntries(t, file, 1)
	if len(video) != 3 {
		t.Fatalf("video tfra has %d entries, want 3 keyframes", len(video))
	}
	for i, e := range video {
		if e[0] != uint64(i)*90000 || tfdtAt(t, file, e[1]) != e[0] {
			t.Errorf("video entry %d = time %d at %d (tfdt there %d)", i, e[0], e[1], tfdtAt(t, file, e[1]))
		}
	}
	audio := mfraEntries(t, file, 2)
	if len(audio) < 130 {
		t.Fatalf("audio tfra has %d entries", len(audio))
	}
	for i, e := range audio {
		if tfdtAt(t, file, e[1]) != e[0] || (i > 0 && e[0] <= audio[i-1][0]) {
			t.Fatalf("audio entry %d = %v", i, e)
		}
	}

	res := probetest.Probe(t, dest)
	v, ok := res.Find("h264")
	if !ok || v.Width != 640 || v.Height != 360 || v.FrameCount() != 45 {
		t.Errorf("video = %+v", res.Streams)
	}
	if _, ok := res.Find("aac"); !ok {
		t.Errorf("no aac stream: %+v", res.Streams)
	}
	if d := res.Duration(); math.Abs(d-3.0) > 1.0/15 {
		t.Errorf("ffprobe duration %.3f s, want 3.000 within a frame", d)
	}
}

func indexOf(b []byte, typ string) int {
	for i := 0; i+4 <= len(b); i++ {
		if string(b[i:i+4]) == typ {
			return i - 4
		}
	}
	return -1
}

// Across filesystems the rename fails with EXDEV and the file is copied.
func TestRecorder_CopiesAcrossFilesystems(t *testing.T) {
	var renamed []string
	defer capture.SetRename(func(o, n string) error {
		renamed = append(renamed, n)
		return &os.LinkError{Op: "rename", Old: o, New: n, Err: syscall.EXDEV}
	})()
	src, s := newScripted(15)
	defer s.Close()
	tmp := t.TempDir()
	r, err := capture.NewRecorder(s, tmp, capture.Options{})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, src.emitted)
	dest := filepath.Join(t.TempDir(), "copied.mp4")
	if err := r.Stop(dest); err != nil {
		t.Fatal(err)
	}
	if len(renamed) != 1 || renamed[0] != dest {
		t.Errorf("rename calls = %v", renamed)
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("tmpDir still holds %v", left)
	}
	res := probetest.Probe(t, dest)
	if v, ok := res.Find("h264"); !ok || v.FrameCount() != 15 {
		t.Errorf("copied file = %+v", res.Streams)
	}

	// A copy into a directory path fails, and the temp file is removed.
	src2, s2 := newScripted(15)
	defer s2.Close()
	r2, err := capture.NewRecorder(s2, tmp, capture.Options{})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, src2.emitted)
	if err := r2.Stop(t.TempDir()); err == nil {
		t.Error("copy onto a directory succeeded")
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("tmpDir still holds %v", left)
	}
}

func TestRecorder_RenameAndDestErrors(t *testing.T) {
	defer capture.SetRename(func(o, n string) error { return errors.New("denied") })()
	tmp := t.TempDir()
	src, s := newScripted(15)
	defer s.Close()
	r, _ := capture.NewRecorder(s, tmp, capture.Options{})
	wait(t, src.emitted)
	if err := r.Stop(filepath.Join(t.TempDir(), "x.mp4")); err == nil || err.Error() != "denied" {
		t.Errorf("Stop = %v, want the rename error", err)
	}
	// A destination under a regular file cannot be created.
	file := filepath.Join(t.TempDir(), "file")
	os.WriteFile(file, nil, 0o644)
	src2, s2 := newScripted(15)
	defer s2.Close()
	r2, _ := capture.NewRecorder(s2, tmp, capture.Options{})
	wait(t, src2.emitted)
	if err := r2.Stop(filepath.Join(file, "x.mp4")); err == nil {
		t.Error("Stop under a file succeeded")
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("tmpDir still holds %v", left)
	}
}

// U11: a recorder that falls behind stops with ErrLagged; Stop keeps what
// it wrote and reports the error.
func TestRecorder_Lagged(t *testing.T) {
	release := make(chan struct{})
	defer capture.HoldRecorderLoops(release)()
	src, s := newScripted(4200) // more than the recorder's 4096-packet channel
	defer s.Close()
	r, err := capture.NewRecorder(s, t.TempDir(), capture.Options{})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, src.emitted)
	close(release)
	deadline := time.Now().Add(10 * time.Second)
	for r.Err() == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !errors.Is(r.Err(), capture.ErrLagged) {
		t.Fatalf("Err = %v, want ErrLagged", r.Err())
	}
	dest := filepath.Join(t.TempDir(), "lagged.mp4")
	if err := r.Stop(dest); !errors.Is(err, capture.ErrLagged) {
		t.Errorf("Stop = %v, want ErrLagged", err)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Errorf("lagged recording not kept: %v", err)
	}
}

// A source failure ends the recording early with the source's error; a
// recording with no video is refused and leaves nothing behind.
func TestRecorder_SourceFailureAndEmpty(t *testing.T) {
	src := &scripted{frames: 15, emitted: make(chan struct{}), failAfter: &capture.Unavailable{Reason: capture.ReasonScrcpyFailed}}
	s := capture.NewSession(src)
	defer s.Close()
	r, err := capture.NewRecorder(s, t.TempDir(), capture.Options{})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, src.emitted)
	deadline := time.Now().Add(10 * time.Second)
	for r.Err() == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if capture.ReasonOf(r.Err(), "") != capture.ReasonScrcpyFailed {
		t.Fatalf("Err = %v", r.Err())
	}
	if err := r.Stop(filepath.Join(t.TempDir(), "failed.mp4")); capture.ReasonOf(err, "") != capture.ReasonScrcpyFailed {
		t.Errorf("Stop = %v", err)
	}

	empty := &scripted{frames: 0, emitted: make(chan struct{})}
	es := capture.NewSession(empty)
	defer es.Close()
	tmp := t.TempDir()
	er, err := capture.NewRecorder(es, tmp, capture.Options{})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, empty.emitted)
	// The config alone starts the stream (video-only); a session that never
	// sends one has nothing to keep.
	if err := er.Stop(filepath.Join(t.TempDir(), "empty.mp4")); err != nil {
		t.Errorf("config-only recording: %v", err)
	}
	closed := capture.NewSession(silent{})
	never, err := capture.NewRecorder(closed, tmp, capture.Options{})
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	dest := filepath.Join(t.TempDir(), "never.mp4")
	if err := never.Stop(dest); err == nil {
		t.Error("a recording without video was kept")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("dest exists: %v", err)
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("tmpDir still holds %v", left)
	}
}

func TestNewRecorder_Errors(t *testing.T) {
	_, s := newScripted(0)
	if _, err := capture.NewRecorder(s, t.TempDir(), capture.Options{FPS: 1}); err == nil {
		t.Error("fps 1 accepted")
	}
	file := filepath.Join(t.TempDir(), "file")
	os.WriteFile(file, nil, 0o644)
	if _, err := capture.NewRecorder(s, file, capture.Options{}); err == nil {
		t.Error("tmpDir under a file accepted")
	}
	s.Close()
	tmp := t.TempDir()
	if _, err := capture.NewRecorder(s, tmp, capture.Options{}); !errors.Is(err, capture.ErrClosed) {
		t.Errorf("closed session: %v", err)
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("tmpDir still holds %v", left)
	}
}

// lateGOP emits the clip's first five frames as one GOP spread over 20 s
// (keyframe at 0 s, the newest delta at 19.9 s) with AAC throughout,
// signals cached, waits for live, then emits the clip's other 40 frames
// 75 ms apart (3 s, from 20.075 s) with AAC beside them, and signals
// emitted.
type lateGOP struct {
	cached, live, emitted chan struct{}
}

func (s *lateGOP) Mode() capture.Mode { return capture.OnDemand }
func (s *lateGOP) FPSControl() bool   { return true }

func (s *lateGOP) Run(ctx context.Context, _ capture.Options, emit func(capture.Packet)) error {
	format, frames, err := fake.ParseClip(fake.Clip())
	if err != nil {
		return err
	}
	emit(capture.Packet{Kind: capture.VideoConfig, Video: format})
	emit(capture.Packet{Kind: capture.AudioConfig, Audio: &capture.AudioFormat{Codec: capture.CodecAAC, SampleRate: 48000, Channels: 2, ASC: fake.ASC()}})
	base := 7 * time.Second // device clock: not zero
	aac := fake.AACFrames()
	j := 0
	audioUpTo := func(pts time.Duration) {
		for {
			apts := time.Duration(j) * 1024 * time.Second / 48000
			if apts > pts {
				return
			}
			emit(capture.Packet{Kind: capture.Audio, PTS: base + apts, Data: aac[j%len(aac)]})
			j++
		}
	}
	cachedAt := []time.Duration{0, 5 * time.Second, 10 * time.Second, 15 * time.Second, 19900 * time.Millisecond}
	for k, pts := range cachedAt {
		audioUpTo(pts)
		emit(capture.Packet{Kind: capture.Video, PTS: base + pts, Key: frames[k].Key, Data: frames[k].Data})
	}
	audioUpTo(20 * time.Second)
	close(s.cached)
	<-s.live
	for k := len(cachedAt); k < len(frames); k++ {
		pts := 20*time.Second + time.Duration(k-len(cachedAt)+1)*75*time.Millisecond
		audioUpTo(pts)
		emit(capture.Packet{Kind: capture.Video, PTS: base + pts, Key: frames[k].Key, Data: frames[k].Data})
	}
	close(s.emitted)
	<-ctx.Done()
	return nil
}

// U8 (late joiner): a recording started on a session whose last keyframe
// was 20 s earlier starts at the join, not at that keyframe. Its five
// cached frames are retimed 1 ms apart and every frame still decodes; the
// cached audio is dropped. A 3 s start→stop records ≈3 s, not ≈23 s.
func TestRecorder_LateJoinerStartsAtTheLiveEdge(t *testing.T) {
	src := &lateGOP{cached: make(chan struct{}), live: make(chan struct{}), emitted: make(chan struct{})}
	s := capture.NewSession(src)
	defer s.Close()
	now := time.Unix(1000, 0)
	capture.SetClock(s, func() time.Time { return now })
	first, err := s.Subscribe(capture.Options{Audio: true}) // starts the source
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	wait(t, src.cached)
	// The recording starts 100 ms after the newest cached frame (19.9 s)
	// arrived: the live edge is 20 s. (The edge follows the video only, so
	// the audio already at 20 s does not move it.)
	now = now.Add(100 * time.Millisecond)
	r, err := capture.NewRecorder(s, t.TempDir(), capture.Options{Audio: true})
	if err != nil {
		t.Fatal(err)
	}
	close(src.live)
	wait(t, src.emitted)
	dest := filepath.Join(t.TempDir(), "late.mp4")
	if err := r.Stop(dest); err != nil {
		t.Fatal(err)
	}
	file, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	// The keyframe sits at 0; the newest cached frame 4 ms later, at the
	// live edge (20 s); the first live frame 75 ms after that.
	if video := mfraEntries(t, file, 1); len(video) == 0 || video[0][0] != 0 {
		t.Fatalf("video tfra = %v", video)
	}
	// Audio starts at the live edge: no cached audio, none from before.
	audio := mfraEntries(t, file, 2)
	if len(audio) == 0 || audio[0][0] < 4*48 {
		t.Fatalf("first audio fragment at %v (48 kHz ticks), want ≥ 4 ms", audio)
	}
	if limit := 3100*48/1024 + 1; len(audio) > limit {
		t.Errorf("%d audio frames, want ≤ %d (only the live 3 s)", len(audio), limit)
	}

	res := probetest.Probe(t, dest)
	v, ok := res.Find("h264")
	if !ok || v.FrameCount() != 45 {
		t.Errorf("video = %+v, want all 45 frames decoded", res.Streams)
	}
	if _, ok := res.Find("aac"); !ok {
		t.Errorf("no aac stream: %+v", res.Streams)
	}
	// 4 ms of cached frames, 3 s of live ones, and the last frame's 75 ms.
	if d, want := res.Duration(), 0.004+3.0+0.075; math.Abs(d-want) > 0.05 {
		t.Errorf("ffprobe duration %.3f s, want ≈%.3f", d, want)
	}
}

// noAudio emits a video config and the clip's first five frames at 15 fps
// (a third of a second), never an audio config, then waits.
type noAudio struct{ emitted chan struct{} }

func (noAudio) Mode() capture.Mode { return capture.OnDemand }
func (noAudio) FPSControl() bool   { return true }
func (s noAudio) Run(ctx context.Context, _ capture.Options, emit func(capture.Packet)) error {
	format, frames, err := fake.ParseClip(fake.Clip())
	if err != nil {
		return err
	}
	emit(capture.Packet{Kind: capture.VideoConfig, Video: format})
	for k := 0; k < 5; k++ {
		emit(capture.Packet{Kind: capture.Video, PTS: time.Duration(k) * time.Second / 15, Key: frames[k].Key, Data: frames[k].Data})
	}
	close(s.emitted)
	<-ctx.Done()
	return nil
}

// A recording that asked for audio and stops before the audio config (or
// the second of video the mux waits for) keeps its video: Stop writes
// what was held, video-only, instead of answering "no video".
func TestRecorder_ShortRecordingWithoutAudioConfig(t *testing.T) {
	src := noAudio{emitted: make(chan struct{})}
	s := capture.NewSession(src)
	defer s.Close()
	r, err := capture.NewRecorder(s, t.TempDir(), capture.Options{Audio: true})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, src.emitted)
	dest := filepath.Join(t.TempDir(), "short.mp4")
	if err := r.Stop(dest); err != nil {
		t.Fatal(err)
	}
	res := probetest.Probe(t, dest)
	if v, ok := res.Find("h264"); !ok || v.FrameCount() != 5 {
		t.Errorf("video = %+v, want 5 frames", res.Streams)
	}
	if _, ok := res.Find("aac"); ok {
		t.Errorf("an audio stream without an audio config: %+v", res.Streams)
	}
}
