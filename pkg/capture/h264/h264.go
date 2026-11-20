// Package h264 holds the H.264 bitstream helpers the capture packages need:
// Annex-B and AVCC (length-prefixed) conversion, NAL unit types, parameter
// set extraction, access-unit splitting and the few SPS fields a container
// needs (profile, level, chroma format, bit depth, picture size).
//
// Nothing here decodes video. Emulation-prevention bytes are left in the
// NAL units untouched; only ParseSPS strips them, on its own copy.
package h264

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// NAL unit types used by the capture packages.
const (
	NALSlice    = 1
	NALIDR      = 5
	NALSEI      = 6
	NALSPS      = 7
	NALPPS      = 8
	NALAUD      = 9
	nalTypeMask = 0x1f
)

// ErrTruncated reports an AVCC buffer whose length prefix runs past its end.
var ErrTruncated = errors.New("h264: truncated AVCC sample")

// NALType returns the type of a NAL unit, or 0 for an empty one.
func NALType(nalu []byte) int {
	if len(nalu) == 0 {
		return 0
	}
	return int(nalu[0] & nalTypeMask)
}

// SplitAnnexB splits an Annex-B byte stream into its NAL units, without
// start codes. Both 3-byte (00 00 01) and 4-byte (00 00 00 01) start codes
// are recognised; the zero bytes before a start code are trailing_zero_8bits
// and are dropped. Bytes before the first start code are ignored.
func SplitAnnexB(b []byte) [][]byte {
	var out [][]byte
	start := -1
	for i := 0; i+2 < len(b); {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			if start >= 0 {
				out = appendNALU(out, b[start:i])
			}
			i += 3
			start = i
			continue
		}
		i++
	}
	if start >= 0 {
		out = appendNALU(out, b[start:])
	}
	return out
}

// appendNALU appends a NAL unit with its trailing zero bytes removed (a NAL
// unit never ends in 0x00; those bytes belong to the next start code).
func appendNALU(out [][]byte, n []byte) [][]byte {
	for len(n) > 0 && n[len(n)-1] == 0 {
		n = n[:len(n)-1]
	}
	if len(n) == 0 {
		return out
	}
	return append(out, n)
}

// JoinAVCC writes NAL units as one AVCC sample with 4-byte big-endian
// lengths.
func JoinAVCC(nalus [][]byte) []byte {
	size := 0
	for _, n := range nalus {
		size += 4 + len(n)
	}
	out := make([]byte, 0, size)
	for _, n := range nalus {
		out = binary.BigEndian.AppendUint32(out, uint32(len(n)))
		out = append(out, n...)
	}
	return out
}

// SplitAVCC splits an AVCC sample with 4-byte lengths into its NAL units.
func SplitAVCC(b []byte) ([][]byte, error) {
	var out [][]byte
	for len(b) > 0 {
		if len(b) < 4 {
			return nil, ErrTruncated
		}
		n := binary.BigEndian.Uint32(b)
		b = b[4:]
		if uint64(n) > uint64(len(b)) {
			return nil, ErrTruncated
		}
		out = append(out, b[:n])
		b = b[n:]
	}
	return out, nil
}

// JoinAnnexB writes NAL units as an Annex-B stream with 4-byte start codes.
func JoinAnnexB(nalus [][]byte) []byte {
	size := 0
	for _, n := range nalus {
		size += 4 + len(n)
	}
	out := make([]byte, 0, size)
	for _, n := range nalus {
		out = append(out, 0, 0, 0, 1)
		out = append(out, n...)
	}
	return out
}

// AnnexBToAVCC converts an Annex-B access unit to an AVCC sample.
func AnnexBToAVCC(b []byte) []byte { return JoinAVCC(SplitAnnexB(b)) }

// AVCCToAnnexB converts an AVCC sample to Annex-B with 4-byte start codes.
func AVCCToAnnexB(b []byte) ([]byte, error) {
	nalus, err := SplitAVCC(b)
	if err != nil {
		return nil, err
	}
	return JoinAnnexB(nalus), nil
}

// ParameterSets returns the SPS and PPS NAL units among nalus, in order.
func ParameterSets(nalus [][]byte) (sps, pps [][]byte) {
	for _, n := range nalus {
		switch NALType(n) {
		case NALSPS:
			sps = append(sps, n)
		case NALPPS:
			pps = append(pps, n)
		}
	}
	return sps, pps
}

// HasIDR reports whether any of nalus is an IDR slice.
func HasIDR(nalus [][]byte) bool {
	for _, n := range nalus {
		if NALType(n) == NALIDR {
			return true
		}
	}
	return false
}

// AccessUnits groups a stream's NAL units into access units (one coded
// picture each). A new access unit starts at an access unit delimiter, at a
// parameter set or SEI that follows a slice, or at a slice whose
// first_mb_in_slice is 0 when the current unit already holds a slice.
func AccessUnits(nalus [][]byte) [][][]byte {
	var out [][][]byte
	var cur [][]byte
	hasSlice := false
	flush := func() {
		if len(cur) > 0 {
			out = append(out, cur)
		}
		cur, hasSlice = nil, false
	}
	for _, n := range nalus {
		switch t := NALType(n); {
		case t == NALAUD:
			flush()
		case t == NALSPS || t == NALPPS || t == NALSEI:
			if hasSlice {
				flush()
			}
		case t == NALSlice || t == NALIDR:
			// first_mb_in_slice is ue(v); a value of 0 is the single bit 1.
			if hasSlice && len(n) > 1 && n[1]&0x80 != 0 {
				flush()
			}
			hasSlice = true
		}
		cur = append(cur, n)
	}
	flush()
	return out
}

// ProfileLevel returns the profile_idc, the constraint-flag byte and the
// level_idc of an SPS NAL unit (its bytes 1–3).
func ProfileLevel(sps []byte) (profile, compat, level byte, err error) {
	if len(sps) < 4 || NALType(sps) != NALSPS {
		return 0, 0, 0, fmt.Errorf("h264: not an SPS (%d bytes)", len(sps))
	}
	return sps[1], sps[2], sps[3], nil
}

// CodecString returns the RFC 6381 codec string of an avc3 stream with the
// given SPS: "avc3.PPCCLL" in hex.
func CodecString(sps []byte) (string, error) {
	p, c, l, err := ProfileLevel(sps)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("avc3.%02x%02x%02x", p, c, l), nil
}
