package capture

import (
	"errors"
	"fmt"
	"strings"

	"github.com/devicelab-dev/maestro-runner/pkg/capture/h264"
)

// VideoCodec returns the RFC 6381 codec string of a video format, as used
// by MSE and the X-Maestro-Codecs header: "avc3.PPCCLL" from its SPS.
func VideoCodec(f *VideoFormat) (string, error) {
	if f == nil || len(f.SPS) == 0 {
		return "", errors.New("capture: video format has no SPS")
	}
	return h264.CodecString(f.SPS[0])
}

// AudioCodec returns the codec string of an audio format:
// "mp4a.40.<audioObjectType>" for AAC (from its AudioSpecificConfig) and
// "ipcm" for integer LPCM and "fpcm" for floating-point LPCM.
func AudioCodec(f *AudioFormat) (string, error) {
	if f == nil {
		return "", errors.New("capture: no audio format")
	}
	switch f.Codec {
	case CodecLPCM:
		if f.Float {
			return "fpcm", nil
		}
		return "ipcm", nil
	case CodecAAC:
		aot, err := audioObjectType(f.ASC)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("mp4a.40.%d", aot), nil
	}
	return "", fmt.Errorf("capture: unknown audio codec %q", f.Codec)
}

// audioObjectType reads an AudioSpecificConfig's object type: 5 bits, or
// 32 + 6 more bits when the first five are all ones.
func audioObjectType(asc []byte) (int, error) {
	if len(asc) < 1 {
		return 0, errors.New("capture: empty AudioSpecificConfig")
	}
	aot := int(asc[0] >> 3)
	if aot == 31 {
		if len(asc) < 2 {
			return 0, errors.New("capture: short AudioSpecificConfig")
		}
		aot = 32 + (int(asc[0]&7)<<3 | int(asc[1]>>5))
	}
	return aot, nil
}

// Codecs joins the video codec and, when audio is non-nil, the audio
// codec, comma-separated.
func Codecs(video *VideoFormat, audio *AudioFormat) (string, error) {
	v, err := VideoCodec(video)
	if err != nil {
		return "", err
	}
	parts := []string{v}
	if audio != nil {
		a, err := AudioCodec(audio)
		if err != nil {
			return "", err
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, ","), nil
}
