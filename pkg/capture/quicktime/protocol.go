package quicktime

// protocol.go: the helper socket protocol, version 1. It is pure Go and
// builds everywhere, so its decoding is tested on every platform.

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/capture"
)

// ProtocolVersion is the helper protocol this package speaks.
const ProtocolVersion = 1

// Frame types.
const (
	frameStatus      = 1 // JSON status
	frameVideoConfig = 2 // u32 width, u32 height, then the avcC atom
	frameVideoSample = 3 // one AVCC sample; flags bit 0 = sync
	frameAudioConfig = 4 // u32 sampleRate, u16 channels, u16 bits, u32 asbdFlags
	frameAudioSample = 5 // interleaved PCM
)

const (
	headerLen = 16
	// maxFrameLen bounds one payload; a 4K keyframe is a few MiB.
	maxFrameLen = 64 << 20
	flagSync    = 1
)

// Status states.
const (
	stateHello             = "hello"
	stateWaitingPermission = "waiting-permission"
	stateReady             = "ready"
	stateUnavailable       = "unavailable"
	stateDeviceFound       = "device-found"
	stateCapturing         = "capturing"
)

// Core Audio's AudioStreamBasicDescription flags the Go side reads.
const (
	asbdIsFloat          = 1 << 0
	asbdIsBigEndian      = 1 << 1
	asbdIsNonInterleaved = 1 << 5
)

// frame is one protocol frame.
type frame struct {
	typ     uint8
	flags   uint8
	pts     time.Duration
	payload []byte
}

// status is a type-1 frame's JSON payload.
type status struct {
	State    string `json:"state"`
	Reason   string `json:"reason,omitempty"`
	Message  string `json:"message,omitempty"`
	Match    string `json:"match,omitempty"`
	UDID     string `json:"udid,omitempty"`
	PID      int    `json:"pid,omitempty"`
	Version  string `json:"version,omitempty"`
	Protocol int    `json:"protocol,omitempty"`
}

// controlOp is one JSON line the Host writes on the control connection.
type controlOp struct {
	Op   string `json:"op"` // "open" or "close"
	UDID string `json:"udid"`
	Name string `json:"name,omitempty"`
}

// readFrame reads one frame. Its payload is freshly allocated.
func readFrame(r io.Reader) (frame, error) {
	var h [headerLen]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return frame{}, err
	}
	n := binary.LittleEndian.Uint32(h[12:16])
	if n > maxFrameLen {
		return frame{}, fmt.Errorf("capture helper frame of %d bytes is too large", n)
	}
	f := frame{
		typ:     h[0],
		flags:   h[1],
		pts:     time.Duration(int64(binary.LittleEndian.Uint64(h[4:12]))) * time.Microsecond,
		payload: make([]byte, n),
	}
	if _, err := io.ReadFull(r, f.payload); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return frame{}, err
	}
	return f, nil
}

// writeFrame writes one frame (the helper's side; the tests' fake helper
// uses it).
func writeFrame(w io.Writer, f frame) error {
	b := make([]byte, headerLen+len(f.payload))
	b[0], b[1] = f.typ, f.flags
	binary.LittleEndian.PutUint64(b[4:12], uint64(f.pts/time.Microsecond))
	binary.LittleEndian.PutUint32(b[12:16], uint32(len(f.payload)))
	copy(b[headerLen:], f.payload)
	_, err := w.Write(b)
	return err
}

// statusFrame encodes a status as a type-1 frame.
func statusFrame(st status) frame {
	b, _ := json.Marshal(st)
	return frame{typ: frameStatus, payload: b}
}

// parseStatus decodes a type-1 payload.
func parseStatus(payload []byte) (status, error) {
	var st status
	if err := json.Unmarshal(payload, &st); err != nil {
		return st, fmt.Errorf("capture helper status: %w", err)
	}
	return st, nil
}

// parseVideoConfig decodes a type-2 payload: the size, then an avcC atom
// (AVCDecoderConfigurationRecord) whose parameter sets become the
// format's SPS and PPS. Samples must use 4-byte NAL lengths.
func parseVideoConfig(payload []byte) (*capture.VideoFormat, error) {
	if len(payload) < 8+7 {
		return nil, errors.New("capture helper video config is too short")
	}
	f := &capture.VideoFormat{
		Width:  int(binary.LittleEndian.Uint32(payload[0:4])),
		Height: int(binary.LittleEndian.Uint32(payload[4:8])),
	}
	avcc := payload[8:]
	if avcc[0] != 1 {
		return nil, fmt.Errorf("capture helper avcC version %d", avcc[0])
	}
	if size := avcc[4]&3 + 1; size != 4 {
		return nil, fmt.Errorf("capture helper samples use %d-byte NAL lengths, want 4", size)
	}
	p := 6
	sets := func(count int) ([][]byte, error) {
		var out [][]byte
		for i := 0; i < count; i++ {
			if p+2 > len(avcc) {
				return nil, errors.New("capture helper avcC is truncated")
			}
			n := int(binary.BigEndian.Uint16(avcc[p:]))
			p += 2
			if p+n > len(avcc) {
				return nil, errors.New("capture helper avcC is truncated")
			}
			out = append(out, append([]byte(nil), avcc[p:p+n]...))
			p += n
		}
		return out, nil
	}
	var err error
	if f.SPS, err = sets(int(avcc[5] & 0x1f)); err != nil {
		return nil, err
	}
	if p >= len(avcc) {
		return nil, errors.New("capture helper avcC has no PPS count")
	}
	count := int(avcc[p])
	p++
	if f.PPS, err = sets(count); err != nil {
		return nil, err
	}
	if len(f.SPS) == 0 || len(f.PPS) == 0 {
		return nil, errors.New("capture helper avcC has no SPS or PPS")
	}
	return f, nil
}

// parseAudioConfig decodes a type-4 payload into an LPCM format. The
// samples are carried as the helper delivers them: integer (16, 24 or 32
// bits) or IEEE 754 floating point (32 or 64 bits), which the session's
// format does not pin down; the host never converts them. planar is true
// when the samples arrive one channel after another (Core Audio's
// non-interleaved layout, which some attaches deliver as 32-bit float);
// the decoder interleaves those, since MP4 LPCM is interleaved.
func parseAudioConfig(payload []byte) (f *capture.AudioFormat, planar bool, err error) {
	if len(payload) < 12 {
		return nil, false, errors.New("capture helper audio config is too short")
	}
	flags := binary.LittleEndian.Uint32(payload[8:12])
	f = &capture.AudioFormat{
		Codec:         capture.CodecLPCM,
		SampleRate:    int(binary.LittleEndian.Uint32(payload[0:4])),
		Channels:      int(binary.LittleEndian.Uint16(payload[4:6])),
		BitsPerSample: int(binary.LittleEndian.Uint16(payload[6:8])),
		LittleEndian:  flags&asbdIsBigEndian == 0,
		Float:         flags&asbdIsFloat != 0,
	}
	switch {
	case f.SampleRate <= 0 || f.Channels <= 0:
		return nil, false, fmt.Errorf("capture helper audio format %d Hz, %d channels (flags %#x)", f.SampleRate, f.Channels, flags)
	case f.Float && f.BitsPerSample != 32 && f.BitsPerSample != 64:
		return nil, false, fmt.Errorf("capture helper audio has %d-bit floating-point samples (flags %#x)", f.BitsPerSample, flags)
	case !f.Float && f.BitsPerSample != 16 && f.BitsPerSample != 24 && f.BitsPerSample != 32:
		return nil, false, fmt.Errorf("capture helper audio has %d-bit samples (flags %#x)", f.BitsPerSample, flags)
	}
	return f, flags&asbdIsNonInterleaved != 0 && f.Channels > 1, nil
}

// describeAudio names an LPCM format for the daemon log.
func describeAudio(a *capture.AudioFormat, planar bool) string {
	kind := "integer"
	if a.Float {
		kind = "float"
	}
	layout := "interleaved"
	if planar {
		layout = "non-interleaved, interleaved here"
	}
	return fmt.Sprintf("%d Hz, %d ch, %d-bit %s, %s", a.SampleRate, a.Channels, a.BitsPerSample, kind, layout)
}

// interleave turns one sample's planes (every frame of channel 0, then of
// channel 1, ...) into interleaved frames. The sample values are copied
// bit for bit.
func interleave(planes []byte, channels, width int) ([]byte, error) {
	if len(planes)%(channels*width) != 0 {
		return nil, fmt.Errorf("capture helper audio sample of %d bytes does not hold whole %d-channel frames", len(planes), channels)
	}
	frames := len(planes) / (channels * width)
	out := make([]byte, len(planes))
	for c := 0; c < channels; c++ {
		plane := planes[c*frames*width : (c+1)*frames*width]
		for i := 0; i < frames; i++ {
			copy(out[(i*channels+c)*width:], plane[i*width:(i+1)*width])
		}
	}
	return out, nil
}

// decoder turns a device connection's media frames into capture packets,
// remembering the current audio layout between frames.
type decoder struct {
	planar          bool
	channels, width int
}

// packetOf decodes a frame with a fresh decoder, for frames that do not
// depend on an earlier audio config.
func packetOf(f frame) (capture.Packet, bool, error) {
	var d decoder
	return d.packet(f)
}

// packet turns a media frame into a capture packet; ok is false for a
// frame that carries none (a status).
func (d *decoder) packet(f frame) (p capture.Packet, ok bool, err error) {
	switch f.typ {
	case frameVideoConfig:
		v, err := parseVideoConfig(f.payload)
		if err != nil {
			return p, false, err
		}
		return capture.Packet{Kind: capture.VideoConfig, PTS: f.pts, Video: v}, true, nil
	case frameVideoSample:
		return capture.Packet{Kind: capture.Video, PTS: f.pts, Key: f.flags&flagSync != 0, Data: f.payload}, true, nil
	case frameAudioConfig:
		a, planar, err := parseAudioConfig(f.payload)
		if err != nil {
			return p, false, err
		}
		d.planar, d.channels, d.width = planar, a.Channels, a.BitsPerSample/8
		return capture.Packet{Kind: capture.AudioConfig, PTS: f.pts, Audio: a}, true, nil
	case frameAudioSample:
		data := f.payload
		if d.planar {
			if data, err = interleave(data, d.channels, d.width); err != nil {
				return p, false, err
			}
		}
		return capture.Packet{Kind: capture.Audio, PTS: f.pts, Data: data}, true, nil
	}
	return p, false, nil
}
