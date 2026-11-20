package fmp4

import "encoding/binary"

// IndexEntry is one random access point: a sample's decode time in its
// track's timescale and the file offset of the moof that holds it.
type IndexEntry struct {
	Time       uint64
	MoofOffset uint64
}

// TrackIndex lists one track's random access points.
type TrackIndex struct {
	Track   uint32
	Entries []IndexEntry
}

// MFRA builds the movie fragment random access box a finished recording
// ends with: one tfra per track, then mfro with mfra's size.
func MFRA(tracks []TrackIndex) []byte {
	var tfras []byte
	for _, t := range tracks {
		entries := make([]byte, 0, len(t.Entries)*19)
		for _, e := range t.Entries {
			entries = binary.BigEndian.AppendUint64(entries, e.Time)
			entries = binary.BigEndian.AppendUint64(entries, e.MoofOffset)
			// traf_number, trun_number, sample_number: 1-based, one byte each
			entries = append(entries, 1, 1, 1)
		}
		// Version 1: 64-bit time and moof_offset. The 32-bit field after the
		// track id holds the length_size_of_* fields, all 0 (one byte each).
		tfras = append(tfras, fullBox("tfra", 1, 0, u32(t.Track), u32(0), u32(uint32(len(t.Entries))), entries)...)
	}
	size := 8 + len(tfras) + 16
	mfro := fullBox("mfro", 0, 0, u32(uint32(size)))
	return box("mfra", tfras, mfro)
}
