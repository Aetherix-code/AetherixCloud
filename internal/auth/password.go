package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters (OWASP minimum profile: 19 MiB, t=2, p=1).
const (
	argonMem     = 19 * 1024
	argonTime    = 2
	argonThreads = 1
	argonKeyLen  = 32
	// MinPasswordLen is enforced when setting passwords.
	MinPasswordLen = 8
)

// HashPassword returns an encoded argon2id hash.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	h := argon2.IDKey([]byte(pw), salt, argonTime, argonMem, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMem, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(h)), nil
}

// VerifyPassword checks pw against an encoded hash in constant time.
func VerifyPassword(pw, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var v, m, t, p int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &v); err != nil {
		return false
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[4])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[5])
	if err1 != nil || err2 != nil || len(want) == 0 || m <= 0 || t <= 0 || p <= 0 || m > 1<<20 {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, uint32(t), uint32(m), uint8(p), uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

var dummyHash, _ = HashPassword("not-a-real-password")

var errWeakPassword = errors.New("password must be at least 8 characters")
