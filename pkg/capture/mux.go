package capture

import (
	"errors"
	"fmt"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/capture/fmp4"
)

const (
	// muxAudioWait is how much video a Mux that asked for audio buffers
	// while it waits for an AudioConfig before it starts without audio.
	muxAudioWait = time.Second
	// muxPendingLimit caps the packets buffered before the start.
	muxPendingLimit = 256
)

// Chunk is a piece of a Mux's output: the init segment, or one fragment.
type Chunk struct {
	Data []byte
	Init bool
	// Track, Time and Key describe a fragment (see fmp4.Fragment).
	Track uint32
	Time  uint64
	Key   bool
}

// Mux turns one subscriber's packets into fragmented MP4. It holds media
// back until it can write the init segment: once it has the video config
// and, when audio was asked for, the audio config (or a second of video
// without one, after which it starts video-only).
type Mux struct {
	fps     int
	audio   bool
	video   *VideoFormat
	audioF  *AudioFormat
	m       *fmp4.Muxer
	pending []Packet
	codecs  string
}

// NewMux returns a Mux for a subscriber with opts (FPS 0 means the
// default; it sets the first video sample's duration).
func NewMux(opts Options) *Mux {
	if opts.FPS <= 0 {
		opts.FPS = DefaultFPS
	}
	return &Mux{fps: opts.FPS, audio: opts.Audio}
}

// Started reports whether the init segment has been written.
func (x *Mux) Started() bool { return x.m != nil }

// HasAudio reports whether the stream has an audio track (after Started).
func (x *Mux) HasAudio() bool { return x.m != nil && x.m.HasAudio() }

// Codecs is the stream's codec string, "avc3.PPCCLL[,audio]" (after
// Started).
func (x *Mux) Codecs() string { return x.codecs }

// VideoFormat is the format the init segment was written with (after
// Started).
func (x *Mux) VideoFormat() *VideoFormat {
	if x.m == nil {
		return nil
	}
	return x.video
}

// MehdOffset is the offset of mehd's duration in the init segment.
func (x *Mux) MehdOffset() int {
	if x.m == nil {
		return 0
	}
	return x.m.MehdOffset()
}

// Duration is the stream's length so far in fmp4.MovieTimescale units.
func (x *Mux) Duration() uint64 {
	if x.m == nil {
		return 0
	}
	return x.m.Duration()
}

// Push consumes one packet and returns what to write: nothing while the
// init segment is held back, then the init segment and the held-back
// fragments, then one fragment per media packet. A sample that cannot be
// muxed returns an error and is skipped; the stream can go on.
func (x *Mux) Push(p Packet) ([]Chunk, error) {
	if x.m != nil {
		c, ok, err := x.write(p)
		if err != nil || !ok {
			return nil, err
		}
		return []Chunk{c}, nil
	}
	switch p.Kind {
	case VideoConfig:
		if p.Video != nil {
			x.video = p.Video
		}
	case AudioConfig:
		if x.audio && p.Audio != nil {
			x.audioF = p.Audio
		}
	case Video, Audio:
		x.pending = append(x.pending, p)
	}
	if !x.ready() {
		return nil, nil
	}
	return x.start()
}

// Flush starts a Mux that still holds its init segment back while it
// waits for an audio config, when it has the video config and media: a
// stream that ends first (a short recording that joined a capture running
// without audio) writes what it has, video-only. Otherwise it returns
// nothing.
func (x *Mux) Flush() ([]Chunk, error) {
	if x.m != nil || x.video == nil || len(x.pending) == 0 {
		return nil, nil
	}
	x.audio, x.audioF = false, nil
	return x.start()
}

func (x *Mux) ready() bool {
	if x.video == nil {
		return len(x.pending) >= muxPendingLimit
	}
	if !x.audio || x.audioF != nil || len(x.pending) >= muxPendingLimit {
		return true
	}
	var first, last time.Duration
	seen := false
	for _, p := range x.pending {
		if p.Kind != Video {
			continue
		}
		if !seen {
			first, seen = p.PTS, true
		}
		last = p.PTS
	}
	return seen && last-first >= muxAudioWait
}

// start writes the init segment and the held-back packets. An audio
// format the muxer refuses is dropped and the stream starts video-only,
// with an error saying so; a video format it refuses is dropped with the
// held-back packets, and the next video config starts it.
func (x *Mux) start() ([]Chunk, error) {
	if x.video == nil {
		x.pending = nil
		return nil, errors.New("capture: no video config before the first frames")
	}
	var errs []error
	m, codecs, err := x.newMuxer(x.audioF)
	if err != nil && x.audioF != nil {
		if vm, vc, verr := x.newMuxer(nil); verr == nil {
			errs = append(errs, fmt.Errorf("capture: dropped the audio, which cannot be muxed (%w); the stream is video-only", err))
			x.audio, x.audioF = false, nil
			m, codecs, err = vm, vc, nil
		}
	}
	if err != nil {
		x.pending, x.video = nil, nil
		return nil, err
	}
	x.m, x.codecs = m, codecs
	out := []Chunk{{Data: m.Init(), Init: true}}
	for _, p := range x.pending {
		c, ok, err := x.write(p)
		if err != nil {
			errs = append(errs, err)
		}
		if ok {
			out = append(out, c)
		}
	}
	x.pending = nil
	return out, errors.Join(errs...)
}

// newMuxer makes the fMP4 muxer and codec string for the video config
// and audio (nil for video-only).
func (x *Mux) newMuxer(audio *AudioFormat) (*fmp4.Muxer, string, error) {
	codecs, err := Codecs(x.video, audio)
	if err != nil {
		return nil, "", err
	}
	v := fmp4.Video{Width: x.video.Width, Height: x.video.Height, SPS: x.video.SPS, PPS: x.video.PPS}
	var a *fmp4.Audio
	if audio != nil {
		a = &fmp4.Audio{
			Codec: audio.Codec, SampleRate: audio.SampleRate, Channels: audio.Channels,
			BitsPerSample: audio.BitsPerSample, LittleEndian: audio.LittleEndian, Float: audio.Float,
			ASC: audio.ASC,
		}
	}
	m, err := fmp4.NewMuxer(v, a, x.fps)
	if err != nil {
		return nil, "", err
	}
	return m, codecs, nil
}

// write muxes one packet after the start.
func (x *Mux) write(p Packet) (Chunk, bool, error) {
	var f fmp4.Fragment
	var err error
	switch p.Kind {
	case VideoConfig:
		if p.Video != nil {
			x.m.SetParameterSets(p.Video.SPS, p.Video.PPS)
		}
		return Chunk{}, false, nil
	case Video:
		f, err = x.m.Video(p.PTS, p.Key, p.Data)
	case Audio:
		if !x.m.HasAudio() {
			return Chunk{}, false, nil
		}
		f, err = x.m.Audio(p.PTS, p.Data)
	default:
		return Chunk{}, false, nil
	}
	if err != nil {
		return Chunk{}, false, err
	}
	return Chunk{Data: f.Bytes, Track: f.Track, Time: f.Time, Key: f.Key}, true, nil
}
