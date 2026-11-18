// Package capture is maestro-d's screen capture core: the packets a device
// source produces, the per-device Session that fans them out to every
// viewer and recorder, and the Recorder that writes them to an MP4 file.
//
// Sources hand over device-encoded H.264 and the device's audio (AAC or
// LPCM) as they arrive; nothing here decodes or re-encodes. pkg/capture/fmp4
// wraps the samples in fragmented MP4. This package never imports
// pkg/daemon and builds on every GOOS.
package capture

import (
	"context"
	"time"
)

// Kind is a packet's kind.
type Kind uint8

// Packet kinds.
const (
	VideoConfig Kind = iota + 1 // Video holds the stream's format and parameter sets
	Video                       // Data is one AVCC sample (4-byte lengths)
	AudioConfig                 // Audio holds the audio format
	Audio                       // Data is one AAC frame, or interleaved PCM
)

func (k Kind) String() string {
	switch k {
	case VideoConfig:
		return "video-config"
	case Video:
		return "video"
	case AudioConfig:
		return "audio-config"
	case Audio:
		return "audio"
	}
	return "unknown"
}

// Packet is one unit a Source emits. Data is shared by every subscriber and
// must not be modified after emit.
type Packet struct {
	Kind  Kind
	PTS   time.Duration // device clock; monotonic per source
	Key   bool          // Video only: sync sample
	Data  []byte
	Video *VideoFormat // VideoConfig only
	Audio *AudioFormat // AudioConfig only
}

// VideoFormat is an H.264 stream's size and parameter sets (NAL units
// without start codes or lengths).
type VideoFormat struct {
	Width, Height int
	SPS, PPS      [][]byte
}

// Audio codecs.
const (
	CodecAAC  = "aac"
	CodecLPCM = "lpcm"
)

// AudioFormat is an audio stream's format.
type AudioFormat struct {
	Codec         string // CodecAAC or CodecLPCM
	SampleRate    int
	Channels      int
	BitsPerSample int    // lpcm
	LittleEndian  bool   // lpcm
	Float         bool   // lpcm: IEEE 754 samples (32 or 64 bits), not integers
	ASC           []byte // aac AudioSpecificConfig
}

// Mode says when a Session runs its Source.
type Mode uint8

const (
	// OnDemand sources start with the first subscriber and stop a while
	// after the last one leaves.
	OnDemand Mode = iota
	// AlwaysOn sources run for the attachment's life, started with
	// Session.Start.
	AlwaysOn
)

// Source produces packets until ctx ends or it fails. Run returns the
// terminal error, or nil when ctx was cancelled. emit never blocks and may
// be called from several goroutines.
type Source interface {
	Run(ctx context.Context, opts Options, emit func(Packet)) error
	Mode() Mode
	// FPSControl reports whether Options.FPS is a ceiling the device
	// honours (false: the device picks its own rate).
	FPSControl() bool
}
