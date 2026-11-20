package fmp4_test

import (
	"bytes"
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/capture/fake"
	"github.com/devicelab-dev/maestro-runner/pkg/capture/fmp4"
)

var fixtureClip = fake.Clip()

func fixtureVideo(t *testing.T) fmp4.Video {
	v, _ := clip(t, fake.Clip())
	return v
}

var (
	aacAudio  = &fmp4.Audio{Codec: fmp4.CodecAAC, SampleRate: 48000, Channels: 2, ASC: []byte{0x11, 0x90}}
	lpcmAudio = &fmp4.Audio{Codec: fmp4.CodecLPCM, SampleRate: 48000, Channels: 2, BitsPerSample: 16, LittleEndian: true}
	fpcmAudio = &fmp4.Audio{Codec: fmp4.CodecLPCM, SampleRate: 48000, Channels: 2, BitsPerSample: 32, LittleEndian: true, Float: true}
)

// U3: the init segment's box tree for video only, video + AAC, video +
// ipcm and video + fpcm.
func TestInit_BoxTree(t *testing.T) {
	video := fixtureVideo(t)
	for _, tc := range []struct {
		name  string
		audio *fmp4.Audio
	}{{"video", nil}, {"aac", aacAudio}, {"lpcm", lpcmAudio}, {"fpcm", fpcmAudio}} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := fmp4.NewMuxer(video, tc.audio, 15)
			if err != nil {
				t.Fatal(err)
			}
			init := m.Init()
			top := parse(t, init, 0)
			if types(top) != "ftyp moov" {
				t.Fatalf("top level = %s", types(top))
			}
			if got := string(top[0].payload); got != "iso6\x00\x00\x00\x00iso6mp41" {
				t.Errorf("ftyp = %q", got)
			}
			tracks := 1
			wantMoov := "mvhd trak mvex"
			if tc.audio != nil {
				tracks, wantMoov = 2, "mvhd trak trak mvex"
			}
			moov := find(t, top, "moov", 0)
			if types(moov.children) != wantMoov {
				t.Fatalf("moov = %s, want %s", types(moov.children), wantMoov)
			}
			mvhd := find(t, top, "moov/mvhd", 0).payload
			if be32(mvhd, 12) != fmp4.MovieTimescale || be32(mvhd, len(mvhd)-4) != uint32(tracks+1) {
				t.Errorf("mvhd timescale %d next_track_ID %d", be32(mvhd, 12), be32(mvhd, len(mvhd)-4))
			}

			// Video track.
			vt := find(t, top, "moov/trak", 0)
			checkTrak(t, vt, "vmhd", fmp4.VideoTrackID, fmp4.VideoTimescale, "vide")
			tkhd := find(t, vt.children, "tkhd", 0).payload
			if be32(tkhd, len(tkhd)-8) != 640<<16 || be32(tkhd, len(tkhd)-4) != 360<<16 {
				t.Errorf("tkhd size = %x x %x", be32(tkhd, len(tkhd)-8), be32(tkhd, len(tkhd)-4))
			}
			avc3 := find(t, vt.children, "mdia/minf/stbl/stsd/avc3", 0)
			if w, h := int(avc3.payload[24])<<8|int(avc3.payload[25]), int(avc3.payload[26])<<8|int(avc3.payload[27]); w != 640 || h != 360 {
				t.Errorf("avc3 size %dx%d", w, h)
			}
			avcC := find(t, avc3.children, "avcC", 0).payload
			sps, pps := video.SPS[0], video.PPS[0]
			want := []byte{1, sps[1], sps[2], sps[3], 0xff, 0xe1, byte(len(sps) >> 8), byte(len(sps))}
			want = append(want, sps...)
			want = append(want, 1, byte(len(pps)>>8), byte(len(pps)))
			want = append(want, pps...)
			want = append(want, 0xfd, 0xf8, 0xf8, 0) // High: 4:2:0, 8-bit, no SPS extensions
			if !bytes.Equal(avcC, want) {
				t.Errorf("avcC = %x\nwant   %x", avcC, want)
			}

			// mvex: mehd version 1 with a zero duration, one trex per track.
			mvex := find(t, top, "moov/mvex", 0)
			wantMvex := "mehd trex"
			if tc.audio != nil {
				wantMvex = "mehd trex trex"
			}
			if types(mvex.children) != wantMvex {
				t.Fatalf("mvex = %s", types(mvex.children))
			}
			mehd := mvex.children[0]
			if mehd.payload[0] != 1 || be64(mehd.payload, 4) != 0 {
				t.Errorf("mehd = %x, want version 1, duration 0", mehd.payload)
			}
			if off := m.MehdOffset(); off != mehd.offset+12 || !bytes.Equal(init[off:off+8], make([]byte, 8)) {
				t.Errorf("MehdOffset = %d, mehd at %d", off, mehd.offset)
			}
			for i := 0; i < tracks; i++ {
				trex := mvex.children[1+i].payload
				if be32(trex, 4) != uint32(i+1) || be32(trex, 8) != 1 {
					t.Errorf("trex %d = %x", i, trex)
				}
			}
			if tc.audio == nil {
				if m.HasAudio() {
					t.Error("HasAudio on a video-only muxer")
				}
				return
			}

			// Audio track.
			if !m.HasAudio() {
				t.Error("HasAudio false")
			}
			at := find(t, top, "moov/trak", 1)
			checkTrak(t, at, "smhd", fmp4.AudioTrackID, 48000, "soun")
			stsd := find(t, at.children, "mdia/minf/stbl/stsd", 0)
			entry := stsd.children[0]
			wantBits := 16
			if tc.audio.Codec == fmp4.CodecLPCM {
				wantBits = tc.audio.BitsPerSample
			}
			if ch, bits, rate := int(entry.payload[16])<<8|int(entry.payload[17]), int(entry.payload[18])<<8|int(entry.payload[19]), be32(entry.payload, 24); ch != 2 || bits != wantBits || rate != 48000<<16 {
				t.Errorf("%s entry: %d ch, %d bits, rate %x", entry.typ, ch, bits, rate)
			}
			switch tc.audio.Codec {
			case fmp4.CodecAAC:
				if entry.typ != "mp4a" {
					t.Fatalf("audio entry %s, want mp4a", entry.typ)
				}
				esds := find(t, entry.children, "esds", 0).payload
				// ... DecoderSpecificInfo: tag 5, length 2, the ASC.
				if !bytes.Contains(esds, []byte{0x05, 0x02, 0x11, 0x90}) || !bytes.Contains(esds, []byte{0x04}) {
					t.Errorf("esds = %x does not carry the ASC", esds)
				}
			case fmp4.CodecLPCM:
				want := "ipcm"
				if tc.audio.Float {
					want = "fpcm"
				}
				if entry.typ != want {
					t.Fatalf("audio entry %s, want %s", entry.typ, want)
				}
				pcmC := find(t, entry.children, "pcmC", 0).payload
				if !bytes.Equal(pcmC, []byte{0, 0, 0, 0, 1, byte(tc.audio.BitsPerSample)}) {
					t.Errorf("pcmC = %x, want version 0, little-endian, %d bits", pcmC, tc.audio.BitsPerSample)
				}
			}
		})
	}
}

// checkTrak checks a trak's tree, track id, timescale and handler.
func checkTrak(t *testing.T, trak *node, mhd string, id uint32, timescale uint32, handler string) {
	t.Helper()
	if types(trak.children) != "tkhd mdia" {
		t.Fatalf("trak = %s", types(trak.children))
	}
	if got := be32(find(t, trak.children, "tkhd", 0).payload, 12); got != id {
		t.Errorf("tkhd track_ID = %d, want %d", got, id)
	}
	mdia := find(t, trak.children, "mdia", 0)
	if types(mdia.children) != "mdhd hdlr minf" {
		t.Fatalf("mdia = %s", types(mdia.children))
	}
	if got := be32(mdia.children[0].payload, 12); got != timescale {
		t.Errorf("mdhd timescale = %d, want %d", got, timescale)
	}
	if got := string(mdia.children[1].payload[8:12]); got != handler {
		t.Errorf("hdlr = %q, want %q", got, handler)
	}
	minf := mdia.children[2]
	if types(minf.children) != mhd+" dinf stbl" {
		t.Fatalf("minf = %s", types(minf.children))
	}
	if got := types(find(t, minf.children, "dinf/dref", 0).children); got != "url " {
		t.Errorf("dref = %s", got)
	}
	stbl := find(t, minf.children, "stbl", 0)
	if types(stbl.children) != "stsd stts stsc stsz stco" {
		t.Fatalf("stbl = %s", types(stbl.children))
	}
	for _, n := range stbl.children[1:] {
		if be32(n.payload, len(n.payload)-4) != 0 {
			t.Errorf("%s is not empty: %x", n.typ, n.payload)
		}
	}
}

func TestInit_BigEndianPCMAndLongASC(t *testing.T) {
	video := fixtureVideo(t)
	be := &fmp4.Audio{Codec: fmp4.CodecLPCM, SampleRate: 44100, Channels: 1, BitsPerSample: 24}
	m, err := fmp4.NewMuxer(video, be, 30)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(m.Init(), []byte("pcmC\x00\x00\x00\x00\x00\x18")) {
		t.Error("big-endian 24-bit pcmC not written")
	}
	// A 200-byte ASC needs a two-byte descriptor length (0x81 0x48).
	long := &fmp4.Audio{Codec: fmp4.CodecAAC, SampleRate: 48000, Channels: 2, ASC: append([]byte{0x11, 0x90}, make([]byte, 198)...)}
	m, err = fmp4.NewMuxer(video, long, 30)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(m.Init(), []byte{0x05, 0x81, 0x48, 0x11, 0x90}) {
		t.Error("long ASC descriptor length not written as two bytes")
	}
}

func TestInit_BaselineHasNoAVCCExtension(t *testing.T) {
	video := fixtureVideo(t)
	sps := append([]byte{0x67, 66, 0xc0, 0x1e}, video.SPS[0][4:]...)
	video.SPS = [][]byte{sps}
	m, err := fmp4.NewMuxer(video, nil, 15)
	if err != nil {
		t.Fatal(err)
	}
	avcC := find(t, find(t, parse(t, m.Init(), 0), "moov/trak", 0).children, "mdia/minf/stbl/stsd/avc3/avcC", 0).payload
	if avcC[len(avcC)-1] != video.PPS[0][len(video.PPS[0])-1] {
		t.Errorf("baseline avcC ends with %x, want the PPS (no extension)", avcC[len(avcC)-4:])
	}
}

func TestNewMuxer_Rejects(t *testing.T) {
	video := fixtureVideo(t)
	bad := func(mut func(*fmp4.Video)) fmp4.Video {
		v := video
		mut(&v)
		return v
	}
	for name, tc := range map[string]struct {
		video fmp4.Video
		audio *fmp4.Audio
		fps   int
	}{
		"no sps":       {bad(func(v *fmp4.Video) { v.SPS = nil }), nil, 15},
		"not an sps":   {bad(func(v *fmp4.Video) { v.SPS = v.PPS }), nil, 15},
		"no size":      {bad(func(v *fmp4.Video) { v.Width = 0 }), nil, 15},
		"too big":      {bad(func(v *fmp4.Video) { v.Height = 70000 }), nil, 15},
		"fps":          {video, nil, 0},
		"rate":         {video, &fmp4.Audio{Codec: fmp4.CodecAAC, Channels: 2, ASC: []byte{0x11, 0x90}}, 15},
		"codec":        {video, &fmp4.Audio{Codec: "opus", SampleRate: 48000, Channels: 2}, 15},
		"aac no asc":   {video, &fmp4.Audio{Codec: fmp4.CodecAAC, SampleRate: 48000, Channels: 2}, 15},
		"lpcm 12 bits": {video, &fmp4.Audio{Codec: fmp4.CodecLPCM, SampleRate: 48000, Channels: 2, BitsPerSample: 12}, 15},
		"fpcm 16 bits": {video, &fmp4.Audio{Codec: fmp4.CodecLPCM, SampleRate: 48000, Channels: 2, BitsPerSample: 16, Float: true}, 15},
		"no channels":  {video, &fmp4.Audio{Codec: fmp4.CodecLPCM, SampleRate: 48000, BitsPerSample: 16}, 15},
	} {
		if _, err := fmp4.NewMuxer(tc.video, tc.audio, tc.fps); err == nil {
			t.Errorf("%s: NewMuxer succeeded", name)
		}
	}
}
