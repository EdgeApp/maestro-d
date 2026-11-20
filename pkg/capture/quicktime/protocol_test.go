package quicktime

import (
	"bytes"
	"encoding/binary"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/capture"
	"github.com/devicelab-dev/maestro-runner/pkg/capture/fake"
)

// avcCOf builds an avcC atom (4-byte NAL lengths) for a format.
func avcCOf(f *capture.VideoFormat) []byte {
	sps := f.SPS[0]
	b := []byte{1, sps[1], sps[2], sps[3], 0xff, 0xe0 | byte(len(f.SPS))}
	for _, s := range f.SPS {
		b = binary.BigEndian.AppendUint16(b, uint16(len(s)))
		b = append(b, s...)
	}
	b = append(b, byte(len(f.PPS)))
	for _, p := range f.PPS {
		b = binary.BigEndian.AppendUint16(b, uint16(len(p)))
		b = append(b, p...)
	}
	return b
}

// videoConfigPayload is a type-2 payload.
func videoConfigPayload(f *capture.VideoFormat) []byte {
	b := binary.LittleEndian.AppendUint32(nil, uint32(f.Width))
	b = binary.LittleEndian.AppendUint32(b, uint32(f.Height))
	return append(b, avcCOf(f)...)
}

// audioConfigPayload is a type-4 payload.
func audioConfigPayload(rate, channels, bits int, flags uint32) []byte {
	b := binary.LittleEndian.AppendUint32(nil, uint32(rate))
	b = binary.LittleEndian.AppendUint16(b, uint16(channels))
	b = binary.LittleEndian.AppendUint16(b, uint16(bits))
	return binary.LittleEndian.AppendUint32(b, flags)
}

// lpcmFlags are the flags iOS sends for integer samples: signed integer,
// packed. floatFlags are those it sends for float samples: float, packed.
const (
	lpcmFlags  = 1<<2 | 1<<3
	floatFlags = asbdIsFloat | 1<<3
)

func clipFormat(t *testing.T) (*capture.VideoFormat, []fake.Frame) {
	t.Helper()
	f, frames, err := fake.ParseClip(fake.Clip())
	if err != nil {
		t.Fatal(err)
	}
	return f, frames
}

// U38: frames of every type decode into the packets the session expects.
func TestProtocol_FrameTypes(t *testing.T) {
	format, frames := clipFormat(t)
	var buf bytes.Buffer
	write := func(f frame) {
		t.Helper()
		if err := writeFrame(&buf, f); err != nil {
			t.Fatal(err)
		}
	}
	write(statusFrame(status{State: stateCapturing, Match: "serial", UDID: "u"}))
	write(frame{typ: frameVideoConfig, pts: time.Second, payload: videoConfigPayload(format)})
	write(frame{typ: frameVideoSample, flags: flagSync, pts: time.Second, payload: frames[0].Data})
	write(frame{typ: frameVideoSample, pts: time.Second + 16*time.Millisecond, payload: frames[1].Data})
	write(frame{typ: frameAudioConfig, pts: time.Second, payload: audioConfigPayload(48000, 2, 16, lpcmFlags)})
	write(frame{typ: frameAudioSample, pts: -time.Millisecond, payload: []byte{1, 2, 3, 4}})
	write(frame{typ: 99, payload: []byte{1}})

	f, err := readFrame(&buf)
	if err != nil || f.typ != frameStatus {
		t.Fatalf("status frame: %+v %v", f, err)
	}
	st, err := parseStatus(f.payload)
	if err != nil || st.State != stateCapturing || st.Match != "serial" || st.UDID != "u" {
		t.Fatalf("status: %+v %v", st, err)
	}
	if _, ok, err := packetOf(f); ok || err != nil {
		t.Fatal("a status is not a packet")
	}

	f, _ = readFrame(&buf)
	p, ok, err := packetOf(f)
	if err != nil || !ok || p.Kind != capture.VideoConfig || p.PTS != time.Second {
		t.Fatalf("video config: %+v %v", p, err)
	}
	if p.Video.Width != 640 || p.Video.Height != 360 ||
		!bytes.Equal(p.Video.SPS[0], format.SPS[0]) || !bytes.Equal(p.Video.PPS[0], format.PPS[0]) {
		t.Fatalf("avcC → SPS/PPS: %+v", p.Video)
	}

	f, _ = readFrame(&buf)
	p, _, _ = packetOf(f)
	if p.Kind != capture.Video || !p.Key || !bytes.Equal(p.Data, frames[0].Data) {
		t.Fatalf("sync sample: %+v", p)
	}
	f, _ = readFrame(&buf)
	p, _, _ = packetOf(f)
	if p.Kind != capture.Video || p.Key || p.PTS != time.Second+16*time.Millisecond {
		t.Fatalf("non-sync sample: %+v", p)
	}

	f, _ = readFrame(&buf)
	p, _, err = packetOf(f)
	want := capture.AudioFormat{Codec: capture.CodecLPCM, SampleRate: 48000, Channels: 2, BitsPerSample: 16, LittleEndian: true}
	if err != nil || p.Kind != capture.AudioConfig || !reflect.DeepEqual(*p.Audio, want) {
		t.Fatalf("audio config: %+v %v", p.Audio, err)
	}
	f, _ = readFrame(&buf)
	p, _, _ = packetOf(f)
	if p.Kind != capture.Audio || p.PTS != -time.Millisecond || !bytes.Equal(p.Data, []byte{1, 2, 3, 4}) {
		t.Fatalf("audio sample: %+v", p)
	}
	f, _ = readFrame(&buf)
	if _, ok, err := packetOf(f); ok || err != nil {
		t.Fatal("an unknown frame type is skipped")
	}
	if _, err := readFrame(&buf); err != io.EOF {
		t.Fatalf("end: %v", err)
	}
}

func TestProtocol_ReadErrors(t *testing.T) {
	var h [headerLen]byte
	binary.LittleEndian.PutUint32(h[12:], maxFrameLen+1)
	if _, err := readFrame(bytes.NewReader(h[:])); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversized frame: %v", err)
	}
	binary.LittleEndian.PutUint32(h[12:], 10)
	if _, err := readFrame(bytes.NewReader(append(h[:], 1, 2))); err != io.ErrUnexpectedEOF {
		t.Fatalf("truncated payload: %v", err)
	}
	if _, err := readFrame(bytes.NewReader(h[:5])); err != io.ErrUnexpectedEOF {
		t.Fatalf("truncated header: %v", err)
	}
	if _, err := parseStatus([]byte("{")); err == nil {
		t.Fatal("bad status JSON")
	}
}

func TestProtocol_VideoConfigErrors(t *testing.T) {
	format, _ := clipFormat(t)
	good := videoConfigPayload(format)
	cases := map[string][]byte{
		"short":       good[:10],
		"version":     append(append([]byte{}, good[:8]...), append([]byte{2}, good[9:]...)...),
		"length size": append(append([]byte{}, good[:12]...), append([]byte{0xfd}, good[13:]...)...),
		"truncated":   good[:len(good)-2],
		"no pps count": func() []byte {
			b := append([]byte{}, good[:8]...)
			avcc := avcCOf(format)
			n := 6 + 2 + len(format.SPS[0])
			return append(b, avcc[:n]...)
		}(),
		"no sps": func() []byte {
			b := append([]byte{}, good[:8]...)
			return append(b, 1, 0x64, 0, 0x1e, 0xff, 0xe0, 0)
		}(),
		"sps truncated": func() []byte {
			b := append([]byte{}, good[:8]...)
			return append(b, 1, 0x64, 0, 0x1e, 0xff, 0xe1, 0)
		}(),
	}
	for name, payload := range cases {
		if _, err := parseVideoConfig(payload); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if _, ok, err := packetOf(frame{typ: frameVideoConfig, payload: payload}); ok || err == nil {
			t.Errorf("%s: packetOf accepted", name)
		}
	}
}

func TestProtocol_AudioConfig(t *testing.T) {
	big, planar, err := parseAudioConfig(audioConfigPayload(44100, 1, 24, lpcmFlags|asbdIsBigEndian))
	if err != nil || planar || big.LittleEndian || big.BitsPerSample != 24 || big.SampleRate != 44100 || big.Channels != 1 {
		t.Fatalf("big-endian: %+v %v %v", big, planar, err)
	}
	// Q9: float samples are carried as delivered, 32 or 64 bits.
	for _, bits := range []int{32, 64} {
		f, planar, err := parseAudioConfig(audioConfigPayload(48000, 2, bits, floatFlags))
		want := capture.AudioFormat{Codec: capture.CodecLPCM, SampleRate: 48000, Channels: 2, BitsPerSample: bits, LittleEndian: true, Float: true}
		if err != nil || planar || !reflect.DeepEqual(*f, want) {
			t.Fatalf("float %d: %+v %v %v", bits, f, planar, err)
		}
		p, ok, err := packetOf(frame{typ: frameAudioConfig, payload: audioConfigPayload(48000, 2, bits, floatFlags)})
		if !ok || err != nil || p.Kind != capture.AudioConfig || !p.Audio.Float {
			t.Fatalf("float %d packet: %+v %v %v", bits, p, ok, err)
		}
		if s, err := capture.AudioCodec(p.Audio); err != nil || s != "fpcm" {
			t.Errorf("float %d codec %q %v, want fpcm", bits, s, err)
		}
	}
	if f, _, err := parseAudioConfig(audioConfigPayload(48000, 2, 32, lpcmFlags)); err != nil || f.Float {
		t.Fatalf("32-bit integer: %+v %v", f, err)
	}
	// Non-interleaved samples are planar for two or more channels; one
	// channel's single plane is already interleaved.
	if _, planar, err := parseAudioConfig(audioConfigPayload(48000, 2, 32, floatFlags|asbdIsNonInterleaved)); err != nil || !planar {
		t.Fatalf("non-interleaved stereo: planar %v %v", planar, err)
	}
	if _, planar, err := parseAudioConfig(audioConfigPayload(48000, 1, 32, floatFlags|asbdIsNonInterleaved)); err != nil || planar {
		t.Fatalf("non-interleaved mono: planar %v %v", planar, err)
	}
	for name, payload := range map[string][]byte{
		"short":          {1, 2, 3},
		"float 16-bit":   audioConfigPayload(48000, 2, 16, floatFlags),
		"float 24-bit":   audioConfigPayload(48000, 2, 24, floatFlags),
		"no channels":    audioConfigPayload(48000, 0, 16, lpcmFlags),
		"8-bit":          audioConfigPayload(48000, 2, 8, lpcmFlags),
		"64-bit integer": audioConfigPayload(48000, 2, 64, lpcmFlags),
	} {
		if _, _, err := parseAudioConfig(payload); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if _, ok, err := packetOf(frame{typ: frameAudioConfig, payload: payload}); ok || err == nil {
			t.Errorf("%s: packetOf accepted", name)
		}
	}
	if got := describeAudio(&capture.AudioFormat{SampleRate: 48000, Channels: 2, BitsPerSample: 16}, false); got != "48000 Hz, 2 ch, 16-bit integer, interleaved" {
		t.Errorf("describeAudio = %q", got)
	}
	if got := describeAudio(&capture.AudioFormat{SampleRate: 48000, Channels: 2, BitsPerSample: 32, Float: true}, true); got != "48000 Hz, 2 ch, 32-bit float, non-interleaved, interleaved here" {
		t.Errorf("describeAudio = %q", got)
	}
}

// Q9: a non-interleaved config makes the decoder interleave each sample's
// planes, bit for bit; an interleaved config leaves samples as they are; a
// sample that is not whole frames is an error.
func TestProtocol_PlanarAudioIsInterleaved(t *testing.T) {
	var d decoder
	if _, _, err := d.packet(frame{typ: frameAudioConfig, payload: audioConfigPayload(48000, 2, 32, floatFlags|asbdIsNonInterleaved)}); err != nil {
		t.Fatal(err)
	}
	// Three frames: left L0 L1 L2, then right R0 R1 R2, 4 bytes each.
	planes := []byte{
		0x10, 0x11, 0x12, 0x13, 0x20, 0x21, 0x22, 0x23, 0x30, 0x31, 0x32, 0x33,
		0x40, 0x41, 0x42, 0x43, 0x50, 0x51, 0x52, 0x53, 0x60, 0x61, 0x62, 0x63,
	}
	p, ok, err := d.packet(frame{typ: frameAudioSample, pts: time.Second, payload: planes})
	want := []byte{
		0x10, 0x11, 0x12, 0x13, 0x40, 0x41, 0x42, 0x43,
		0x20, 0x21, 0x22, 0x23, 0x50, 0x51, 0x52, 0x53,
		0x30, 0x31, 0x32, 0x33, 0x60, 0x61, 0x62, 0x63,
	}
	if !ok || err != nil || p.Kind != capture.Audio || p.PTS != time.Second || !bytes.Equal(p.Data, want) {
		t.Fatalf("planar sample: %x %v %v, want %x", p.Data, ok, err, want)
	}
	if _, _, err := d.packet(frame{typ: frameAudioSample, payload: planes[:20]}); err == nil {
		t.Error("a sample of partial frames was accepted")
	}
	// Back to an interleaved config: samples pass through.
	if _, _, err := d.packet(frame{typ: frameAudioConfig, payload: audioConfigPayload(48000, 2, 16, lpcmFlags)}); err != nil {
		t.Fatal(err)
	}
	if p, _, _ := d.packet(frame{typ: frameAudioSample, payload: planes[:20]}); !bytes.Equal(p.Data, planes[:20]) {
		t.Errorf("interleaved sample changed: %x", p.Data)
	}
}
