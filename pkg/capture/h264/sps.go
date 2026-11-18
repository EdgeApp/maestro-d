package h264

import (
	"errors"
	"fmt"
)

// SPS holds the sequence parameter set fields a container needs.
type SPS struct {
	Profile, Compat, Level byte
	// ChromaFormat is chroma_format_idc (1 = 4:2:0, the default for
	// profiles without the field).
	ChromaFormat int
	// BitDepthLuma and BitDepthChroma are the sample bit depths (8 unless
	// the SPS says otherwise).
	BitDepthLuma, BitDepthChroma int
	// Width and Height are the cropped picture size in pixels.
	Width, Height int
}

var errSPSShort = errors.New("h264: SPS ends early")

// HighProfile reports whether an SPS with this profile_idc carries the
// chroma format and bit depth fields (H.264 7.3.2.1.1), and so whether its
// avcC record has the matching extension.
func HighProfile(p byte) bool {
	switch p {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		return true
	}
	return false
}

// ParseSPS reads an SPS NAL unit.
func ParseSPS(nalu []byte) (SPS, error) {
	var s SPS
	if len(nalu) < 4 || NALType(nalu) != NALSPS {
		return s, fmt.Errorf("h264: not an SPS (%d bytes)", len(nalu))
	}
	s.Profile, s.Compat, s.Level = nalu[1], nalu[2], nalu[3]
	s.ChromaFormat, s.BitDepthLuma, s.BitDepthChroma = 1, 8, 8
	r := &bitReader{b: unescape(nalu[4:])}
	r.ue() // seq_parameter_set_id
	separateColourPlane := false
	if HighProfile(s.Profile) {
		s.ChromaFormat = int(r.ue())
		if s.ChromaFormat == 3 {
			separateColourPlane = r.bit() == 1
		}
		s.BitDepthLuma = int(r.ue()) + 8
		s.BitDepthChroma = int(r.ue()) + 8
		r.bit()           // qpprime_y_zero_transform_bypass_flag
		if r.bit() == 1 { // seq_scaling_matrix_present_flag
			lists := 8
			if s.ChromaFormat == 3 {
				lists = 12
			}
			for i := 0; i < lists; i++ {
				if r.bit() == 1 {
					size := 16
					if i >= 6 {
						size = 64
					}
					r.skipScalingList(size)
				}
			}
		}
	}
	r.ue()          // log2_max_frame_num_minus4
	switch r.ue() { // pic_order_cnt_type
	case 0:
		r.ue() // log2_max_pic_order_cnt_lsb_minus4
	case 1:
		r.bit() // delta_pic_order_always_zero_flag
		r.se()  // offset_for_non_ref_pic
		r.se()  // offset_for_top_to_bottom_field
		n := r.ue()
		for i := uint32(0); i < n && r.err == nil; i++ {
			r.se()
		}
	}
	r.ue()  // max_num_ref_frames
	r.bit() // gaps_in_frame_num_value_allowed_flag
	widthMBs := int(r.ue()) + 1
	heightMapUnits := int(r.ue()) + 1
	frameMBsOnly := int(r.bit())
	if frameMBsOnly == 0 {
		r.bit() // mb_adaptive_frame_field_flag
	}
	r.bit() // direct_8x8_inference_flag
	var cropL, cropR, cropT, cropB int
	if r.bit() == 1 {
		cropL, cropR, cropT, cropB = int(r.ue()), int(r.ue()), int(r.ue()), int(r.ue())
	}
	if r.err != nil {
		return s, r.err
	}
	// Crop units (H.264 7-19 to 7-22).
	cropX, cropY := 1, 2-frameMBsOnly
	if !separateColourPlane && s.ChromaFormat != 0 {
		subW, subH := 2, 2
		switch s.ChromaFormat {
		case 2:
			subH = 1
		case 3:
			subW, subH = 1, 1
		}
		cropX, cropY = subW, subH*(2-frameMBsOnly)
	}
	s.Width = widthMBs*16 - cropX*(cropL+cropR)
	s.Height = (2-frameMBsOnly)*heightMapUnits*16 - cropY*(cropT+cropB)
	return s, nil
}

// unescape removes emulation-prevention bytes (00 00 03 → 00 00).
func unescape(b []byte) []byte {
	out := make([]byte, 0, len(b))
	zeros := 0
	for _, c := range b {
		if zeros >= 2 && c == 3 {
			zeros = 0
			continue
		}
		if c == 0 {
			zeros++
		} else {
			zeros = 0
		}
		out = append(out, c)
	}
	return out
}

// bitReader reads an RBSP MSB first. The first read past the end sets err;
// later reads return 0.
type bitReader struct {
	b   []byte
	pos int
	err error
}

func (r *bitReader) bit() uint32 {
	if r.pos >= len(r.b)*8 {
		r.err = errSPSShort
		return 0
	}
	v := r.b[r.pos/8] >> (7 - r.pos%8) & 1
	r.pos++
	return uint32(v)
}

// ue reads an unsigned Exp-Golomb value.
func (r *bitReader) ue() uint32 {
	zeros := 0
	for r.bit() == 0 {
		if r.err != nil || zeros >= 31 {
			r.err = errSPSShort
			return 0
		}
		zeros++
	}
	v := uint32(0)
	for i := 0; i < zeros; i++ {
		v = v<<1 | r.bit()
	}
	return (1<<zeros - 1) + v
}

// se reads a signed Exp-Golomb value.
func (r *bitReader) se() int32 {
	v := r.ue()
	if v&1 == 1 {
		return int32(v/2 + 1)
	}
	return -int32(v / 2)
}

func (r *bitReader) skipScalingList(size int) {
	last, next := int32(8), int32(8)
	for j := 0; j < size && r.err == nil; j++ {
		if next != 0 {
			next = (last + r.se() + 256) % 256
		}
		if next != 0 {
			last = next
		}
	}
}
