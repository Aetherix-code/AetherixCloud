// Package secrets encrypts values at rest under the cluster key using NaCl
// secretbox (XSalsa20-Poly1305). The cluster key lives only in each node's
// local state file, never in the replicated store.
package secrets

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"

	"golang.org/x/crypto/nacl/secretbox"
)

// KeySize is the cluster key length.
const KeySize = 32

// Key is the cluster key.
type Key [KeySize]byte

// NewKey generates a random key.
func NewKey() (Key, error) {
	var k Key
	_, err := rand.Read(k[:])
	return k, err
}

// ParseKey decodes a base64 key.
func ParseKey(s string) (Key, error) {
	var k Key
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) != KeySize {
		return k, errors.New("secrets: invalid key")
	}
	copy(k[:], b)
	return k, nil
}

func (k Key) String() string { return base64.StdEncoding.EncodeToString(k[:]) }

// Seal returns "sb1:" + base64(nonce||box).
func (k Key) Seal(plain []byte) (string, error) {
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	box := secretbox.Seal(nonce[:], plain, &nonce, (*[32]byte)(&k))
	return "sb1:" + base64.StdEncoding.EncodeToString(box), nil
}

// Open reverses Seal.
func (k Key) Open(s string) ([]byte, error) {
	if len(s) < 4 || s[:4] != "sb1:" {
		return nil, errors.New("secrets: not a sealed value")
	}
	raw, err := base64.StdEncoding.DecodeString(s[4:])
	if err != nil || len(raw) < 24+secretbox.Overhead {
		return nil, errors.New("secrets: malformed")
	}
	var nonce [24]byte
	copy(nonce[:], raw[:24])
	out, ok := secretbox.Open(nil, raw[24:], &nonce, (*[32]byte)(&k))
	if !ok {
		return nil, fmt.Errorf("secrets: decryption failed")
	}
	return out, nil
}
