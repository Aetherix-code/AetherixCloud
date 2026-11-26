package auth

import (
	"context"
	"strings"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
)

// Level is a capability level. Token scopes and RBAC roles both map to it; a
// request needs min(scope level, role level) >= the procedure's level.
type Level int

const (
	LevelNone Level = iota
	LevelRead
	LevelDeploy
	LevelAdmin
)

func (l Level) String() string {
	switch l {
	case LevelRead:
		return ScopeRead
	case LevelDeploy:
		return ScopeDeploy
	case LevelAdmin:
		return ScopeAdmin
	}
	return "none"
}

// ScopeLevel returns the highest level granted by scopes.
func ScopeLevel(scopes []string) Level {
	l := LevelNone
	for _, s := range scopes {
		switch s {
		case ScopeAdmin:
			l = max(l, LevelAdmin)
		case ScopeDeploy:
			l = max(l, LevelDeploy)
		case ScopeRead:
			l = max(l, LevelRead)
		}
	}
	return l
}

// RoleLevel maps an RBAC role to the highest level it may use.
//
//	viewer -> read, developer -> deploy, admin/owner -> admin
func RoleLevel(r v1.Role) Level {
	switch r {
	case v1.Role_ROLE_VIEWER:
		return LevelRead
	case v1.Role_ROLE_DEVELOPER:
		return LevelDeploy
	case v1.Role_ROLE_ADMIN, v1.Role_ROLE_OWNER:
		return LevelAdmin
	}
	return LevelNone
}

// ValidScopes reports whether every scope is known.
func ValidScopes(scopes []string) bool {
	for _, s := range scopes {
		if s != ScopeRead && s != ScopeDeploy && s != ScopeAdmin {
			return false
		}
	}
	return len(scopes) > 0
}

// Credential kinds.
const (
	KindSession  = "session"
	KindAPIToken = "api_token"
	KindOAuth    = "oauth"
	KindSystem   = "system" // trusted mesh/loopback callers
)

// Principal is the authenticated caller.
type Principal struct {
	UserID string
	Email  string
	Role   v1.Role
	Scopes []string
	Kind   string
	// CredentialID is the API token ID or OAuth grant ID ("" for sessions).
	CredentialID string
	// Client is a human-readable client name (OAuth client name, token name).
	Client string
	// Via is set per request by the interceptor ("dashboard", "api", "mcp").
	Via string
	// Bearer is the raw credential (never persisted or logged).
	Bearer string
	// Expires is when the credential ends (OAuth access tokens); zero if N/A.
	Expires time.Time
}

// MaxLevel is the highest level the principal may exercise.
func (p *Principal) MaxLevel() Level {
	if p.Kind == KindSystem {
		return LevelAdmin
	}
	return min(ScopeLevel(p.Scopes), RoleLevel(p.Role))
}

// Allows reports whether the principal may exercise l.
func (p *Principal) Allows(l Level) bool { return p.MaxLevel() >= l }

// IsOwner reports whether the principal has the owner role.
func (p *Principal) IsOwner() bool { return p.Kind == KindSystem || p.Role == v1.Role_ROLE_OWNER }

// RateKey identifies the credential for rate limiting.
func (p *Principal) RateKey() string {
	if p.CredentialID != "" {
		return p.Kind + ":" + p.CredentialID
	}
	return p.Kind + ":" + p.UserID
}

type ctxKey struct{}

// WithPrincipal stores p in ctx.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext returns the authenticated principal, or nil.
func FromContext(ctx context.Context) *Principal {
	p, _ := ctx.Value(ctxKey{}).(*Principal)
	return p
}

// BearerFromHeader extracts the token of an "Authorization: Bearer" header.
func BearerFromHeader(h string) string {
	f := strings.Fields(h)
	if len(f) == 2 && strings.EqualFold(f[0], "bearer") {
		return f[1]
	}
	return ""
}
