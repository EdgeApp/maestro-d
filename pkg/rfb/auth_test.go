package rfb

import (
	"bufio"
	"bytes"
	"crypto/des"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math/big"
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

// accountParams is a server's account-login parameters: generator, key
// length, prime and server key, each keyLen bytes.
func accountParams(gen uint16, keyLen int, prime, pub *big.Int) *bufio.Reader {
	b := make([]byte, 4, 4+2*keyLen)
	binary.BigEndian.PutUint16(b[0:], gen)
	binary.BigEndian.PutUint16(b[2:], uint16(keyLen))
	b = append(b, leftPad(prime.Bytes(), keyLen)...)
	b = append(b, leftPad(pub.Bytes(), keyLen)...)
	return bufio.NewReader(bytes.NewReader(b))
}

func TestAccountLogin_RefusesBadParameters(t *testing.T) {
	p := big.NewInt(23)
	for name, r := range map[string]*bufio.Reader{
		"zero prime":      accountParams(2, 8, big.NewInt(0), big.NewInt(5)),
		"prime 2":         accountParams(2, 8, big.NewInt(2), big.NewInt(1)),
		"generator 0":     accountParams(0, 8, p, big.NewInt(5)),
		"generator 1":     accountParams(1, 8, p, big.NewInt(5)),
		"server key 0":    accountParams(2, 8, p, big.NewInt(0)),
		"server key 1":    accountParams(2, 8, p, big.NewInt(1)),
		"server key p-1":  accountParams(2, 8, p, big.NewInt(22)),
		"server key p":    accountParams(2, 8, p, p),
		"server key >= p": accountParams(2, 8, p, big.NewInt(30)),
	} {
		var out bytes.Buffer
		err := accountLogin(&out, r, "paul", "pw")
		if !errors.Is(err, ErrProtocol) {
			t.Errorf("%s: %v", name, err)
		}
		if out.Len() != 1 {
			t.Errorf("%s: sent %d bytes after choosing the type", name, out.Len())
		}
	}
	var out bytes.Buffer
	if err := accountLogin(&out, accountParams(2, 8, p, big.NewInt(21)), "paul", "pw"); err != nil {
		t.Errorf("valid parameters: %v", err)
	}
}
