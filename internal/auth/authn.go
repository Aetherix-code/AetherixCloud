package auth

import (
	"context"
	"strings"
	"sync"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Authenticator turns bearer credentials (API tokens, session JWTs, OAuth
// access tokens) into Principals. It is stateless apart from the store, so
// every node authenticates identically.
type Authenticator struct {
	Store  store.Store
	Signer *Signer

	mu       sync.Mutex
	lastUsed map[string]time.Time
}

// NewAuthenticator builds an Authenticator from the base64 cluster key.
func NewAuthenticator(st store.Store, clusterKey string) (*Authenticator, error) {
	sg, err := NewSigner(clusterKey)
	if err != nil {
		return nil, err
	}
	return &Authenticator{Store: st, Signer: sg, lastUsed: map[string]time.Time{}}, nil
}

// Authenticate verifies a bearer credential.
func (a *Authenticator) Authenticate(ctx context.Context, bearer string) (*Principal, error) {
	switch {
	case strings.HasPrefix(bearer, TokenPrefix):
		return a.apiToken(ctx, bearer)
	case strings.Count(bearer, ".") == 2:
		return a.jwt(ctx, bearer)
	}
	return nil, ErrInvalidToken
}

func (a *Authenticator) apiToken(ctx context.Context, bearer string) (*Principal, error) {
	tok, err := Verify(ctx, a.Store, bearer)
	if err != nil {
		return nil, err
	}
	p := &Principal{Kind: KindAPIToken, CredentialID: tok.Id, Client: tok.Name, Scopes: tok.Scopes, Bearer: bearer, UserID: tok.UserId}
	if tok.UserId == "" {
		// Tokens minted by `ax machine init` / cluster bootstrap belong to the
		// cluster itself and act as owner (limited by their scopes).
		p.Role = v1.Role_ROLE_OWNER
	} else {
		u, err := GetUser(ctx, a.Store, tok.UserId)
		if err != nil {
			return nil, ErrInvalidToken
		}
		p.Role, p.Email = u.Role, u.Email
	}
	a.touchToken(tok)
	return p, nil
}

func (a *Authenticator) touchToken(tok *v1.ApiToken) {
	a.mu.Lock()
	t, ok := a.lastUsed[tok.Id]
	if ok && time.Since(t) < time.Minute {
		a.mu.Unlock()
		return
	}
	a.lastUsed[tok.Id] = time.Now()
	a.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cur, err := store.GetProto[v1.ApiToken](ctx, a.Store, store.TableAPITokens, tok.Id)
		if err != nil {
			return
		}
		cur.LastUsedAt = timestamppb.Now()
		hash := ""
		if r, err := a.Store.Get(ctx, store.TableAPITokens, tok.Id); err == nil {
			hash = r.Idx["token_hash"]
		}
		_ = store.PutProto(ctx, a.Store, store.TableAPITokens, cur.Id, map[string]string{"user_id": cur.UserId, "token_hash": hash}, cur)
	}()
}

func (a *Authenticator) jwt(ctx context.Context, bearer string) (*Principal, error) {
	c, err := a.Signer.Parse(bearer)
	if err != nil {
		return nil, ErrInvalidToken
	}
	u, err := GetUser(ctx, a.Store, c.Sub)
	if err != nil {
		return nil, ErrInvalidToken
	}
	p := &Principal{UserID: u.Id, Email: u.Email, Role: u.Role, Scopes: strings.Fields(c.Scope), Bearer: bearer, Kind: c.Typ}
	switch c.Typ {
	case KindSession:
		x, err := store.GetJSON[Session](ctx, a.Store, store.TableSessions, c.Sid)
		if err == nil && x.Revoked {
			return nil, ErrInvalidToken
		}
	case KindOAuth:
		p.CredentialID = c.Sid
		g, err := GetGrant(ctx, a.Store, c.Sid)
		if err == nil {
			if g.Revoked {
				return nil, ErrInvalidToken
			}
			p.Client = g.ClientName
			a.touchGrant(g)
		}
	default:
		return nil, ErrInvalidToken
	}
	return p, nil
}

func (a *Authenticator) touchGrant(g *Grant) {
	a.mu.Lock()
	t, ok := a.lastUsed[g.ID]
	if ok && time.Since(t) < time.Minute {
		a.mu.Unlock()
		return
	}
	a.lastUsed[g.ID] = time.Now()
	a.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cur, err := GetGrant(ctx, a.Store, g.ID)
		if err != nil || cur.Revoked {
			return
		}
		cur.LastUsedAt = time.Now().UTC()
		_ = PutGrant(ctx, a.Store, cur)
	}()
}

// CreateUserToken stores a new API token owned by userID.
func CreateUserToken(ctx context.Context, s store.Store, userID, name string, scopes []string, ttl time.Duration) (string, *v1.ApiToken, error) {
	plain, tok, hash := NewTokenRecord(name, scopes, ttl)
	tok.UserId = userID
	err := store.PutProto(ctx, s, store.TableAPITokens, tok.Id, map[string]string{"user_id": userID, "token_hash": hash}, tok)
	return plain, tok, err
}
