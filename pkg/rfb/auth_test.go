package rfb

import (
	"bytes"
	"crypto/des"
	"encoding/hex"
	"testing"
)

func TestVNCKey_BitReversal(t *testing.T) {
	// 'p' 0x70 = 01110000 → 00001110 = 0x0e; padding NULs stay 0.
	got := vncKey("pw")
	want := []byte{0x0e, 0xee, 0, 0, 0, 0, 0, 0}
	if !bytes.Equal(got, want) {
		t.Errorf("% x", got)
	}
	if got := vncKey("123456789"); len(got) != 8 || got[7] != 0x1c { // '8' 0x38 → 0x1c; '9' dropped
		t.Errorf("long password key % x", got)
	}
}

func TestVNCResponse_Vector(t *testing.T) {
	// FIPS 46 vector: key 133457799BBCDFF1, block 0123456789ABCDEF →
	// 85E813540F0AB405. vncResponse bit-reverses the password's bytes first,
	// so pass the reversal of that key.
	key, _ := hex.DecodeString("133457799BBCDFF1")
	pw := make([]byte, 8)
	for i, b := range key {
		var r byte
		for j := 0; j < 8; j++ {
			r = r<<1 | b&1
			b >>= 1
		}
		pw[i] = r
	}
	block, _ := hex.DecodeString("0123456789ABCDEF")
	resp, err := vncResponse(string(pw), append(append([]byte{}, block...), block...))
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(resp[:8]); got != "85e813540f0ab405" {
		t.Errorf("first half %s", got)
	}
	if !bytes.Equal(resp[:8], resp[8:]) {
		t.Error("halves differ for equal input")
	}
	// Cross-check against crypto/des directly with the reversed key.
	c, _ := des.NewCipher(vncKey(string(pw)))
	direct := make([]byte, 8)
	c.Encrypt(direct, block)
	if !bytes.Equal(direct, resp[:8]) {
		t.Error("vncResponse disagrees with crypto/des")
	}
}

func TestLeftPad(t *testing.T) {
	if got := leftPad([]byte{1, 2}, 4); !bytes.Equal(got, []byte{0, 0, 1, 2}) {
		t.Errorf("% x", got)
	}
	if got := leftPad([]byte{1, 2, 3}, 2); !bytes.Equal(got, []byte{2, 3}) {
		t.Errorf("% x", got)
	}
}

func TestPutCString(t *testing.T) {
	b := []byte{9, 9, 9, 9}
	putCString(b, "ab")
	if !bytes.Equal(b, []byte{'a', 'b', 0, 9}) {
		t.Errorf("% x", b)
	}
}
