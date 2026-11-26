package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"strings"
	"time"

	"github.com/aetherix/aetherixcloud/internal/store"
)

// OAuthAccessPrefix marks opaque OAuth access tokens (axa_<grantID>.<secret>).
// Opaque tokens avoid clients trying to verify our HS256 JWTs without a JWKS.
const OAuthAccessPrefix = "axa_"

// Grant is an OAuth grant (one user authorising one MCP connector). Access
// tokens are opaque secrets referencing the grant; revoking the grant
// (tombstone) invalidates them on every node. Stored in oauth_grants with
// Kind "grant". Legacy JWT access tokens (typ=oauth) are still accepted until
// they expire.
type Grant struct {
	Kind       string    `json:"kind"` // "grant"
	ID         string    `json:"id"`
	ClientID   string    `json:"client_id"`
	ClientName string    `json:"client_name"`
	UserID     string    `json:"user_id"`
	Scopes     []string  `json:"scopes"`
	Resource   string    `json:"resource,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at,omitempty"`
	Revoked    bool      `json:"revoked,omitempty"`
	// AccessHash / AccessExpires are the current opaque access token.
	AccessHash    string    `json:"access_hash,omitempty"`
	AccessExpires time.Time `json:"access_expires,omitempty"`
	// RefreshHash is the SHA-256 of the current refresh secret; PrevRefresh
	// the one it replaced (presenting it again means theft: revoke).
	RefreshHash    string    `json:"refresh_hash"`
	PrevRefresh    string    `json:"prev_refresh,omitempty"`
	RefreshExpires time.Time `json:"refresh_expires"`
}

func hashSecret(s string) string {
	sum := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func randSecret(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// MintAccessToken sets a new opaque access token on g and returns the plaintext.
// Caller must PutGrant afterwards.
func MintAccessToken(g *Grant, ttl time.Duration) string {
	secret := randSecret(32)
	g.AccessHash = hashSecret(secret)
	g.AccessExpires = time.Now().UTC().Add(ttl)
	return OAuthAccessPrefix + g.ID + "." + secret
}

// SplitOAuthAccess parses an opaque access token.
func SplitOAuthAccess(t string) (grantID, secret string, ok bool) {
	if !strings.HasPrefix(t, OAuthAccessPrefix) {
		return "", "", false
	}
	grantID, secret, ok = strings.Cut(strings.TrimPrefix(t, OAuthAccessPrefix), ".")
	return grantID, secret, ok && grantID != "" && secret != ""
}

// AccessMatches reports whether plaintext matches the grant's current access token.
func AccessMatches(g *Grant, secret string) bool {
	if g == nil || g.AccessHash == "" || secret == "" || time.Now().After(g.AccessExpires) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(hashSecret(secret)), []byte(g.AccessHash)) == 1
}

// PutGrant stores g.
func PutGrant(ctx context.Context, s store.Store, g *Grant) error {
	g.Kind = "grant"
	return store.PutJSON(ctx, s, store.TableOAuthGrants, g.ID, map[string]string{"client_id": g.ClientID, "user_id": g.UserID}, g)
}

// GetGrant loads a grant (including revoked ones).
func GetGrant(ctx context.Context, s store.Store, id string) (*Grant, error) {
	g, err := store.GetJSON[Grant](ctx, s, store.TableOAuthGrants, id)
	if err != nil {
		return nil, err
	}
	if g.Kind != "grant" {
		return nil, store.ErrNotFound
	}
	return g, nil
}

// ListGrants returns active grants of a user ("" = all users).
func ListGrants(ctx context.Context, s store.Store, userID string) ([]*Grant, error) {
	q := store.Query{}
	if userID != "" {
		q.Where = map[string]string{"user_id": userID}
	}
	rows, err := store.ListJSON[Grant](ctx, s, store.TableOAuthGrants, q)
	if err != nil {
		return nil, err
	}
	var out []*Grant
	for i := range rows {
		if rows[i].Kind == "grant" && !rows[i].Revoked {
			out = append(out, &rows[i])
		}
	}
	return out, nil
}

// RevokeGrant tombstones a grant.
func RevokeGrant(ctx context.Context, s store.Store, id string) error {
	g, err := GetGrant(ctx, s, id)
	if err != nil {
		if store.IsNotFound(err) {
			return nil
		}
		return err
	}
	g.Revoked = true
	g.RefreshHash, g.PrevRefresh = "", ""
	g.AccessHash, g.AccessExpires = "", time.Time{}
	return PutGrant(ctx, s, g)
}
