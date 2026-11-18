package fmp4_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/capture/fake"
	"github.com/devicelab-dev/maestro-runner/pkg/capture/fmp4"
	"github.com/devicelab-dev/maestro-runner/pkg/capture/internal/probetest"
)

// clip parses a fixture clip into the fmp4 video format and its frames.
func clip(t *testing.T, annexB []byte) (fmp4.Video, []fake.Frame) {
	t.Helper()
	f, frames, err := fake.ParseClip(annexB)
	if err != nil {
		t.Fatal(err)
	}
	return fmp4.Video{Width: f.Width, Height: f.Height, SPS: f.SPS, PPS: f.PPS}, frames
}

// writeFile writes the init segment and fragments of one stream.
func writeFile(t *testing.T, name string, parts ...[]byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	var all []byte
	for _, b := range parts {
		all = append(all, b...)
	}
	if err := os.WriteFile(p, all, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// muxClip writes frames at 15 fps from pts start and, with audio, interleaves
// audio samples from next on its own clock.
func muxClip(t *testing.T, m *fmp4.Muxer, frames []fake.Frame, start time.Duration, audio func(until time.Duration) [][]byte) [][]byte {
	t.Helper()
	var out [][]byte
	for i, f := range frames {
		pts := start + time.Duration(i)*time.Second/15
		if audio != nil {
			out = append(out, audio(pts)...)
		}
		frag, err := m.Video(pts, f.Key, f.Data)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, frag.Bytes)
	}
	return out
}

// aacFeeder returns a func writing AAC frames whose PTS is before until.
func aacFeeder(t *testing.T, m *fmp4.Muxer) func(time.Duration) [][]byte {
	frames := fake.AACFrames()
	j := 0
	return func(until time.Duration) [][]byte {
		var out [][]byte
		for {
			pts := time.Duration(j) * 1024 * time.Second / 48000
			if pts > until {
				return out
			}
			frag, err := m.Audio(pts, frames[j%len(frames)])
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, frag.Bytes)
			j++
		}
	}
}

// pcmFeeder returns a func writing 20 ms s16 PCM chunks whose PTS is
// before until.
func pcmFeeder(t *testing.T, m *fmp4.Muxer) func(time.Duration) [][]byte {
	return pcmChunks(t, m, fake.PCM(), 960*4) // 20 ms of 48 kHz stereo s16
}

// floatPCMFeeder is pcmFeeder for the 32-bit float fixture.
func floatPCMFeeder(t *testing.T, m *fmp4.Muxer) func(time.Duration) [][]byte {
	return pcmChunks(t, m, fake.FloatPCM(), 960*8) // 20 ms of 48 kHz stereo f32
}

// pcmChunks returns a func writing 20 ms chunks of pcm whose PTS is before
// until.
func pcmChunks(t *testing.T, m *fmp4.Muxer, pcm []byte, chunk int) func(time.Duration) [][]byte {
	j := 0
	return func(until time.Duration) [][]byte {
		var out [][]byte
		for {
			pts := time.Duration(j) * 20 * time.Millisecond
			if pts > until {
				return out
			}
			off := j % (len(pcm) / chunk) * chunk
			frag, err := m.Audio(pts, pcm[off:off+chunk])
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, frag.Bytes)
			j++
		}
	}
}

// U5: the muxed fixtures read back as h264/avc3 640x360 with 45 frames, AAC
// 48 kHz stereo, pcm_s16le in ipcm and pcm_f32le in fpcm.
func TestFFprobe_MuxedFixtures(t *testing.T) {
	probetest.Binary(t)
	video, frames := clip(t, fake.Clip())
	if len(frames) != 45 {
		t.Fatalf("clip has %d frames, want 45", len(frames))
	}
	cases := []struct {
		name  string
		audio *fmp4.Audio
		feed  func(*testing.T, *fmp4.Muxer) func(time.Duration) [][]byte
		codec string
		tag   string
	}{
		{name: "video"},
		{name: "aac", audio: &fmp4.Audio{Codec: fmp4.CodecAAC, SampleRate: 48000, Channels: 2, ASC: fake.ASC()}, feed: aacFeeder, codec: "aac", tag: "mp4a"},
		{name: "lpcm", audio: &fmp4.Audio{Codec: fmp4.CodecLPCM, SampleRate: 48000, Channels: 2, BitsPerSample: 16, LittleEndian: true}, feed: pcmFeeder, codec: "pcm_s16le", tag: "ipcm"},
		{name: "fpcm", audio: &fmp4.Audio{Codec: fmp4.CodecLPCM, SampleRate: 48000, Channels: 2, BitsPerSample: 32, LittleEndian: true, Float: true}, feed: floatPCMFeeder, codec: "pcm_f32le", tag: "fpcm"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := fmp4.NewMuxer(video, tc.audio, 15)
			if err != nil {
				t.Fatal(err)
			}
			var feed func(time.Duration) [][]byte
			if tc.feed != nil {
				feed = tc.feed(t, m)
			}
			parts := append([][]byte{m.Init()}, muxClip(t, m, frames, 5*time.Second, feed)...)
			r := probetest.Probe(t, writeFile(t, tc.name+".mp4", parts...))
			v, ok := r.Find("h264")
			if !ok {
				t.Fatalf("no h264 stream: %+v", r.Streams)
			}
			if v.CodecTag != "avc3" || v.Width != 640 || v.Height != 360 || v.FrameCount() != 45 {
				t.Errorf("video stream = %+v, want avc3 640x360 with 45 frames", v)
			}
			if tc.audio == nil {
				if len(r.Streams) != 1 {
					t.Errorf("streams = %+v, want video only", r.Streams)
				}
				return
			}
			a, ok := r.Find(tc.codec)
			if !ok {
				t.Fatalf("no %s stream: %+v", tc.codec, r.Streams)
			}
			if a.CodecTag != tc.tag || a.SampleRate != "48000" || a.Channels != 2 || a.FrameCount() == 0 {
				t.Errorf("audio stream = %+v, want %s 48000 Hz 2 ch", a, tc.tag)
			}
		})
	}
}

// U6: the 640x360 clip then the 360x640 clip in one stream decode as 60
// frames: the new SPS travels in band (avc3).
func TestFFprobe_FormatChangeInBand(t *testing.T) {
	probetest.Binary(t)
	video, frames := clip(t, fake.Clip())
	rotated, rframes := clip(t, fake.RotatedClip())
	if rotated.Width != 360 || rotated.Height != 640 || len(rframes) != 15 {
		t.Fatalf("rotated clip = %dx%d with %d frames", rotated.Width, rotated.Height, len(rframes))
	}
	m, err := fmp4.NewMuxer(video, nil, 15)
	if err != nil {
		t.Fatal(err)
	}
	parts := append([][]byte{m.Init()}, muxClip(t, m, frames, 0, nil)...)
	m.SetParameterSets(rotated.SPS, rotated.PPS)
	parts = append(parts, muxClip(t, m, rframes, 3*time.Second, nil)...)
	r := probetest.Probe(t, writeFile(t, "rotate.mp4", parts...))
	v, ok := r.Find("h264")
	if !ok || v.FrameCount() != 60 {
		t.Fatalf("streams = %+v, want one h264 stream with 60 frames", r.Streams)
	}
}
