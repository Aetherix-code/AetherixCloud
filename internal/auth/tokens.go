// Package auth: users, sessions, RBAC and API tokens.
//
// OWNERSHIP: the auth-api worker owns this package. Phase A only provides the
// minimal API-token primitives (Create/Verify) that the API server's bearer
// interceptor and `aetherixd init` (initial admin token) need. Users,
// sessions (JWT signed with the cluster key), OAuth and RBAC are TODO.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/ids"
	"github.com/aetherix/aetherixcloud/internal/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Scopes.
const (
	ScopeRead   = "read"
	ScopeDeploy = "deploy"
	ScopeAdmin  = "admin"
)

// TokenPrefix marks AetherixCloud API tokens.
const TokenPrefix = "axt_"

// ErrInvalidToken is returned for unknown/expired tokens.
var ErrInvalidToken = errors.New("auth: invalid token")

// HashToken returns the hex SHA-256 used to index tokens.
func HashToken(plain string) string {
	h := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(h[:])
}

// NewTokenRecord builds a new token and its plaintext (shown once).
func NewTokenRecord(name string, scopes []string, ttl time.Duration) (plain string, tok *v1.ApiToken, hash string) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	plain = TokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	tok = &v1.ApiToken{Id: ids.New(), Name: name, Scopes: scopes, CreatedAt: timestamppb.Now()}
	if ttl > 0 {
		tok.ExpiresAt = timestamppb.New(time.Now().Add(ttl))
	}
	return plain, tok, HashToken(plain)
}

// CreateToken stores a new token and returns the plaintext.
func CreateToken(ctx context.Context, s store.Store, name string, scopes []string, ttl time.Duration) (string, *v1.ApiToken, error) {
	plain, tok, hash := NewTokenRecord(name, scopes, ttl)
	err := store.PutProto(ctx, s, store.TableAPITokens, tok.Id,
		map[string]string{"user_id": tok.UserId, "token_hash": hash}, tok)
	return plain, tok, err
}

// Verify looks up a bearer token.
func Verify(ctx context.Context, s store.Store, plain string) (*v1.ApiToken, error) {
	if !strings.HasPrefix(plain, TokenPrefix) {
		return nil, ErrInvalidToken
	}
	hash := HashToken(plain)
	recs, err := s.List(ctx, store.TableAPITokens, store.Query{Where: map[string]string{"token_hash": hash}})
	if err != nil {
		return nil, err
	}
	for _, r := range recs {
		if subtle.ConstantTimeCompare([]byte(r.Idx["token_hash"]), []byte(hash)) != 1 {
			continue
		}
		tok := &v1.ApiToken{}
		if err := store.UnmarshalProto(r, tok); err != nil {
			return nil, err
		}
		if tok.ExpiresAt != nil && time.Now().After(tok.ExpiresAt.AsTime()) {
			return nil, ErrInvalidToken
		}
		return tok, nil
	}
	return nil, ErrInvalidToken
}

// HasScope reports whether the token grants scope (admin implies all).
func HasScope(tok *v1.ApiToken, scope string) bool {
	for _, s := range tok.Scopes {
		if s == scope || s == ScopeAdmin {
			return true
		}
		if scope == ScopeRead && s == ScopeDeploy {
			return true
		}
	}
	return false
}
