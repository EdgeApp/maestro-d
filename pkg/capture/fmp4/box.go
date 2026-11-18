// Package fmp4 writes fragmented MP4 (ISO/IEC 14496-12) for live capture:
// an init segment and one moof+mdat fragment per sample, for an H.264
// video track (avc3, parameter sets in band) and an optional AAC (mp4a) or
// LPCM (ipcm for integer samples, fpcm for floating point; ISO/IEC 23003-5)
// audio track. It also writes the mfra index a finished recording ends with.
//
// The package knows nothing about capture sessions; pkg/capture feeds it.
package fmp4

import "encoding/binary"

// box returns an ISO BMFF box: a 32-bit size, the four-character type and
// the concatenated payload parts.
func box(typ string, parts ...[]byte) []byte {
	size := 8
	for _, p := range parts {
		size += len(p)
	}
	out := make([]byte, 0, size)
	out = binary.BigEndian.AppendUint32(out, uint32(size))
	out = append(out, typ...)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// fullBox returns a box whose payload starts with a version byte and
// 24 bits of flags.
func fullBox(typ string, version byte, flags uint32, parts ...[]byte) []byte {
	vf := u32(uint32(version)<<24 | flags&0xffffff)
	return box(typ, append([][]byte{vf}, parts...)...)
}

func u8(v uint8) []byte   { return []byte{v} }
func u16(v uint16) []byte { return binary.BigEndian.AppendUint16(nil, v) }
func u32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }
func u64(v uint64) []byte { return binary.BigEndian.AppendUint64(nil, v) }
func zeros(n int) []byte  { return make([]byte, n) }
func join(p ...[]byte) []byte {
	var out []byte
	for _, b := range p {
		out = append(out, b...)
	}
	return out
}

// unityMatrix is the identity transformation matrix of mvhd and tkhd.
var unityMatrix = join(
	u32(0x00010000), u32(0), u32(0),
	u32(0), u32(0x00010000), u32(0),
	u32(0), u32(0), u32(0x40000000),
)
