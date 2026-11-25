package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/aetherix/aetherixcloud/internal/secrets"
)

// Claims are the JWT claims used for dashboard sessions and OAuth access
// tokens. Both are HS256 signed with a key derived from the cluster key, so a
// token minted on one node is valid on every node.
type Claims struct {
	Iss   string `json:"iss,omitempty"`
	Sub   string `json:"sub"`           // user ID
	Sid   string `json:"sid,omitempty"` // session ID or OAuth grant ID
	Typ   string `json:"typ"`           // KindSession | KindOAuth
	Scope string `json:"scope,omitempty"`
	Aud   string `json:"aud,omitempty"`
	Cid   string `json:"cid,omitempty"` // OAuth client ID
	Iat   int64  `json:"iat"`
	Exp   int64  `json:"exp"`
}

// Signer signs and verifies JWTs.
type Signer struct{ key []byte }

// NewSigner derives the JWT signing key from the base64 cluster key.
func NewSigner(clusterKey string) (*Signer, error) {
	k, err := secrets.ParseKey(clusterKey)
	if err != nil {
		return nil, err
	}
	m := hmac.New(sha256.New, k[:])
	m.Write([]byte("aetherix/jwt/v1"))
	return &Signer{key: m.Sum(nil)}, nil
}

var jwtHeader = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))

// ErrBadJWT is returned for malformed, forged or expired JWTs.
var ErrBadJWT = errors.New("auth: invalid jwt")

// Sign encodes c.
func (s *Signer) Sign(c Claims) string {
	b, _ := json.Marshal(c)
	msg := jwtHeader + "." + base64.RawURLEncoding.EncodeToString(b)
	return msg + "." + s.mac(msg)
}

func (s *Signer) mac(msg string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(msg))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// Parse verifies the signature and expiry.
func (s *Signer) Parse(tok string) (*Claims, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 || parts[0] != jwtHeader {
		return nil, ErrBadJWT
	}
	if !hmac.Equal([]byte(s.mac(parts[0]+"."+parts[1])), []byte(parts[2])) {
		return nil, ErrBadJWT
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, ErrBadJWT
	}
	c := &Claims{}
	if err := json.Unmarshal(raw, c); err != nil {
		return nil, ErrBadJWT
	}
	if c.Exp == 0 || time.Now().Unix() >= c.Exp {
		return nil, ErrBadJWT
	}
	return c, nil
}

// MAC returns a label-separated HMAC (derived from the same cluster key) used
// for stateless, node-independent tokens such as MCP confirmation tokens.
func (s *Signer) MAC(label, msg string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte("mac:" + label + "\x00" + msg))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}
