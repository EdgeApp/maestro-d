package fmp4_test

import (
	"testing"

	"github.com/devicelab-dev/maestro-runner/pkg/capture/fmp4"
)

func TestMFRA(t *testing.T) {
	b := fmp4.MFRA([]fmp4.TrackIndex{
		{Track: 1, Entries: []fmp4.IndexEntry{{Time: 0, MoofOffset: 900}, {Time: 90000, MoofOffset: 1 << 33}}},
		{Track: 2},
	})
	top := parse(t, b, 0)
	if types(top) != "mfra" || types(top[0].children) != "tfra tfra mfro" {
		t.Fatalf("mfra = %s / %s", types(top), types(top[0].children))
	}
	v := top[0].children[0].payload
	if v[0] != 1 || be32(v, 4) != 1 || be32(v, 8) != 0 || be32(v, 12) != 2 {
		t.Fatalf("tfra header = %x", v[:16])
	}
	if be64(v, 16) != 0 || be64(v, 24) != 900 || be64(v, 35) != 90000 || be64(v, 43) != 1<<33 || v[32] != 1 || v[33] != 1 || v[34] != 1 {
		t.Errorf("tfra entries = %x", v[16:])
	}
	if a := top[0].children[1].payload; be32(a, 4) != 2 || be32(a, 12) != 0 {
		t.Errorf("audio tfra = %x", a)
	}
	if mfro := top[0].children[2].payload; be32(mfro, 4) != uint32(len(b)) {
		t.Errorf("mfro size %d, mfra is %d bytes", be32(mfro, 4), len(b))
	}
}
