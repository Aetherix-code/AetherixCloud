package auth

import (
	"context"
	"time"

	"github.com/aetherix/aetherixcloud/internal/store"
)

// Grant is an OAuth grant (one user authorising one MCP connector). Access
// tokens are JWTs referencing the grant; revoking the grant (tombstone)
// invalidates them on every node. Stored in oauth_grants with Kind "grant".
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
	// RefreshHash is the SHA-256 of the current refresh secret; PrevRefresh
	// the one it replaced (presenting it again means theft: revoke).
	RefreshHash    string    `json:"refresh_hash"`
	PrevRefresh    string    `json:"prev_refresh,omitempty"`
	RefreshExpires time.Time `json:"refresh_expires"`
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
	return PutGrant(ctx, s, g)
}
