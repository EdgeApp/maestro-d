package fmp4

import (
	"errors"
	"fmt"

	"github.com/devicelab-dev/maestro-runner/pkg/capture/h264"
)

// Track ids and timescales.
const (
	VideoTrackID   = 1
	AudioTrackID   = 2
	VideoTimescale = 90000
	// MovieTimescale is mvhd's timescale, the unit of mehd's duration.
	MovieTimescale = 1000
)

// Audio codecs.
const (
	CodecAAC  = "aac"
	CodecLPCM = "lpcm"
)

// Video describes the H.264 track. Width and Height go in tkhd and the
// sample entry; the first SPS and PPS go in avcC.
type Video struct {
	Width, Height int
	SPS, PPS      [][]byte
}

// Audio describes the audio track.
type Audio struct {
	Codec         string // CodecAAC or CodecLPCM
	SampleRate    int
	Channels      int
	BitsPerSample int    // lpcm
	LittleEndian  bool   // lpcm
	Float         bool   // lpcm: IEEE 754 samples (fpcm), 32 or 64 bits
	ASC           []byte // aac AudioSpecificConfig
}

// validate checks what the init segment and fragments need.
func (v Video) validate() error {
	if len(v.SPS) == 0 || len(v.PPS) == 0 {
		return errors.New("fmp4: video needs an SPS and a PPS")
	}
	if _, _, _, err := h264.ProfileLevel(v.SPS[0]); err != nil {
		return fmt.Errorf("fmp4: %w", err)
	}
	if v.Width <= 0 || v.Height <= 0 || v.Width > 0xffff || v.Height > 0xffff {
		return fmt.Errorf("fmp4: bad video size %dx%d", v.Width, v.Height)
	}
	return nil
}

func (a Audio) validate() error {
	if a.SampleRate <= 0 || a.SampleRate > 0xffff || a.Channels <= 0 || a.Channels > 0xffff {
		return fmt.Errorf("fmp4: bad audio format %d Hz, %d channels", a.SampleRate, a.Channels)
	}
	switch a.Codec {
	case CodecAAC:
		if len(a.ASC) < 2 {
			return errors.New("fmp4: aac needs an AudioSpecificConfig")
		}
	case CodecLPCM:
		if a.BitsPerSample <= 0 || a.BitsPerSample%8 != 0 || a.BitsPerSample > 64 {
			return fmt.Errorf("fmp4: bad lpcm sample size %d", a.BitsPerSample)
		}
		if a.Float && a.BitsPerSample != 32 && a.BitsPerSample != 64 {
			return fmt.Errorf("fmp4: bad floating-point lpcm sample size %d", a.BitsPerSample)
		}
	default:
		return fmt.Errorf("fmp4: unknown audio codec %q", a.Codec)
	}
	return nil
}

// timescale is the audio track's timescale: its sample rate.
func (a Audio) timescale() uint32 { return uint32(a.SampleRate) }

// initSegment builds ftyp+moov and returns it with the offset of mehd's
// 64-bit fragment_duration inside it.
func initSegment(v Video, a *Audio) ([]byte, int) {
	ftyp := box("ftyp", []byte("iso6"), u32(0), []byte("iso6"), []byte("mp41"))
	tracks := 1
	if a != nil {
		tracks = 2
	}
	mvhd := fullBox("mvhd", 0, 0,
		u32(0), u32(0), // creation, modification time
		u32(MovieTimescale), u32(0), // timescale, duration
		u32(0x00010000), u16(0x0100), zeros(10), // rate, volume, reserved
		unityMatrix, zeros(24), // matrix, pre_defined
		u32(uint32(tracks+1)), // next_track_ID
	)
	traks := videoTrak(v)
	trexes := trex(VideoTrackID)
	if a != nil {
		traks = join(traks, audioTrak(*a))
		trexes = join(trexes, trex(AudioTrackID))
	}
	mehd := fullBox("mehd", 1, 0, u64(0))
	mvex := box("mvex", mehd, trexes)
	moov := box("moov", mvhd, traks, mvex)
	// mehd is mvex's first child; its duration follows its 12-byte header.
	mehdAt := len(ftyp) + len(moov) - len(mvex) + 8 + 12
	return join(ftyp, moov), mehdAt
}

func trex(track uint32) []byte {
	return fullBox("trex", 0, 0, u32(track), u32(1), u32(0), u32(0), u32(0))
}

func tkhd(track uint32, volume uint16, width, height int) []byte {
	return fullBox("tkhd", 0, 3, // enabled, in movie
		u32(0), u32(0), u32(track), u32(0), u32(0), // times, track_ID, reserved, duration
		zeros(8), u16(0), u16(0), u16(volume), u16(0), // reserved, layer, alternate_group, volume, reserved
		unityMatrix, u32(uint32(width)<<16), u32(uint32(height)<<16),
	)
}

func mdia(timescale uint32, handler, name string, mhd, stsd []byte) []byte {
	mdhd := fullBox("mdhd", 0, 0, u32(0), u32(0), u32(timescale), u32(0), u16(0x55c4), u16(0)) // language "und"
	hdlr := fullBox("hdlr", 0, 0, u32(0), []byte(handler), zeros(12), []byte(name), u8(0))
	dref := fullBox("dref", 0, 0, u32(1), fullBox("url ", 0, 1))
	stbl := box("stbl", stsd,
		fullBox("stts", 0, 0, u32(0)),
		fullBox("stsc", 0, 0, u32(0)),
		fullBox("stsz", 0, 0, u32(0), u32(0)),
		fullBox("stco", 0, 0, u32(0)),
	)
	minf := box("minf", mhd, box("dinf", dref), stbl)
	return box("mdia", mdhd, hdlr, minf)
}

func videoTrak(v Video) []byte {
	vmhd := fullBox("vmhd", 0, 1, u16(0), zeros(6))
	entry := box("avc3",
		zeros(6), u16(1), // reserved, data_reference_index
		u16(0), u16(0), zeros(12), // pre_defined, reserved, pre_defined
		u16(uint16(v.Width)), u16(uint16(v.Height)),
		u32(0x00480000), u32(0x00480000), u32(0), // 72 dpi, reserved
		u16(1), zeros(32), u16(0x0018), u16(0xffff), // frame_count, compressorname, depth, pre_defined -1
		avcC(v.SPS, v.PPS),
	)
	stsd := fullBox("stsd", 0, 0, u32(1), entry)
	return box("trak", tkhd(VideoTrackID, 0, v.Width, v.Height), mdia(VideoTimescale, "vide", "VideoHandler", vmhd, stsd))
}

// avcC is the AVCDecoderConfigurationRecord (ISO/IEC 14496-15 5.3.3.1).
func avcC(sps, pps [][]byte) []byte {
	first := sps[0]
	b := []byte{1, first[1], first[2], first[3], 0xfc | 3, 0xe0 | byte(len(sps))}
	for _, s := range sps {
		b = append(append(b, u16(uint16(len(s)))...), s...)
	}
	b = append(b, byte(len(pps)))
	for _, p := range pps {
		b = append(append(b, u16(uint16(len(p)))...), p...)
	}
	if h264.HighProfile(first[1]) {
		chroma, luma, chromaDepth := 1, 8, 8
		if info, err := h264.ParseSPS(first); err == nil {
			chroma, luma, chromaDepth = info.ChromaFormat, info.BitDepthLuma, info.BitDepthChroma
		}
		b = append(b, 0xfc|byte(chroma&3), 0xf8|byte((luma-8)&7), 0xf8|byte((chromaDepth-8)&7), 0)
	}
	return box("avcC", b)
}

func audioTrak(a Audio) []byte {
	smhd := fullBox("smhd", 0, 0, u16(0), u16(0))
	var entry []byte
	switch a.Codec {
	case CodecAAC:
		entry = box("mp4a", audioEntryFields(a, 16), esds(a.ASC))
	default: // CodecLPCM; validate rejects anything else
		// ISO/IEC 23003-5: ipcm for integer samples, fpcm for IEEE 754
		// ones; pcmC's format_flags bit 0 is little-endian.
		flags := byte(0)
		if a.LittleEndian {
			flags = 1
		}
		typ := "ipcm"
		if a.Float {
			typ = "fpcm"
		}
		pcmC := fullBox("pcmC", 0, 0, u8(flags), u8(uint8(a.BitsPerSample)))
		entry = box(typ, audioEntryFields(a, a.BitsPerSample), pcmC)
	}
	stsd := fullBox("stsd", 0, 0, u32(1), entry)
	return box("trak", tkhd(AudioTrackID, 0x0100, 0, 0), mdia(a.timescale(), "soun", "SoundHandler", smhd, stsd))
}

// audioEntryFields are the AudioSampleEntry (version 0) fields before its
// child boxes.
func audioEntryFields(a Audio, sampleSize int) []byte {
	return join(
		zeros(6), u16(1), // reserved, data_reference_index
		zeros(8), // reserved
		u16(uint16(a.Channels)), u16(uint16(sampleSize)),
		u16(0), u16(0), // pre_defined, reserved
		u32(uint32(a.SampleRate)<<16),
	)
}

// esds wraps an AudioSpecificConfig in an ES_Descriptor (ISO/IEC 14496-1).
func esds(asc []byte) []byte {
	dsi := descriptor(0x05, asc)
	dcd := descriptor(0x04, join(
		u8(0x40),                 // objectTypeIndication: MPEG-4 audio
		u8(0x15),                 // streamType audio (5) << 2 | reserved 1
		zeros(3), u32(0), u32(0), // bufferSizeDB, maxBitrate, avgBitrate
		dsi,
	))
	sl := descriptor(0x06, []byte{0x02})
	es := descriptor(0x03, join(u16(AudioTrackID), u8(0), dcd, sl))
	return fullBox("esds", 0, 0, es)
}

// descriptor writes an MPEG-4 descriptor with its expandable size field.
func descriptor(tag byte, payload []byte) []byte {
	n := len(payload)
	var size []byte
	for {
		size = append([]byte{byte(n & 0x7f)}, size...)
		n >>= 7
		if n == 0 {
			break
		}
	}
	for i := 0; i < len(size)-1; i++ {
		size[i] |= 0x80
	}
	return join(u8(tag), size, payload)
}
