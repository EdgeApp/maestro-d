package scrcpy

// protocol.go: the scrcpy-server 5.0.1 socket protocol, as maestro-d starts
// it (send_stream_meta and send_frame_meta on, send_device_meta off).
//
// Video socket: the dummy byte (read by the connect loop), a u32 codec id,
// then 12-byte headers. A header whose most significant bit is set is a
// session packet (u32 flags, u32 width, u32 height). Otherwise it is a media
// packet: u64 PTS and flags (bit 62 config, bit 61 key frame, the low 61
// bits the PTS in microseconds), u32 size, then the payload.
//
// Audio socket: a u32 codec id (0: the device disabled audio, 1: an error),
// then media packets only. All integers are big-endian.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/capture"
	"github.com/devicelab-dev/maestro-runner/pkg/capture/h264"
)

// Codec ids, and the two audio ids that mean "no audio stream".
const (
	codecH264            = 0x68323634 // "h264"
	codecAAC             = 0x00616163 // "\0aac"
	codecDisabled        = 0
	codecError           = 1
	flagSession   uint64 = 1 << 63
	flagConfig    uint64 = 1 << 62
	flagKey       uint64 = 1 << 61
	ptsMask       uint64 = flagKey - 1
	// maxPacket bounds a packet's declared size, so a corrupt header
	// cannot make the reader allocate gigabytes.
	maxPacket = 64 << 20
)

// header is one 12-byte packet header.
type header struct {
	session       bool
	width, height int // session packets
	config, key   bool
	pts           time.Duration
	size          int
}

func readU32(r io.Reader) (uint32, error) {
	var b [4]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(b[:]), nil
}

func readHeader(r io.Reader) (header, error) {
	var b [12]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return header{}, err
	}
	v := binary.BigEndian.Uint64(b[:8])
	if v&flagSession != 0 {
		return header{
			session: true,
			width:   int(binary.BigEndian.Uint32(b[4:8])),
			height:  int(binary.BigEndian.Uint32(b[8:12])),
		}, nil
	}
	h := header{
		config: v&flagConfig != 0,
		key:    v&flagKey != 0,
		pts:    time.Duration(v&ptsMask) * time.Microsecond,
		size:   int(binary.BigEndian.Uint32(b[8:12])),
	}
	if h.size > maxPacket {
		return h, fmt.Errorf("scrcpy: packet of %d bytes exceeds %d", h.size, maxPacket)
	}
	return h, nil
}

func readPayload(r io.Reader, n int) ([]byte, error) {
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, unexpected(err)
	}
	return buf, nil
}

// unexpected turns an EOF inside a packet into io.ErrUnexpectedEOF.
func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// readVideo reads the video socket after its dummy byte until it ends,
// emitting a VideoConfig per config packet (sized by the latest session
// packet, else by the SPS) and a Video per media packet (Annex-B turned
// into AVCC). It returns io.EOF when the stream ends between packets.
func readVideo(r io.Reader, emit func(capture.Packet)) error {
	id, err := readU32(r)
	if err != nil {
		return err
	}
	if id != codecH264 {
		return fmt.Errorf("scrcpy: video codec id %#x, want h264", id)
	}
	var width, height int
	for {
		h, err := readHeader(r)
		if err != nil {
			return err
		}
		if h.session {
			width, height = h.width, h.height
			continue
		}
		data, err := readPayload(r, h.size)
		if err != nil {
			return err
		}
		if h.config {
			f, err := videoFormat(data, width, height)
			if err != nil {
				return err
			}
			emit(capture.Packet{Kind: capture.VideoConfig, PTS: h.pts, Video: f})
			continue
		}
		emit(capture.Packet{Kind: capture.Video, PTS: h.pts, Key: h.key, Data: h264.AnnexBToAVCC(data)})
	}
}

// videoFormat reads a config packet's SPS and PPS. Width and height come
// from the session packet when there was one, else from the SPS.
func videoFormat(annexB []byte, width, height int) (*capture.VideoFormat, error) {
	sps, pps := h264.ParameterSets(h264.SplitAnnexB(annexB))
	if len(sps) == 0 || len(pps) == 0 {
		return nil, errors.New("scrcpy: video config packet has no SPS/PPS")
	}
	if width <= 0 || height <= 0 {
		info, err := h264.ParseSPS(sps[0])
		if err != nil {
			return nil, err
		}
		width, height = info.Width, info.Height
	}
	return &capture.VideoFormat{Width: width, Height: height, SPS: sps, PPS: pps}, nil
}

// errNoAudio is the audio socket's codec id 0 or 1: the device sends no
// audio. The reason is in the message.
type errNoAudio struct{ id uint32 }

func (e errNoAudio) Error() string {
	if e.id == codecDisabled {
		return "scrcpy: the device disabled audio capture (Android 11 needs the screen unlocked when capture starts)"
	}
	return "scrcpy: the device's audio capture failed to start"
}

// readAudio reads the audio socket until it ends: an AudioConfig per
// config packet (the AudioSpecificConfig), an Audio per media packet. A
// codec id 0 or 1 returns errNoAudio; the end of the stream returns
// io.EOF.
func readAudio(r io.Reader, emit func(capture.Packet)) error {
	id, err := readU32(r)
	if err != nil {
		return err
	}
	switch id {
	case codecAAC:
	case codecDisabled, codecError:
		return errNoAudio{id}
	default:
		return fmt.Errorf("scrcpy: audio codec id %#x, want aac", id)
	}
	for {
		h, err := readHeader(r)
		if err != nil {
			return err
		}
		if h.session {
			return errors.New("scrcpy: session packet on the audio socket")
		}
		data, err := readPayload(r, h.size)
		if err != nil {
			return err
		}
		if h.config {
			f, err := audioFormat(data)
			if err != nil {
				return err
			}
			emit(capture.Packet{Kind: capture.AudioConfig, PTS: h.pts, Audio: f})
			continue
		}
		emit(capture.Packet{Kind: capture.Audio, PTS: h.pts, Data: data})
	}
}

// aacRates are the AudioSpecificConfig sampling frequencies by index.
var aacRates = [...]int{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}

// audioFormat reads the sample rate and channel count from an
// AudioSpecificConfig (ISO/IEC 14496-3 1.6.2.1).
func audioFormat(asc []byte) (*capture.AudioFormat, error) {
	if len(asc) < 2 {
		return nil, fmt.Errorf("scrcpy: AudioSpecificConfig of %d bytes", len(asc))
	}
	var bits uint64
	for i := 0; i < 8; i++ {
		bits <<= 8
		if i < len(asc) {
			bits |= uint64(asc[i])
		}
	}
	take := func(n uint) int {
		v := int(bits >> (64 - n))
		bits <<= n
		return v
	}
	if take(5) == 31 { // audioObjectType escape
		take(6)
	}
	idx := take(4)
	var rate int
	switch {
	case idx == 15:
		if len(asc) < 5 {
			return nil, errors.New("scrcpy: truncated AudioSpecificConfig")
		}
		rate = take(24)
	case idx < len(aacRates):
		rate = aacRates[idx]
	default:
		return nil, fmt.Errorf("scrcpy: AudioSpecificConfig sampling index %d", idx)
	}
	channels := take(4)
	if channels == 0 || channels > 7 {
		return nil, fmt.Errorf("scrcpy: AudioSpecificConfig channel configuration %d", channels)
	}
	if channels == 7 {
		channels = 8
	}
	return &capture.AudioFormat{Codec: capture.CodecAAC, SampleRate: rate, Channels: channels, ASC: asc}, nil
}
