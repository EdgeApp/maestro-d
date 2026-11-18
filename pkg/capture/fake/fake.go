// Package fake is a deterministic capture.Source that loops fixtures
// embedded in the binary: a 640x360 H.264 clip (45 frames, a keyframe every
// 15) and 48 kHz stereo AAC. The mock driver captures through it, and the
// capture packages' tests read its fixtures.
//
// fixtures/make.sh regenerates the fixtures with FFmpeg.
package fake

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/capture"
	"github.com/devicelab-dev/maestro-runner/pkg/capture/h264"
)

//go:embed fixtures
var fixtures embed.FS

// AAC fixture format.
const (
	AACSampleRate   = 48000
	AACChannels     = 2
	aacFrameSamples = 1024
	// PCM fixture format: 48 kHz, stereo, signed 16-bit little-endian.
	PCMSampleRate    = 48000
	PCMChannels      = 2
	PCMBitsPerSample = 16
)

func mustRead(name string) []byte {
	b, err := fixtures.ReadFile(name)
	if err != nil {
		panic(fmt.Sprintf("fake: missing fixture %s: %v", name, err))
	}
	return b
}

// Clip is clip.h264: 640x360, 45 frames at 15 fps, H.264 High, Annex-B,
// an IDR every 15 frames.
func Clip() []byte { return mustRead("fixtures/clip.h264") }

// RotatedClip is clip-rotated.h264: 360x640, 15 frames.
func RotatedClip() []byte { return mustRead("fixtures/clip-rotated.h264") }

// ASC is the AAC frames' AudioSpecificConfig (AAC-LC, 48 kHz, stereo).
func ASC() []byte { return mustRead("fixtures/asc.bin") }

// PCM is 0.5 s of 48 kHz stereo s16le.
func PCM() []byte { return mustRead("fixtures/pcm.raw") }

// FloatPCM is 0.25 s of 48 kHz stereo 32-bit float little-endian.
func FloatPCM() []byte { return mustRead("fixtures/pcm-f32.raw") }

// AACFrames are the raw AAC frames (ADTS headers stripped), in order.
func AACFrames() [][]byte {
	entries, err := fs.ReadDir(fixtures, "fixtures/aac")
	if err != nil {
		panic(fmt.Sprintf("fake: missing fixture fixtures/aac: %v", err))
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	out := make([][]byte, len(names))
	for i, n := range names {
		out[i] = mustRead(path.Join("fixtures/aac", n))
	}
	return out
}

// Frame is one access unit of a clip as a source emits it: an AVCC sample
// without parameter sets or access unit delimiters.
type Frame struct {
	Key  bool
	Data []byte
}

// ParseClip splits an Annex-B clip into its video format (from the first
// SPS and PPS) and its frames.
func ParseClip(annexB []byte) (*capture.VideoFormat, []Frame, error) {
	nalus := h264.SplitAnnexB(annexB)
	sps, pps := h264.ParameterSets(nalus)
	if len(sps) == 0 || len(pps) == 0 {
		return nil, nil, fmt.Errorf("fake: clip has no SPS/PPS")
	}
	info, err := h264.ParseSPS(sps[0])
	if err != nil {
		return nil, nil, err
	}
	format := &capture.VideoFormat{Width: info.Width, Height: info.Height, SPS: sps[:1], PPS: pps[:1]}
	var frames []Frame
	for _, au := range h264.AccessUnits(nalus) {
		var keep [][]byte
		for _, n := range au {
			switch h264.NALType(n) {
			case h264.NALSPS, h264.NALPPS, h264.NALAUD:
			default:
				keep = append(keep, n)
			}
		}
		if len(keep) == 0 {
			continue
		}
		frames = append(frames, Frame{Key: h264.HasIDR(keep), Data: h264.JoinAVCC(keep)})
	}
	if len(frames) == 0 || !frames[0].Key {
		return nil, nil, fmt.Errorf("fake: clip does not start with a keyframe")
	}
	return format, frames, nil
}

// Source loops the embedded clip, and the AAC frames when audio is asked
// for, in real time. Video frame k (counting across passes) has PTS
// k/fps, so PTS never resets; each pass starts with the clip's keyframe.
// AAC frame j has PTS j*1024/48000 s, looping on its own clock.
type Source struct {
	// sleep waits until d after the run started; tests replace it.
	sleep func(ctx context.Context, start time.Time, d time.Duration) error
}

// New returns a fake source.
func New() *Source { return &Source{sleep: sleepUntil} }

// Mode is capture.OnDemand.
func (*Source) Mode() capture.Mode { return capture.OnDemand }

// FPSControl is true: the requested rate is the rate emitted.
func (*Source) FPSControl() bool { return true }

func sleepUntil(ctx context.Context, start time.Time, d time.Duration) error {
	wait := time.Until(start.Add(d))
	if wait <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Run emits the configs, then frames until ctx ends; it returns nil then.
func (s *Source) Run(ctx context.Context, opts capture.Options, emit func(capture.Packet)) error {
	opts, err := opts.Normalize()
	if err != nil {
		return err
	}
	format, frames, err := ParseClip(Clip())
	if err != nil {
		return err
	}
	emit(capture.Packet{Kind: capture.VideoConfig, Video: format})
	var aac [][]byte
	if opts.Audio {
		aac = AACFrames()
		emit(capture.Packet{Kind: capture.AudioConfig, Audio: &capture.AudioFormat{
			Codec: capture.CodecAAC, SampleRate: AACSampleRate, Channels: AACChannels, ASC: ASC(),
		}})
	}
	sleep := s.sleep
	if sleep == nil {
		sleep = sleepUntil
	}
	start := time.Now()
	var vk, ak int64
	for {
		vPTS := time.Duration(vk) * time.Second / time.Duration(opts.FPS)
		aPTS := time.Duration(ak) * aacFrameSamples * time.Second / AACSampleRate
		next, video := vPTS, true
		if len(aac) > 0 && aPTS < vPTS {
			next, video = aPTS, false
		}
		if err := sleep(ctx, start, next); err != nil {
			return nil
		}
		if video {
			f := frames[vk%int64(len(frames))]
			emit(capture.Packet{Kind: capture.Video, PTS: vPTS, Key: f.Key, Data: f.Data})
			vk++
		} else {
			emit(capture.Packet{Kind: capture.Audio, PTS: aPTS, Data: aac[ak%int64(len(aac))]})
			ak++
		}
	}
}
