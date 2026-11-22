package rfb

import (
	"bufio"
	"crypto/aes"
	"crypto/des"
	"crypto/md5"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"math/big"
)

// credentialField is the size of each half of the account login's
// credentials block: a NUL-terminated UTF-8 string filled with random bytes.
const credentialField = 64

// accountLogin answers Apple's security type 30: a Diffie-Hellman exchange
// whose shared secret, hashed with MD5, is the AES-128 key for the user name
// and password. This is the login Apple's own Screen Sharing app uses, and
// the one that lands on the user's desktop.
func accountLogin(w io.Writer, r *bufio.Reader, user, password string) error {
	if len(user) >= credentialField || len(password) >= credentialField {
		return fmt.Errorf("the macOS user name and password must each be at most %d bytes", credentialField-1)
	}
	if _, err := w.Write([]byte{SecurityAccount}); err != nil {
		return err
	}
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return fmt.Errorf("read account login parameters: %w", err)
	}
	gen := binary.BigEndian.Uint16(hdr[0:])
	keyLen := int(binary.BigEndian.Uint16(hdr[2:]))
	if keyLen == 0 || keyLen > 1024 {
		return fmt.Errorf("%w: account login key length %d", ErrProtocol, keyLen)
	}
	params := make([]byte, 2*keyLen)
	if _, err := io.ReadFull(r, params); err != nil {
		return fmt.Errorf("read account login keys: %w", err)
	}
	prime := new(big.Int).SetBytes(params[:keyLen])
	serverPub := new(big.Int).SetBytes(params[keyLen:])
	if err := checkDH(gen, prime, serverPub); err != nil {
		return err
	}

	priv, err := rand.Int(rand.Reader, prime)
	if err != nil {
		return err
	}
	pub := new(big.Int).Exp(big.NewInt(int64(gen)), priv, prime)
	secret := new(big.Int).Exp(serverPub, priv, prime)
	key := md5.Sum(leftPad(secret.Bytes(), keyLen))

	creds := make([]byte, 2*credentialField)
	if _, err := rand.Read(creds); err != nil {
		return err
	}
	putCString(creds[:credentialField], user)
	putCString(creds[credentialField:], password)
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return err
	}
	out := make([]byte, len(creds))
	for i := 0; i < len(creds); i += aes.BlockSize { // ECB, as the server expects
		block.Encrypt(out[i:i+aes.BlockSize], creds[i:i+aes.BlockSize])
	}
	_, err = w.Write(append(out, leftPad(pub.Bytes(), keyLen)...))
	return err
}

// checkDH refuses parameters that would fail the exchange or make its
// secret predictable: a prime below 3, a generator below 2, or a server key
// outside 2…p−2.
func checkDH(gen uint16, prime, serverPub *big.Int) error {
	switch {
	case prime.Cmp(big.NewInt(3)) < 0:
		return fmt.Errorf("%w: account login prime %v", ErrProtocol, prime)
	case gen < 2:
		return fmt.Errorf("%w: account login generator %d", ErrProtocol, gen)
	case serverPub.Cmp(big.NewInt(2)) < 0 || serverPub.Cmp(new(big.Int).Sub(prime, big.NewInt(2))) > 0:
		return fmt.Errorf("%w: account login server key out of range", ErrProtocol)
	}
	return nil
}

// vncLogin answers security type 2: the 16-byte challenge encrypted with DES
// under the password's first 8 bytes, each bit-reversed (the VNC convention).
func vncLogin(w io.Writer, r *bufio.Reader, password string) error {
	if _, err := w.Write([]byte{SecurityVNC}); err != nil {
		return err
	}
	var challenge [16]byte
	if _, err := io.ReadFull(r, challenge[:]); err != nil {
		return fmt.Errorf("read VNC challenge: %w", err)
	}
	resp, err := vncResponse(password, challenge[:])
	if err != nil {
		return err
	}
	_, err = w.Write(resp)
	return err
}

func vncResponse(password string, challenge []byte) ([]byte, error) {
	block, err := des.NewCipher(vncKey(password))
	if err != nil {
		return nil, err
	}
	out := make([]byte, 16)
	block.Encrypt(out[:8], challenge[:8])
	block.Encrypt(out[8:], challenge[8:16])
	return out, nil
}

func vncKey(password string) []byte {
	key := make([]byte, 8)
	copy(key, password)
	for i, b := range key {
		var rev byte
		for j := 0; j < 8; j++ {
			rev = rev<<1 | b&1
			b >>= 1
		}
		key[i] = rev
	}
	return key
}

func leftPad(b []byte, n int) []byte {
	if len(b) >= n {
		return b[len(b)-n:]
	}
	out := make([]byte, n)
	copy(out[n-len(b):], b)
	return out
}

func putCString(dst []byte, s string) {
	n := copy(dst, s)
	dst[n] = 0
}
