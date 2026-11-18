package scrcpy

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/capture"
	"github.com/devicelab-dev/maestro-runner/pkg/capture/fake"
	"github.com/devicelab-dev/maestro-runner/pkg/capture/h264"
)

// U28: the video parser on the Pixel 8's recorded socket: codec id, the
// session packet's size, config vs key vs delta packets, PTS, sizes.
func TestReadVideo_RecordedFixture(t *testing.T) {
	data := readFixture(t, "video.bin")
	var c collector
	r := bytes.NewReader(data)
	if err := readVideo(r, c.emit); !errors.Is(err, io.EOF) {
		t.Fatalf("readVideo ended with %v, want io.EOF at the end of the dump", err)
	}
	if r.Len() != 0 {
		t.Fatalf("%d bytes left unread", r.Len())
	}
	ps := c.packets()
	if len(ps) < 3 || ps[0].Kind != capture.VideoConfig || ps[1].Kind != capture.Video || !ps[1].Key {
		t.Fatalf("stream starts %v", kindsOf(ps, 3))
	}
	f := ps[0].Video
	info, err := h264.ParseSPS(f.SPS[0])
	if err != nil {
		t.Fatal(err)
	}
	// The session packet's size is the encoded size the SPS describes.
	if f.Width != info.Width || f.Height != info.Height || f.Width < 1000 || f.Height < 1000 {
		t.Errorf("config %dx%d, SPS %dx%d", f.Width, f.Height, info.Width, info.Height)
	}
	var keys, deltas int
	var last time.Duration
	for _, p := range ps[1:] {
		if p.Kind != capture.Video {
			continue
		}
		if p.PTS < last {
			t.Fatalf("PTS went back: %v after %v", p.PTS, last)
		}
		last = p.PTS
		if _, err := h264.SplitAVCC(p.Data); err != nil || len(p.Data) == 0 {
			t.Fatalf("sample is not AVCC: %v", err)
		}
		if p.Key {
			keys++
		} else {
			deltas++
		}
	}
	if keys < 1 || deltas < 1 || last-ps[1].PTS > 10*time.Second || last <= 0 {
		t.Errorf("%d keys, %d deltas over %v", keys, deltas, last-ps[1].PTS)
	}
}

func kindsOf(ps []capture.Packet, n int) []capture.Kind {
	var out []capture.Kind
	for i := 0; i < n && i < len(ps); i++ {
		out = append(out, ps[i].Kind)
	}
	return out
}

// U28: sizes, flags and the PTS mask on synthetic bytes, including a
// second session packet (a rotation) and every malformed input.
func TestReadVideo_Synthetic(t *testing.T) {
	var c collector
	data := syntheticVideo(t, 20)
	w := &streamWriter{}
	w.session(360, 640)
	format, _, err := fakeFormat()
	if err != nil {
		t.Fatal(err)
	}
	w.packet(0, true, false, h264.JoinAnnexB(append(append([][]byte{}, format.SPS...), format.PPS...)))
	w.packet(5*time.Second, false, true, []byte{0, 0, 0, 1, 0x65, 1, 2})
	data = append(data, w.Bytes()...)
	if err := readVideo(bytes.NewReader(data), c.emit); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	ps := c.packets()
	if c.count(capture.VideoConfig) != 2 || c.count(capture.Video) != 21 {
		t.Fatalf("%d configs, %d frames", c.count(capture.VideoConfig), c.count(capture.Video))
	}
	if v := ps[0].Video; v.Width != 640 || v.Height != 360 {
		t.Errorf("first config %dx%d", v.Width, v.Height)
	}
	last := ps[len(ps)-1]
	if cfg := ps[len(ps)-2]; cfg.Video.Width != 360 || cfg.Video.Height != 640 {
		t.Errorf("config after the session packet %dx%d", cfg.Video.Width, cfg.Video.Height)
	}
	if !last.Key || last.PTS != 5*time.Second || !bytes.Equal(last.Data, []byte{0, 0, 0, 3, 0x65, 1, 2}) {
		t.Errorf("last frame %+v", last)
	}
	if ps[2].Key || ps[2].PTS != time.Second+(time.Second/15).Truncate(time.Microsecond) {
		t.Errorf("second frame key %v pts %v", ps[2].Key, ps[2].PTS)
	}

	// No session packet: the size comes from the SPS.
	c = collector{}
	w = &streamWriter{}
	w.u32(codecH264).packet(0, true, false, h264.JoinAnnexB(append(append([][]byte{}, format.SPS...), format.PPS...)))
	if err := readVideo(bytes.NewReader(w.Bytes()), c.emit); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if v := c.packets()[0].Video; v.Width != 640 || v.Height != 360 {
		t.Errorf("SPS size %dx%d", v.Width, v.Height)
	}

	full := syntheticVideo(t, 3)
	bad := func(name string, b []byte, want string) {
		t.Helper()
		err := readVideo(bytes.NewReader(b), func(capture.Packet) {})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", name, err, want)
		}
	}
	bad("truncated payload", full[:len(full)-3], "unexpected EOF")
	bad("truncated header", full[:10], "unexpected EOF")
	bad("empty", nil, "EOF")
	bad("wrong codec", (&streamWriter{}).u32(0x68323635).Bytes(), "video codec id")
	bad("oversized", (&streamWriter{}).u32(codecH264).u32(0).u32(1).u32(maxPacket+1).Bytes(), "exceeds")
	bad("config without SPS", (&streamWriter{}).u32(codecH264).packet(0, true, false, []byte{0, 0, 0, 1, 0x65}).Bytes(), "no SPS/PPS")
	bad("bad SPS", (&streamWriter{}).u32(codecH264).packet(0, true, false, []byte{0, 0, 0, 1, 0x67, 0, 0, 0, 1, 0x68, 1}).Bytes(), "")
}

func fakeFormat() (*capture.VideoFormat, []fake.Frame, error) {
	return fake.ParseClip(fake.Clip())
}

// U29: the audio parser on the recorded socket: codec id aac, the
// AudioSpecificConfig's rate and channels, then AAC frames.
func TestReadAudio_RecordedFixture(t *testing.T) {
	data := readFixture(t, "audio.bin")
	var c collector
	r := bytes.NewReader(data)
	if err := readAudio(r, c.emit); !errors.Is(err, io.EOF) {
		t.Fatalf("readAudio ended with %v", err)
	}
	ps := c.packets()
	if len(ps) < 10 || ps[0].Kind != capture.AudioConfig {
		t.Fatalf("stream starts %v", kindsOf(ps, 3))
	}
	a := ps[0].Audio
	if a.Codec != capture.CodecAAC || a.SampleRate != 48000 || a.Channels != 2 {
		t.Errorf("audio format %+v", a)
	}
	if c.count(capture.Audio) != len(ps)-1 {
		t.Errorf("%d audio frames of %d packets", c.count(capture.Audio), len(ps))
	}
}

// U29: codec ids 0 and 1 mean no audio; other ids, session packets and
// malformed configs are errors.
func TestReadAudio_Synthetic(t *testing.T) {
	var c collector
	if err := readAudio(bytes.NewReader(syntheticAudio(10)), c.emit); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if c.count(capture.AudioConfig) != 1 || c.count(capture.Audio) != 10 {
		t.Fatalf("%d configs, %d frames", c.count(capture.AudioConfig), c.count(capture.Audio))
	}
	var none errNoAudio
	for _, id := range []uint32{codecDisabled, codecError} {
		err := readAudio(bytes.NewReader((&streamWriter{}).u32(id).Bytes()), c.emit)
		if !errors.As(err, &none) || none.id != id || err.Error() == "" {
			t.Errorf("id %d: %v", id, err)
		}
	}
	if !strings.Contains(errNoAudio{codecDisabled}.Error(), "disabled") || !strings.Contains(errNoAudio{codecError}.Error(), "failed") {
		t.Error("errNoAudio messages")
	}
	bad := func(name string, b []byte, want string) {
		t.Helper()
		err := readAudio(bytes.NewReader(b), func(capture.Packet) {})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", name, err, want)
		}
	}
	bad("empty", nil, "EOF")
	bad("opus", (&streamWriter{}).u32(0x6f707573).Bytes(), "audio codec id")
	bad("session", (&streamWriter{}).u32(codecAAC).session(1, 1).Bytes(), "session packet")
	bad("short ASC", (&streamWriter{}).u32(codecAAC).packet(0, true, false, []byte{0x11}).Bytes(), "AudioSpecificConfig")
	bad("truncated", syntheticAudio(2)[:30], "unexpected EOF")
	bad("bad header", (&streamWriter{}).u32(codecAAC).u32(0).Bytes(), "unexpected EOF")
}

func TestAudioFormat(t *testing.T) {
	cases := []struct {
		asc      []byte
		rate, ch int
		err      string
	}{
		{asc: []byte{0x11, 0x90}, rate: 48000, ch: 2},                   // AAC-LC 48 kHz stereo
		{asc: []byte{0x12, 0x08}, rate: 44100, ch: 1},                   // AAC-LC 44.1 kHz mono
		{asc: []byte{0x17, 0x80, 0x5d, 0xc0, 0x10}, rate: 48000, ch: 2}, // explicit frequency
		{asc: []byte{0xf8, 0x06, 0x40}, rate: 48000, ch: 2},             // escaped object type 32
		{asc: []byte{0x11, 0xb8}, rate: 48000, ch: 8},                   // 7.1
		{asc: []byte{0x17, 0x80}, err: "truncated"},                     // explicit frequency, too short
		{asc: []byte{0x16, 0x90}, err: "sampling index 13"},             // reserved index
		{asc: []byte{0x11, 0x80}, err: "channel configuration 0"},       // channels in the PCE
		{asc: []byte{0x11}, err: "of 1 bytes"},
	}
	for _, c := range cases {
		f, err := audioFormat(c.asc)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%x: %v, want %q", c.asc, err, c.err)
			}
			continue
		}
		if err != nil || f.SampleRate != c.rate || f.Channels != c.ch || f.Codec != capture.CodecAAC || !bytes.Equal(f.ASC, c.asc) {
			t.Errorf("%x: %+v %v, want %d Hz %d ch", c.asc, f, err, c.rate, c.ch)
		}
	}
}
