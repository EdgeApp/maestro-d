package fmp4_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/devicelab-dev/maestro-runner/pkg/capture/fmp4"
	"github.com/devicelab-dev/maestro-runner/pkg/capture/h264"
)

// fragInfo is what one fragment says about its sample.
type fragInfo struct {
	seq, track     uint32
	tfdt           uint64
	duration, size uint32
	flags          uint32
	data           []byte
}

func readFragment(t *testing.T, b []byte) fragInfo {
	t.Helper()
	top := parse(t, b, 0)
	if types(top) != "moof mdat" {
		t.Fatalf("fragment = %s", types(top))
	}
	moof := top[0]
	if types(moof.children) != "mfhd traf" {
		t.Fatalf("moof = %s", types(moof.children))
	}
	traf := moof.children[1]
	if types(traf.children) != "tfhd tfdt trun" {
		t.Fatalf("traf = %s", types(traf.children))
	}
	tfhd, tfdt, trun := traf.children[0].payload, traf.children[1].payload, traf.children[2].payload
	if be32(tfhd, 0) != 0x020000 {
		t.Errorf("tfhd flags = %x, want default-base-is-moof", be32(tfhd, 0))
	}
	if tfdt[0] != 1 {
		t.Errorf("tfdt version %d, want 1", tfdt[0])
	}
	if be32(trun, 0) != 0x000701 || be32(trun, 4) != 1 {
		t.Errorf("trun flags/count = %x %d", be32(trun, 0), be32(trun, 4))
	}
	off := int(be32(trun, 8))
	size := be32(trun, 16)
	if off+int(size) != len(b) || off != top[1].offset+8 {
		t.Errorf("trun data offset %d size %d, mdat payload at %d of %d", off, size, top[1].offset+8, len(b))
	}
	return fragInfo{
		seq:      be32(moof.children[0].payload, 4),
		track:    be32(tfhd, 4),
		tfdt:     be64(tfdt, 4),
		duration: be32(trun, 12),
		size:     size,
		flags:    be32(trun, 20),
		data:     b[off:],
	}
}

// U4: one moof+mdat per sample; mfhd counts up; tfdt is rebased to 0; the
// trun data offset points at the sample; sync and non-sync flags; the first
// video sample lasts 1/fps; later ones the previous interval, clamped.
func TestFragment_Video(t *testing.T) {
	video, frames := clip(t, fixtureClip)
	m, err := fmp4.NewMuxer(video, nil, 15)
	if err != nil {
		t.Fatal(err)
	}
	start := 7 * time.Second
	pts := []time.Duration{start, start + time.Second/15, start + time.Second/15 + 100*time.Microsecond, start + 5*time.Second}
	var got []fragInfo
	for i, p := range pts {
		f, err := m.Video(p, frames[i].Key, frames[i].Data)
		if err != nil {
			t.Fatal(err)
		}
		info := readFragment(t, f.Bytes)
		if f.Track != fmp4.VideoTrackID || f.Time != info.tfdt || f.Duration != info.duration || f.Key != frames[i].Key {
			t.Errorf("Fragment %d = %+v, box says %+v", i, f, info)
		}
		got = append(got, info)
	}
	want := []struct {
		tfdt     uint64
		duration uint32
		flags    uint32
	}{
		{0, 6000, fmp4.FlagsSync},          // first: 1/15 s
		{6000, 6000, fmp4.FlagsNonSync},    // the interval before it
		{6009, 90, fmp4.FlagsNonSync},      // 0.1 ms clamped to 1 ms
		{450000, 90000, fmp4.FlagsNonSync}, // ~4.93 s clamped to 1 s
	}
	for i, w := range want {
		g := got[i]
		if g.seq != uint32(i+1) || g.track != 1 || g.tfdt != w.tfdt || g.duration != w.duration || g.flags != w.flags {
			t.Errorf("fragment %d = seq %d track %d tfdt %d dur %d flags %x; want seq %d tfdt %d dur %d flags %x",
				i, g.seq, g.track, g.tfdt, g.duration, g.flags, i+1, w.tfdt, w.duration, w.flags)
		}
	}
	// The keyframe carries the SPS and PPS in band, ahead of its own NAL
	// units; the delta frame is written as it came.
	nalus, err := h264.SplitAVCC(got[0].data)
	if err != nil {
		t.Fatal(err)
	}
	if h264.NALType(nalus[0]) != h264.NALSPS || h264.NALType(nalus[1]) != h264.NALPPS || !bytes.HasSuffix(got[0].data, frames[0].Data) {
		t.Errorf("keyframe sample starts with types %d %d", h264.NALType(nalus[0]), h264.NALType(nalus[1]))
	}
	if !bytes.Equal(got[1].data, frames[1].Data) {
		t.Error("delta frame sample changed")
	}
	if d := m.Duration(); d != 6000 {
		t.Errorf("Duration = %d ms", d)
	}
}

func TestFragment_MonotonicAndInBandSPS(t *testing.T) {
	video, frames := clip(t, fixtureClip)
	m, err := fmp4.NewMuxer(video, nil, 30)
	if err != nil {
		t.Fatal(err)
	}
	f1, _ := m.Video(time.Second, true, frames[0].Data)
	f2, _ := m.Video(time.Second, false, frames[1].Data)                  // the same PTS
	f3, _ := m.Video(time.Second-time.Millisecond, false, frames[2].Data) // earlier
	if f1.Time != 0 || f1.Duration != 3000 || f2.Time != 1 || f3.Time != 2 {
		t.Errorf("times %d/%d %d %d, want 0/3000 1 2", f1.Time, f1.Duration, f2.Time, f3.Time)
	}
	// A keyframe that already carries an SPS gets no second copy.
	withSPS := h264.JoinAVCC([][]byte{video.SPS[0], video.PPS[0], {0x65, 0x88, 0x80}})
	f4, _ := m.Video(2*time.Second, true, withSPS)
	if !bytes.Equal(readFragment(t, f4.Bytes).data, withSPS) {
		t.Error("parameter sets inserted twice")
	}
	// SetParameterSets changes what later keyframes carry; empty sets are
	// ignored.
	newSPS := append([]byte{0x67, 0x64, 0x00, 0x1f}, video.SPS[0][4:]...)
	m.SetParameterSets(nil, nil)
	m.SetParameterSets([][]byte{newSPS}, video.PPS)
	f5, _ := m.Video(3*time.Second, true, frames[0].Data)
	if nalus, _ := h264.SplitAVCC(readFragment(t, f5.Bytes).data); !bytes.Equal(nalus[0], newSPS) {
		t.Error("new SPS not in band")
	}
	if _, err := m.Video(4*time.Second, false, []byte{0, 0, 0, 9, 1}); err == nil {
		t.Error("a truncated AVCC sample was muxed")
	}
	if _, err := m.Audio(time.Second, []byte{1}); err == nil {
		t.Error("audio on a video-only muxer was muxed")
	}
}

// U4: audio fragments rebase to the stream's first sample (either track);
// the first AAC sample lasts 1024 ticks, the first PCM sample its frame
// count; audio is always sync.
func TestFragment_Audio(t *testing.T) {
	video, frames := clip(t, fixtureClip)

	m, err := fmp4.NewMuxer(video, aacAudio, 15)
	if err != nil {
		t.Fatal(err)
	}
	a1, _ := m.Audio(10*time.Second, []byte{1, 2, 3})
	v1, _ := m.Video(10*time.Second+50*time.Millisecond, true, frames[0].Data)
	a2, _ := m.Audio(10*time.Second+1024*time.Second/48000, []byte{4, 5})
	i1, i2, iv := readFragment(t, a1.Bytes), readFragment(t, a2.Bytes), readFragment(t, v1.Bytes)
	if i1.track != 2 || i1.tfdt != 0 || i1.duration != 1024 || i1.flags != fmp4.FlagsSync || !bytes.Equal(i1.data, []byte{1, 2, 3}) {
		t.Errorf("first AAC fragment = %+v", i1)
	}
	if i2.tfdt != 1024 || i2.duration != 1024 || i2.seq != 3 {
		t.Errorf("second AAC fragment = %+v", i2)
	}
	if iv.tfdt != 4500 || iv.seq != 2 { // 50 ms at 90 kHz after the audio's start
		t.Errorf("video fragment = %+v", iv)
	}
	if a1.Key != true || a1.Track != fmp4.AudioTrackID {
		t.Errorf("audio Fragment = %+v", a1)
	}
	if d := m.Duration(); d != 116 { // the video's end, 10500 ticks at 90 kHz
		t.Errorf("Duration = %d ms, want 116", d)
	}

	m, err = fmp4.NewMuxer(video, lpcmAudio, 15)
	if err != nil {
		t.Fatal(err)
	}
	p1, _ := m.Audio(0, make([]byte, 3840)) // 960 frames of stereo s16
	if info := readFragment(t, p1.Bytes); info.duration != 960 || info.size != 3840 {
		t.Errorf("first PCM fragment = %+v", info)
	}
}
