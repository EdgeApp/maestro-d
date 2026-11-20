package h264_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/capture/fake"
	"github.com/devicelab-dev/maestro-runner/pkg/capture/h264"
)

// U2: Annex-B with 3- and 4-byte start codes splits into NAL units with
// emulation-prevention bytes untouched, and round-trips through AVCC.
func TestAnnexB_AVCC_RoundTrip(t *testing.T) {
	sps := []byte{0x67, 0x64, 0x00, 0x28, 0xac}
	slice := []byte{0x65, 0x88, 0x00, 0x00, 0x03, 0x01, 0x42} // 00 00 03 is emulation prevention
	pps := []byte{0x68, 0xee, 0x3c, 0x80}
	stream := []byte{0xff} // junk before the first start code is ignored
	stream = append(stream, 0, 0, 0, 1)
	stream = append(stream, sps...)
	stream = append(stream, 0, 0, 1)
	stream = append(stream, pps...)
	stream = append(stream, 0, 0, 0, 0, 1) // trailing_zero_8bits then a 4-byte code
	stream = append(stream, slice...)
	stream = append(stream, 0, 0) // trailing zeros at the end

	nalus := h264.SplitAnnexB(stream)
	want := [][]byte{sps, pps, slice}
	if len(nalus) != len(want) {
		t.Fatalf("got %d NAL units, want %d: %x", len(nalus), len(want), nalus)
	}
	for i := range want {
		if !bytes.Equal(nalus[i], want[i]) {
			t.Errorf("NAL %d = %x, want %x", i, nalus[i], want[i])
		}
	}

	avcc := h264.AnnexBToAVCC(stream)
	wantAVCC := []byte{0, 0, 0, 5}
	wantAVCC = append(wantAVCC, sps...)
	wantAVCC = append(wantAVCC, 0, 0, 0, 4)
	wantAVCC = append(wantAVCC, pps...)
	wantAVCC = append(wantAVCC, 0, 0, 0, 7)
	wantAVCC = append(wantAVCC, slice...)
	if !bytes.Equal(avcc, wantAVCC) {
		t.Fatalf("AVCC = %x, want %x", avcc, wantAVCC)
	}
	back, err := h264.AVCCToAnnexB(avcc)
	if err != nil {
		t.Fatal(err)
	}
	if again := h264.AnnexBToAVCC(back); !bytes.Equal(again, avcc) {
		t.Errorf("round trip = %x, want %x", again, avcc)
	}
	if !bytes.HasPrefix(back, []byte{0, 0, 0, 1, 0x67}) {
		t.Errorf("Annex-B = %x, want 4-byte start codes", back)
	}

	if got := h264.SplitAnnexB([]byte{1, 2, 3}); got != nil {
		t.Errorf("no start code: got %x", got)
	}
	if got := h264.SplitAnnexB([]byte{0, 0, 1, 0, 0, 0, 1, 0x09}); len(got) != 1 || got[0][0] != 0x09 {
		t.Errorf("empty NAL between start codes: got %x", got)
	}
}

func TestSplitAVCC_Truncated(t *testing.T) {
	for _, b := range [][]byte{{0, 0, 1}, {0, 0, 0, 9, 1, 2}} {
		if _, err := h264.SplitAVCC(b); !errors.Is(err, h264.ErrTruncated) {
			t.Errorf("SplitAVCC(%x) err = %v, want ErrTruncated", b, err)
		}
		if _, err := h264.AVCCToAnnexB(b); err == nil {
			t.Errorf("AVCCToAnnexB(%x) succeeded", b)
		}
	}
}

// U2: SPS/PPS extraction, profile/level bytes and the codec string.
func TestParameterSets_ProfileLevel_CodecString(t *testing.T) {
	nalus := h264.SplitAnnexB(fake.Clip())
	sps, pps := h264.ParameterSets(nalus)
	if len(sps) != 3 || len(pps) != 3 { // x264 repeats them before each of the 3 IDRs
		t.Fatalf("got %d SPS, %d PPS; want 3 each", len(sps), len(pps))
	}
	if h264.NALType(sps[0]) != h264.NALSPS || h264.NALType(pps[0]) != h264.NALPPS || h264.NALType(nil) != 0 {
		t.Fatal("NALType mismatch")
	}
	p, c, l, err := h264.ProfileLevel(sps[0])
	if err != nil || p != 100 || c != 0 {
		t.Fatalf("ProfileLevel = %d %d %d %v, want High (100), constraints 0", p, c, l, err)
	}
	if !h264.HasIDR(nalus) || h264.HasIDR([][]byte{{0x41}}) {
		t.Error("HasIDR mismatch")
	}

	// A High 4.0 SPS: profile 0x64, constraints 0x00, level 0x28.
	high40 := append([]byte{0x67, 0x64, 0x00, 0x28}, sps[0][4:]...)
	if s, err := h264.CodecString(high40); err != nil || s != "avc3.640028" {
		t.Errorf("CodecString = %q, %v; want avc3.640028", s, err)
	}
	if _, err := h264.CodecString(pps[0]); err == nil {
		t.Error("CodecString of a PPS succeeded")
	}
	if _, _, _, err := h264.ProfileLevel([]byte{0x67, 0x64}); err == nil {
		t.Error("ProfileLevel of a short SPS succeeded")
	}
}

func TestAccessUnits(t *testing.T) {
	aus := h264.AccessUnits(h264.SplitAnnexB(fake.Clip()))
	if len(aus) != 45 {
		t.Fatalf("clip has %d access units, want 45", len(aus))
	}
	// Without delimiters: a slice with first_mb_in_slice 0 (top bit set)
	// starts a new picture, one with a later macroblock continues it, and a
	// parameter set after a slice starts the next.
	idr0 := []byte{0x65, 0x88}
	idr1 := []byte{0x65, 0x40} // first_mb_in_slice 1
	p0 := []byte{0x41, 0x9a}
	sps := []byte{0x67, 0x42, 0, 0x1e}
	aus = h264.AccessUnits([][]byte{idr0, idr1, p0, sps, idr0})
	if len(aus) != 3 || len(aus[0]) != 2 || len(aus[1]) != 1 || len(aus[2]) != 2 {
		t.Fatalf("access units = %x", aus)
	}
}

// bits builds an RBSP for synthetic SPS tests.
type bits struct {
	b []byte
	n int
}

func (w *bits) bit(v uint) {
	if w.n%8 == 0 {
		w.b = append(w.b, 0)
	}
	if v != 0 {
		w.b[len(w.b)-1] |= 0x80 >> (w.n % 8)
	}
	w.n++
}

func (w *bits) ue(v uint) {
	v++
	n := 0
	for x := v; x > 1; x >>= 1 {
		n++
	}
	for i := 0; i < n; i++ {
		w.bit(0)
	}
	for i := n; i >= 0; i-- {
		w.bit(v >> i & 1)
	}
}

func (w *bits) se(v int) {
	if v > 0 {
		w.ue(uint(2*v - 1))
	} else {
		w.ue(uint(-2 * v))
	}
}

func (w *bits) sps(profile byte) []byte {
	w.bit(1) // rbsp_stop_one_bit
	return append([]byte{0x67, profile, 0, 0x1f}, w.b...)
}

func TestParseSPS_Fixtures(t *testing.T) {
	for _, tc := range []struct {
		clip []byte
		w, h int
	}{{fake.Clip(), 640, 360}, {fake.RotatedClip(), 360, 640}} {
		sps, _ := h264.ParameterSets(h264.SplitAnnexB(tc.clip))
		s, err := h264.ParseSPS(sps[0])
		if err != nil {
			t.Fatal(err)
		}
		if s.Width != tc.w || s.Height != tc.h || s.ChromaFormat != 1 || s.BitDepthLuma != 8 || s.BitDepthChroma != 8 || s.Profile != 100 {
			t.Errorf("SPS = %+v, want %dx%d 4:2:0 8-bit High", s, tc.w, tc.h)
		}
	}
}

func TestParseSPS_Synthetic(t *testing.T) {
	// High 4:4:4 with a scaling matrix, POC type 1, 1920x1080 cropped.
	var w bits
	w.ue(0)  // sps id
	w.ue(3)  // chroma_format_idc 4:4:4
	w.bit(0) // separate_colour_plane_flag
	w.ue(2)  // bit_depth_luma_minus8
	w.ue(2)  // bit_depth_chroma_minus8
	w.bit(0) // qpprime
	w.bit(1) // seq_scaling_matrix_present_flag
	for i := 0; i < 12; i++ {
		if i == 0 || i == 6 {
			w.bit(1)
			w.se(-8) // next becomes 0 on the first delta: the rest of the list is implied
		} else {
			w.bit(0)
		}
	}
	w.ue(0)   // log2_max_frame_num_minus4
	w.ue(1)   // pic_order_cnt_type 1
	w.bit(0)  // delta_pic_order_always_zero_flag
	w.se(1)   // offset_for_non_ref_pic
	w.se(-1)  // offset_for_top_to_bottom_field
	w.ue(2)   // num_ref_frames_in_pic_order_cnt_cycle
	w.se(3)   //
	w.se(-3)  //
	w.ue(4)   // max_num_ref_frames
	w.bit(0)  // gaps
	w.ue(119) // pic_width_in_mbs_minus1: 1920
	w.ue(67)  // pic_height_in_map_units_minus1: 1088
	w.bit(1)  // frame_mbs_only_flag
	w.bit(1)  // direct_8x8
	w.bit(1)  // frame_cropping_flag
	w.ue(0)   // left
	w.ue(0)   // right
	w.ue(0)   // top
	w.ue(8)   // bottom: 8 rows at 4:4:4 (crop unit 1)
	s, err := h264.ParseSPS(w.sps(244))
	if err != nil {
		t.Fatal(err)
	}
	if s.Width != 1920 || s.Height != 1080 || s.ChromaFormat != 3 || s.BitDepthLuma != 10 || s.BitDepthChroma != 10 {
		t.Errorf("SPS = %+v, want 1920x1080 4:4:4 10-bit", s)
	}

	// Baseline, POC type 2, interlaced (frame_mbs_only 0), 4:2:0 crop.
	var b bits
	b.ue(0)  // sps id
	b.ue(0)  // log2_max_frame_num_minus4
	b.ue(2)  // pic_order_cnt_type 2
	b.ue(1)  // max_num_ref_frames
	b.bit(0) // gaps
	b.ue(44) // 720
	b.ue(17) // 18 map units: 576 lines
	b.bit(0) // frame_mbs_only_flag 0
	b.bit(0) // mb_adaptive_frame_field_flag
	b.bit(1) // direct_8x8
	b.bit(1) // cropping
	b.ue(2)  // left: 4 px
	b.ue(2)  // right: 4 px
	b.ue(1)  // top: 4 lines (unit 2*2)
	b.ue(1)  // bottom: 4 lines
	s, err = h264.ParseSPS(b.sps(66))
	if err != nil {
		t.Fatal(err)
	}
	if s.Width != 712 || s.Height != 568 || s.ChromaFormat != 1 || s.BitDepthLuma != 8 {
		t.Errorf("SPS = %+v, want 712x568 4:2:0 8-bit", s)
	}

	// High 4:2:2 (crop unit 2x1), POC type 0.
	var c bits
	c.ue(0)  // sps id
	c.ue(2)  // 4:2:2
	c.ue(0)  // luma 8
	c.ue(0)  // chroma 8
	c.bit(0) // qpprime
	c.bit(0) // no scaling matrix
	c.ue(0)  // log2_max_frame_num_minus4
	c.ue(0)  // poc type 0
	c.ue(0)  // log2_max_poc_lsb_minus4
	c.ue(1)  // refs
	c.bit(0) // gaps
	c.ue(39) // 640
	c.ue(22) // 368
	c.bit(1) // frame_mbs_only
	c.bit(1) // direct
	c.bit(1) // cropping
	c.ue(0)  //
	c.ue(0)  //
	c.ue(0)  //
	c.ue(8)  // 8 lines at subH 1
	s, err = h264.ParseSPS(c.sps(122))
	if err != nil {
		t.Fatal(err)
	}
	if s.Width != 640 || s.Height != 360 || s.ChromaFormat != 2 {
		t.Errorf("SPS = %+v, want 640x360 4:2:2", s)
	}
}

func TestParseSPS_Errors(t *testing.T) {
	if _, err := h264.ParseSPS([]byte{0x68, 1, 2, 3}); err == nil {
		t.Error("a PPS parsed as an SPS")
	}
	if _, err := h264.ParseSPS([]byte{0x67, 0x64, 0, 0x1f, 0x80}); err == nil {
		t.Error("a truncated SPS parsed")
	}
	if _, err := h264.ParseSPS([]byte{0x67, 0x42, 0, 0x1f, 0, 0, 0, 0, 0}); err == nil {
		t.Error("an SPS of zeros parsed")
	}
	// Emulation prevention: 00 00 03 inside the RBSP is removed before
	// parsing, so an SPS whose width field spans it still reads correctly.
	var b bits
	b.ue(0)
	b.ue(0)
	b.ue(2)
	b.ue(1)
	b.bit(0)
	b.ue(39)
	b.ue(22)
	b.bit(1)
	b.bit(1)
	b.bit(0)
	raw := b.sps(66)
	withEP := append(append([]byte{}, raw...), 0, 0, 3, 0) // trailing bytes after the stop bit
	if _, err := h264.ParseSPS(withEP); err != nil {
		t.Errorf("SPS with an emulation-prevention byte: %v", err)
	}
}
