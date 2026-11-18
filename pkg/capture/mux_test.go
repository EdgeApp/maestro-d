package capture_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/capture"
	"github.com/devicelab-dev/maestro-runner/pkg/capture/fake"
	"github.com/devicelab-dev/maestro-runner/pkg/capture/h264"
)

func fixture(t *testing.T, annexB []byte) (*capture.VideoFormat, []fake.Frame) {
	t.Helper()
	f, frames, err := fake.ParseClip(annexB)
	if err != nil {
		t.Fatal(err)
	}
	return f, frames
}

var aacFormat = &capture.AudioFormat{Codec: capture.CodecAAC, SampleRate: 48000, Channels: 2, ASC: []byte{0x11, 0x90}}

func push(t *testing.T, x *capture.Mux, p capture.Packet) []capture.Chunk {
	t.Helper()
	c, err := x.Push(p)
	if err != nil {
		t.Fatalf("Push(%v): %v", p.Kind, err)
	}
	return c
}

func TestMux_VideoOnlyStartsAtConfig(t *testing.T) {
	format, frames := fixture(t, fake.Clip())
	x := capture.NewMux(capture.Options{})
	if x.Started() || x.HasAudio() || x.VideoFormat() != nil || x.MehdOffset() != 0 || x.Duration() != 0 || x.Codecs() != "" {
		t.Fatal("fresh Mux reports a stream")
	}
	got := push(t, x, capture.Packet{Kind: capture.VideoConfig, Video: format})
	if len(got) != 1 || !got[0].Init || !bytes.HasPrefix(got[0].Data[4:], []byte("ftyp")) {
		t.Fatalf("config gave %d chunks", len(got))
	}
	if !x.Started() || x.HasAudio() || x.VideoFormat() != format || x.MehdOffset() == 0 {
		t.Fatal("not started")
	}
	sps, _ := h264.CodecString(format.SPS[0])
	if x.Codecs() != sps {
		t.Errorf("Codecs = %q, want %q", x.Codecs(), sps)
	}
	got = push(t, x, capture.Packet{Kind: capture.Video, PTS: time.Second, Key: true, Data: frames[0].Data})
	if len(got) != 1 || got[0].Init || got[0].Track != 1 || !got[0].Key || got[0].Time != 0 {
		t.Fatalf("keyframe chunk = %+v", got)
	}
	// Audio, an audio config and unknown kinds produce nothing on a
	// video-only stream; a later video config changes the in-band SPS.
	for _, p := range []capture.Packet{
		{Kind: capture.AudioConfig, Audio: aacFormat},
		{Kind: capture.Audio, PTS: time.Second, Data: []byte{1}},
		{Kind: capture.Kind(99)},
	} {
		if got := push(t, x, p); len(got) != 0 {
			t.Errorf("%v gave %d chunks", p.Kind, len(got))
		}
	}
	rotated, rframes := fixture(t, fake.RotatedClip())
	if got := push(t, x, capture.Packet{Kind: capture.VideoConfig, Video: rotated}); len(got) != 0 {
		t.Fatal("config after start gave chunks")
	}
	got = push(t, x, capture.Packet{Kind: capture.Video, PTS: 2 * time.Second, Key: true, Data: rframes[0].Data})
	if !bytes.Contains(got[0].Data, rotated.SPS[0]) {
		t.Error("keyframe after the format change lacks the new SPS")
	}
	if _, err := x.Push(capture.Packet{Kind: capture.Video, PTS: 3 * time.Second, Data: []byte{0, 0, 0, 9}}); err == nil {
		t.Error("truncated sample muxed")
	}
	if x.Duration() == 0 {
		t.Error("Duration 0 after samples")
	}
}

// With audio asked for, media waits for the audio config, then the init
// segment carries both tracks and the held-back packets follow it.
func TestMux_WaitsForAudioConfig(t *testing.T) {
	format, frames := fixture(t, fake.Clip())
	x := capture.NewMux(capture.Options{FPS: 15, Audio: true})
	for _, p := range []capture.Packet{
		{Kind: capture.VideoConfig, Video: format},
		{Kind: capture.Video, PTS: 0, Key: true, Data: frames[0].Data},
		{Kind: capture.Audio, PTS: 0, Data: []byte{1, 2}},
	} {
		if got := push(t, x, p); len(got) != 0 {
			t.Fatalf("%v gave chunks before the audio config", p.Kind)
		}
	}
	got := push(t, x, capture.Packet{Kind: capture.AudioConfig, Audio: aacFormat})
	if len(got) != 3 || !got[0].Init || got[1].Track != 1 || got[2].Track != 2 {
		t.Fatalf("start gave %+v", got)
	}
	if !x.HasAudio() || !strings.HasSuffix(x.Codecs(), ",mp4a.40.2") {
		t.Errorf("HasAudio %v, Codecs %q", x.HasAudio(), x.Codecs())
	}
	if got := push(t, x, capture.Packet{Kind: capture.Audio, PTS: 30 * time.Millisecond, Data: []byte{3}}); len(got) != 1 || got[0].Track != 2 {
		t.Errorf("audio after start = %+v", got)
	}
}

// A floating-point LPCM config is carried as delivered: an fpcm track whose
// pcmC says 32 bits, and the fpcm codec string.
func TestMux_FloatPCMIsFpcm(t *testing.T) {
	format, frames := fixture(t, fake.Clip())
	x := capture.NewMux(capture.Options{FPS: 15, Audio: true})
	push(t, x, capture.Packet{Kind: capture.VideoConfig, Video: format})
	push(t, x, capture.Packet{Kind: capture.Video, PTS: 0, Key: true, Data: frames[0].Data})
	f32 := &capture.AudioFormat{Codec: capture.CodecLPCM, SampleRate: 48000, Channels: 2, BitsPerSample: 32, LittleEndian: true, Float: true}
	got := push(t, x, capture.Packet{Kind: capture.AudioConfig, Audio: f32})
	if len(got) != 2 || !got[0].Init {
		t.Fatalf("start gave %+v", got)
	}
	if !bytes.Contains(got[0].Data, []byte("fpcm")) || !bytes.Contains(got[0].Data, []byte("pcmC\x00\x00\x00\x00\x01\x20")) {
		t.Error("init segment has no fpcm entry with a little-endian 32-bit pcmC")
	}
	if !strings.HasSuffix(x.Codecs(), ",fpcm") {
		t.Errorf("Codecs %q, want an fpcm suffix", x.Codecs())
	}
	if got := push(t, x, capture.Packet{Kind: capture.Audio, PTS: 20 * time.Millisecond, Data: fake.FloatPCM()[:960*8]}); len(got) != 1 || got[0].Track != 2 {
		t.Errorf("audio after start = %+v", got)
	}
}

// Without an audio config after a second of video, the stream starts
// video-only; a pending-packet cap bounds the wait.
func TestMux_StartsWithoutAudioAfterASecond(t *testing.T) {
	format, frames := fixture(t, fake.Clip())
	x := capture.NewMux(capture.Options{Audio: true})
	push(t, x, capture.Packet{Kind: capture.VideoConfig, Video: format})
	var got []capture.Chunk
	for i := 0; i <= 15 && len(got) == 0; i++ {
		got = push(t, x, capture.Packet{Kind: capture.Video, PTS: time.Duration(i) * time.Second / 15, Key: frames[i].Key, Data: frames[i].Data})
	}
	if len(got) != 17 || x.HasAudio() {
		t.Fatalf("started with %d chunks, audio %v; want init + 16 frames, no audio", len(got), x.HasAudio())
	}

	y := capture.NewMux(capture.Options{Audio: true})
	push(t, y, capture.Packet{Kind: capture.VideoConfig, Video: format})
	got = nil
	for i := 0; i < 256 && len(got) == 0; i++ {
		got = push(t, y, capture.Packet{Kind: capture.Audio, PTS: 0, Data: []byte{1}})
	}
	if len(got) != 1 || !got[0].Init {
		t.Fatalf("pending cap gave %d chunks", len(got))
	}
}

func TestMux_Errors(t *testing.T) {
	format, frames := fixture(t, fake.Clip())
	// Media with no video config at all: after the cap, an error.
	x := capture.NewMux(capture.Options{})
	var err error
	for i := 0; i < 256; i++ {
		_, err = x.Push(capture.Packet{Kind: capture.Video, Key: true, Data: frames[0].Data})
	}
	if err == nil || x.Started() {
		t.Fatal("no error without a video config")
	}
	// A config with no SPS, then one with a bad size: errors; a good one
	// recovers.
	if _, err := x.Push(capture.Packet{Kind: capture.VideoConfig, Video: &capture.VideoFormat{}}); err == nil {
		t.Error("empty video config accepted")
	}
	if _, err := x.Push(capture.Packet{Kind: capture.VideoConfig, Video: &capture.VideoFormat{SPS: format.SPS, PPS: format.PPS}}); err == nil {
		t.Error("0x0 video config accepted")
	}
	if got := push(t, x, capture.Packet{Kind: capture.VideoConfig, Video: format}); len(got) != 1 {
		t.Fatal("good config did not start the stream")
	}
	// A held-back sample that cannot be muxed is reported, the rest written.
	y := capture.NewMux(capture.Options{Audio: true})
	push(t, y, capture.Packet{Kind: capture.VideoConfig, Video: format})
	push(t, y, capture.Packet{Kind: capture.Video, Key: true, Data: []byte{0, 0, 0, 9}})
	push(t, y, capture.Packet{Kind: capture.Video, Key: true, Data: frames[0].Data})
	got, err := y.Push(capture.Packet{Kind: capture.AudioConfig, Audio: aacFormat})
	if err == nil || len(got) != 2 {
		t.Errorf("start with a bad held-back sample: %d chunks, %v", len(got), err)
	}
	// A nil config is ignored.
	z := capture.NewMux(capture.Options{})
	if got := push(t, z, capture.Packet{Kind: capture.VideoConfig}); len(got) != 0 {
		t.Error("nil config started the stream")
	}
}

// Flush starts a Mux still waiting for its audio config, video-only, with
// what it holds; it gives nothing once started or without a video config.
func TestMux_Flush(t *testing.T) {
	format, frames := fixture(t, fake.Clip())
	if got, err := capture.NewMux(capture.Options{Audio: true}).Flush(); got != nil || err != nil {
		t.Fatalf("Flush of an empty Mux = %v, %v", got, err)
	}
	x := capture.NewMux(capture.Options{FPS: 15, Audio: true})
	push(t, x, capture.Packet{Kind: capture.VideoConfig, Video: format})
	if got, _ := x.Flush(); got != nil {
		t.Fatalf("Flush with a config and no media = %+v", got)
	}
	for i := 0; i < 3; i++ {
		push(t, x, capture.Packet{Kind: capture.Video, PTS: time.Duration(i) * time.Second / 15, Key: frames[i].Key, Data: frames[i].Data})
	}
	got, err := x.Flush()
	if err != nil || len(got) != 4 || !got[0].Init || !x.Started() || x.HasAudio() {
		t.Fatalf("Flush = %d chunks, %v; started %v, audio %v", len(got), err, x.Started(), x.HasAudio())
	}
	if again, _ := x.Flush(); again != nil {
		t.Errorf("a second Flush = %+v", again)
	}
}

// Review 1.harness-review.7: an audio format the muxer refuses (a codec
// the codec string cannot name, or LPCM the muxer cannot write) drops the
// audio: the stream starts video-only with the held-back video, says why,
// and keeps going.
func TestMux_RefusedAudioStartsVideoOnly(t *testing.T) {
	format, frames := fixture(t, fake.Clip())
	for name, bad := range map[string]*capture.AudioFormat{
		"codec":  {Codec: "opus", SampleRate: 48000, Channels: 2},
		"muxer":  {Codec: capture.CodecLPCM, SampleRate: 48000, Channels: 2, BitsPerSample: 12},
		"no ASC": {Codec: capture.CodecAAC, SampleRate: 48000, Channels: 2},
	} {
		t.Run(name, func(t *testing.T) {
			x := capture.NewMux(capture.Options{FPS: 15, Audio: true})
			push(t, x, capture.Packet{Kind: capture.VideoConfig, Video: format})
			push(t, x, capture.Packet{Kind: capture.Video, Key: true, Data: frames[0].Data})
			push(t, x, capture.Packet{Kind: capture.Audio, Data: []byte{1, 2}})
			got, err := x.Push(capture.Packet{Kind: capture.AudioConfig, Audio: bad})
			if err == nil || !strings.Contains(err.Error(), "dropped the audio") {
				t.Errorf("err = %v, want the audio dropped", err)
			}
			if len(got) != 2 || !got[0].Init || got[1].Track != 1 || !x.Started() || x.HasAudio() || strings.Contains(x.Codecs(), ",") {
				t.Fatalf("got %d chunks, started %v, audio %v, codecs %q; want init + 1 frame, video-only", len(got), x.Started(), x.HasAudio(), x.Codecs())
			}
			if x.VideoFormat() != format {
				t.Error("the video config was not kept")
			}
			if got := push(t, x, capture.Packet{Kind: capture.Video, PTS: time.Second / 15, Key: frames[1].Key, Data: frames[1].Data}); len(got) != 1 || got[0].Track != 1 {
				t.Errorf("video after the start = %+v", got)
			}
			if got := push(t, x, capture.Packet{Kind: capture.Audio, PTS: time.Second / 15, Data: []byte{3}}); len(got) != 0 {
				t.Errorf("audio after the start = %+v", got)
			}
		})
	}
}
