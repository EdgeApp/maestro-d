package fmp4

import (
	"errors"
	"fmt"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/capture/h264"
)

// Sample flags (ISO/IEC 14496-12 8.8.3.1).
const (
	// FlagsSync: sample_depends_on = 2 (depends on no other sample).
	FlagsSync = 0x02000000
	// FlagsNonSync: sample_depends_on = 1 and sample_is_non_sync_sample.
	FlagsNonSync = 0x01010000
)

// Duration bounds of an estimated sample duration.
const (
	minSampleDuration = time.Millisecond
	maxSampleDuration = time.Second
	// aacFrameSamples is the sample count of one AAC-LC frame.
	aacFrameSamples = 1024
)

// Fragment is one moof+mdat, with what an index of it needs.
type Fragment struct {
	Bytes []byte
	Track uint32
	// Time is the sample's decode time (tfdt) in the track's timescale.
	Time uint64
	// Duration is the sample's duration in the track's timescale.
	Duration uint32
	Key      bool
}

// Muxer writes one subscriber's stream: the init segment, then one
// fragment per sample, with times rebased so the first sample of either
// track starts at 0.
type Muxer struct {
	video    Video
	audio    *Audio
	fps      int
	init     []byte
	mehdAt   int
	seq      uint32
	base     time.Duration
	haveBase bool
	tracks   [2]trackState
}

type trackState struct {
	timescale uint32
	started   bool
	lastPTS   time.Duration
	lastTime  uint64
	end       uint64 // lastTime + last duration
}

// NewMuxer validates the formats and builds the init segment. audio is nil
// for a video-only stream. fps is the first video sample's assumed rate.
func NewMuxer(video Video, audio *Audio, fps int) (*Muxer, error) {
	if err := video.validate(); err != nil {
		return nil, err
	}
	if audio != nil {
		if err := audio.validate(); err != nil {
			return nil, err
		}
	}
	if fps <= 0 {
		return nil, fmt.Errorf("fmp4: bad fps %d", fps)
	}
	m := &Muxer{video: video, audio: audio, fps: fps}
	m.init, m.mehdAt = initSegment(video, audio)
	m.tracks[0].timescale = VideoTimescale
	if audio != nil {
		m.tracks[1].timescale = audio.timescale()
	}
	return m, nil
}

// Init returns the init segment (ftyp+moov).
func (m *Muxer) Init() []byte { return m.init }

// MehdOffset is the offset in Init of mehd's 64-bit fragment_duration,
// which a finished recording patches with Duration.
func (m *Muxer) MehdOffset() int { return m.mehdAt }

// HasAudio reports whether the init segment has an audio track.
func (m *Muxer) HasAudio() bool { return m.audio != nil }

// SetParameterSets replaces the SPS and PPS written in band before each
// keyframe (a format change: the init segment keeps the first ones).
func (m *Muxer) SetParameterSets(sps, pps [][]byte) {
	if len(sps) > 0 && len(pps) > 0 {
		m.video.SPS, m.video.PPS = sps, pps
	}
}

// Duration is the stream's length so far in MovieTimescale units: the end
// of the latest sample of either track.
func (m *Muxer) Duration() uint64 {
	var d uint64
	for _, t := range m.tracks {
		if t.timescale == 0 || !t.started {
			continue
		}
		if v := t.end * MovieTimescale / uint64(t.timescale); v > d {
			d = v
		}
	}
	return d
}

// Video writes one AVCC video sample. A keyframe gets the current SPS and
// PPS in front of its NAL units unless it already carries an SPS.
func (m *Muxer) Video(pts time.Duration, key bool, sample []byte) (Fragment, error) {
	nalus, err := h264.SplitAVCC(sample)
	if err != nil {
		return Fragment{}, err
	}
	if key && !hasSPS(nalus) {
		ps := append(append([][]byte{}, m.video.SPS...), m.video.PPS...)
		sample = append(h264.JoinAVCC(ps), sample...)
	}
	first := time.Second / time.Duration(m.fps)
	flags := uint32(FlagsNonSync)
	if key {
		flags = FlagsSync
	}
	return m.fragment(0, VideoTrackID, pts, first, key, flags, sample), nil
}

// Audio writes one audio sample: an AAC frame, or interleaved PCM.
func (m *Muxer) Audio(pts time.Duration, data []byte) (Fragment, error) {
	if m.audio == nil {
		return Fragment{}, errors.New("fmp4: stream has no audio track")
	}
	frames := aacFrameSamples
	if m.audio.Codec == CodecLPCM {
		frames = len(data) / (m.audio.Channels * m.audio.BitsPerSample / 8)
	}
	first := time.Duration(frames) * time.Second / time.Duration(m.audio.SampleRate)
	return m.fragment(1, AudioTrackID, pts, first, true, FlagsSync, data), nil
}

func hasSPS(nalus [][]byte) bool {
	for _, n := range nalus {
		if h264.NALType(n) == h264.NALSPS {
			return true
		}
	}
	return false
}

// fragment rebases pts, estimates the duration and builds moof+mdat.
func (m *Muxer) fragment(idx int, track uint32, pts, first time.Duration, key bool, flags uint32, data []byte) Fragment {
	if !m.haveBase {
		m.base, m.haveBase = pts, true
	}
	t := &m.tracks[idx]
	dur := first
	if t.started {
		dur = pts - t.lastPTS
	}
	dur = min(max(dur, minSampleDuration), maxSampleDuration)
	rel := max(pts-m.base, 0)
	tfdt := toTimescale(rel, t.timescale)
	if t.started && tfdt <= t.lastTime {
		tfdt = t.lastTime + 1
	}
	ticks := uint32(toTimescale(dur, t.timescale))
	if ticks == 0 {
		ticks = 1
	}
	t.started, t.lastPTS, t.lastTime, t.end = true, pts, tfdt, tfdt+uint64(ticks)
	m.seq++
	return Fragment{
		Bytes:    moofMdat(m.seq, track, tfdt, ticks, flags, data),
		Track:    track,
		Time:     tfdt,
		Duration: ticks,
		Key:      key,
	}
}

// toTimescale converts d to ticks of timescale, rounding to the nearest.
func toTimescale(d time.Duration, timescale uint32) uint64 {
	if d <= 0 {
		return 0
	}
	sec := uint64(d / time.Second)
	rem := uint64(d % time.Second)
	return sec*uint64(timescale) + (rem*uint64(timescale)+uint64(time.Second)/2)/uint64(time.Second)
}

// sampleDurationAt is where moofMdat's trun keeps the sample's duration:
// past moof's header, mfhd, traf's header, tfhd, tfdt (version 1) and
// trun's header, sample_count and data_offset.
const sampleDurationAt = 8 + 16 + 8 + 16 + 20 + 12 + 4 + 4

// SetSampleDuration rewrites the duration (in its track's timescale) of a
// fragment this package wrote, in place: a consumer that holds a sample
// until the next one of its track arrives can write the real gap instead
// of the estimate. It reports false, changing nothing, for bytes that are
// not such a fragment.
func SetSampleDuration(fragment []byte, ticks uint32) bool {
	if len(fragment) < sampleDurationAt+4 || string(fragment[4:8]) != "moof" ||
		string(fragment[sampleDurationAt-16:sampleDurationAt-12]) != "trun" {
		return false
	}
	copy(fragment[sampleDurationAt:], u32(ticks))
	return true
}

// moofMdat builds a one-sample fragment.
func moofMdat(seq, track uint32, tfdt uint64, duration, flags uint32, data []byte) []byte {
	const (
		tfhdDefaultBaseIsMoof = 0x020000
		trunFlags             = 0x000001 | 0x000100 | 0x000200 | 0x000400 // data offset, duration, size, flags
		moofSize              = 8 + 16 + 8 + 16 + 20 + 32
	)
	mfhd := fullBox("mfhd", 0, 0, u32(seq))
	tfhd := fullBox("tfhd", 0, tfhdDefaultBaseIsMoof, u32(track))
	tfdtBox := fullBox("tfdt", 1, 0, u64(tfdt))
	trun := fullBox("trun", 0, trunFlags,
		u32(1),                  // sample_count
		u32(uint32(moofSize+8)), // data_offset: past moof and mdat's header
		u32(duration), u32(uint32(len(data))), u32(flags),
	)
	moof := box("moof", mfhd, box("traf", tfhd, tfdtBox, trun))
	return join(moof, box("mdat", data))
}
