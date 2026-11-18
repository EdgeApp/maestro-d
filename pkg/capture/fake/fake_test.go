package fake

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/capture"
	"github.com/devicelab-dev/maestro-runner/pkg/capture/h264"
)

func TestFixtures(t *testing.T) {
	format, frames, err := ParseClip(Clip())
	if err != nil {
		t.Fatal(err)
	}
	if format.Width != 640 || format.Height != 360 || len(frames) != 45 {
		t.Fatalf("clip = %dx%d, %d frames", format.Width, format.Height, len(frames))
	}
	for i, f := range frames {
		if f.Key != (i%15 == 0) {
			t.Errorf("frame %d key = %v", i, f.Key)
		}
		nalus, err := h264.SplitAVCC(f.Data)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range nalus {
			if typ := h264.NALType(n); typ == h264.NALSPS || typ == h264.NALPPS || typ == h264.NALAUD {
				t.Errorf("frame %d carries NAL type %d", i, typ)
			}
		}
	}
	rf, rframes, err := ParseClip(RotatedClip())
	if err != nil || rf.Width != 360 || rf.Height != 640 || len(rframes) != 15 {
		t.Fatalf("rotated clip = %+v, %d frames, %v", rf, len(rframes), err)
	}
	if n := len(AACFrames()); n < 40 {
		t.Errorf("%d AAC frames", n)
	}
	if asc := ASC(); len(asc) != 2 || asc[0] != 0x11 || asc[1] != 0x90 {
		t.Errorf("ASC = %x", asc)
	}
	if n := len(PCM()); n != 48000*2*2/2 {
		t.Errorf("PCM is %d bytes, want 0.5 s", n)
	}
}

func TestParseClip_Rejects(t *testing.T) {
	sps, pps := h264.ParameterSets(h264.SplitAnnexB(Clip()))
	pSlice := []byte{0x41, 0x9a, 0x00}
	for name, nalus := range map[string][][]byte{
		"no parameter sets": {pSlice},
		"bad sps":           {{0x67, 0x64, 0, 0x1e}, pps[0], {0x65, 0x88}},
		"no keyframe first": {{0x09, 0x10}, sps[0], pps[0], {0x09, 0x10}, pSlice},
	} {
		if _, _, err := ParseClip(h264.JoinAnnexB(nalus)); err == nil {
			t.Errorf("%s: ParseClip succeeded", name)
		}
	}
}

// collect runs src unpaced until n packets were emitted, then cancels.
func collect(t *testing.T, opts capture.Options, n int) []capture.Packet {
	t.Helper()
	src := New()
	var mu sync.Mutex
	var slept []time.Duration
	src.sleep = func(ctx context.Context, _ time.Time, d time.Duration) error {
		mu.Lock()
		slept = append(slept, d)
		mu.Unlock()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out []capture.Packet
	err := src.Run(ctx, opts, func(p capture.Packet) {
		out = append(out, p)
		if len(out) == n {
			cancel()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	// Each packet is emitted after sleeping until its PTS.
	for i := 1; i < len(slept); i++ {
		if slept[i] < slept[i-1] {
			t.Fatalf("sleep %d (%v) before sleep %d (%v)", i, slept[i], i-1, slept[i-1])
		}
	}
	return out
}

// U12: the clip loops at the requested fps with PTS monotonic across the
// loop (at 15 fps the second pass starts at 3 s); each pass starts at the
// keyframe; ctx cancel ends Run with nil.
func TestSource_LoopsAtFPS(t *testing.T) {
	src := New()
	if src.Mode() != capture.OnDemand || !src.FPSControl() {
		t.Fatal("mode/fps control")
	}
	pkts := collect(t, capture.Options{FPS: 15}, 1+100)
	if pkts[0].Kind != capture.VideoConfig || pkts[0].Video.Width != 640 {
		t.Fatalf("first packet = %+v", pkts[0])
	}
	video := pkts[1:]
	for i, p := range video {
		if p.Kind != capture.Video {
			t.Fatalf("packet %d is %v without audio", i, p.Kind)
		}
		if want := time.Duration(i) * time.Second / 15; p.PTS != want {
			t.Errorf("frame %d PTS = %v, want %v", i, p.PTS, want)
		}
		if p.Key != (i%15 == 0) {
			t.Errorf("frame %d key = %v", i, p.Key)
		}
	}
	if video[45].PTS != 3*time.Second || !video[45].Key || video[90].PTS != 6*time.Second {
		t.Errorf("pass 2 starts at %v (key %v), pass 3 at %v", video[45].PTS, video[45].Key, video[90].PTS)
	}
	if string(video[45].Data) != string(video[0].Data) {
		t.Error("pass 2 does not replay the clip")
	}
}

func TestSource_AudioOnItsOwnClock(t *testing.T) {
	pkts := collect(t, capture.Options{FPS: 30, Audio: true}, 2+400)
	if pkts[1].Kind != capture.AudioConfig || pkts[1].Audio.Codec != capture.CodecAAC || pkts[1].Audio.SampleRate != 48000 || pkts[1].Audio.Channels != 2 {
		t.Fatalf("second packet = %+v", pkts[1])
	}
	frames := AACFrames()
	var vk, ak int
	var last time.Duration
	for _, p := range pkts[2:] {
		if p.PTS < last {
			t.Fatalf("PTS went back: %v after %v", p.PTS, last)
		}
		last = p.PTS
		switch p.Kind {
		case capture.Video:
			if want := time.Duration(vk) * time.Second / 30; p.PTS != want {
				t.Errorf("video %d PTS = %v, want %v", vk, p.PTS, want)
			}
			vk++
		case capture.Audio:
			if want := time.Duration(ak) * 1024 * time.Second / 48000; p.PTS != want {
				t.Errorf("audio %d PTS = %v, want %v", ak, p.PTS, want)
			}
			if string(p.Data) != string(frames[ak%len(frames)]) {
				t.Errorf("audio %d is not fixture frame %d", ak, ak%len(frames))
			}
			ak++
		}
	}
	if ak <= len(frames) || vk == 0 {
		t.Errorf("%d audio frames (the fixture has %d), %d video", ak, len(frames), vk)
	}
}

// Real pacing: about fps frames a second, and cancel returns promptly.
func TestSource_RealTimeAndCancel(t *testing.T) {
	src := New()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var n int
	start := time.Now()
	if err := src.Run(ctx, capture.Options{FPS: 60}, func(p capture.Packet) {
		if p.Kind == capture.Video {
			n++
		}
	}); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("Run returned %v after the deadline", took)
	}
	if n < 5 || n > 30 {
		t.Errorf("%d frames in 300 ms at 60 fps", n)
	}
	if err := (&Source{}).Run(context.Background(), capture.Options{FPS: 2}, func(capture.Packet) {}); err == nil {
		t.Error("fps 2 accepted")
	}
	// A zero Source paces in real time too; an expired context ends it at once.
	done, stop := context.WithCancel(context.Background())
	stop()
	if err := (&Source{}).Run(done, capture.Options{}, func(capture.Packet) {}); err != nil {
		t.Error(err)
	}
}
