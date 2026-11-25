// Package ids generates sortable unique IDs (lowercase ULIDs).
package ids

import (
	"crypto/rand"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// New returns a new lowercase ULID.
func New() string {
	return strings.ToLower(ulid.MustNew(ulid.Timestamp(time.Now()), rand.Reader).String())
}

// Short returns n random lowercase base32 characters (for hostnames etc.).
func Short(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz234567"
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}
