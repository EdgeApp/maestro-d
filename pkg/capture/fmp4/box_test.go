package fmp4_test

import (
	"encoding/binary"
	"strings"
	"testing"
)

// node is a parsed box: its type, its payload and its children for the
// container types the tests descend into.
type node struct {
	typ      string
	offset   int // of the box header in the parsed buffer
	payload  []byte
	children []*node
}

// sampleEntryHeader is how many payload bytes precede a sample entry's
// child boxes.
var sampleEntryHeader = map[string]int{"avc3": 78, "mp4a": 28, "ipcm": 28, "fpcm": 28}

var containers = map[string]int{
	"moov": 0, "trak": 0, "mdia": 0, "minf": 0, "dinf": 0, "stbl": 0, "mvex": 0,
	"moof": 0, "traf": 0, "mfra": 0,
	"stsd": 8, "dref": 8, // full box + entry_count
}

// parse splits b into boxes, failing the test when sizes do not add up.
func parse(t *testing.T, b []byte, base int) []*node {
	t.Helper()
	var out []*node
	for off := 0; off < len(b); {
		if len(b)-off < 8 {
			t.Fatalf("trailing %d bytes at %d", len(b)-off, base+off)
		}
		size := int(binary.BigEndian.Uint32(b[off:]))
		if size < 8 || off+size > len(b) {
			t.Fatalf("box at %d: size %d overruns %d bytes", base+off, size, len(b)-off)
		}
		n := &node{typ: string(b[off+4 : off+8]), offset: base + off, payload: b[off+8 : off+size]}
		skip, ok := containers[n.typ]
		if h, entry := sampleEntryHeader[n.typ]; entry {
			skip, ok = h, true
		}
		if ok {
			n.children = parse(t, n.payload[skip:], base+off+8+skip)
		}
		out = append(out, n)
		off += size
	}
	return out
}

func types(ns []*node) string {
	s := make([]string, len(ns))
	for i, n := range ns {
		s[i] = n.typ
	}
	return strings.Join(s, " ")
}

// find follows a path of box types ("moov/trak/mdia"); index picks among
// siblings of the last type.
func find(t *testing.T, ns []*node, path string, index int) *node {
	t.Helper()
	parts := strings.Split(path, "/")
	for i, p := range parts {
		var match []*node
		for _, n := range ns {
			if n.typ == p {
				match = append(match, n)
			}
		}
		pick := 0
		if i == len(parts)-1 {
			pick = index
		}
		if pick >= len(match) {
			t.Fatalf("no %s[%d] (have %s)", path, index, types(ns))
		}
		if i == len(parts)-1 {
			return match[pick]
		}
		ns = match[pick].children
	}
	return nil
}

func be32(b []byte, off int) uint32 { return binary.BigEndian.Uint32(b[off:]) }
func be64(b []byte, off int) uint64 { return binary.BigEndian.Uint64(b[off:]) }
