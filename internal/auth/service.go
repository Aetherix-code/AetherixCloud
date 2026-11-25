package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/gen/aetherix/v1/aetherixv1connect"
	"github.com/aetherix/aetherixcloud/internal/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Service implements AuthService. Authorization of each RPC is done by the
// Interceptor; handlers additionally scope data to the caller where needed.
type Service struct {
	aetherixv1connect.UnimplementedAuthServiceHandler
	Store store.Store
	Auth  *Authenticator
	// Trusted is true on the mesh/loopback listener: calls without a
	// principal are treated as the system (owner).
	Trusted bool

	mu    sync.Mutex
	fails map[string][]time.Time
}

// NewService creates the AuthService handler.
func NewService(a *Authenticator, trusted bool) *Service {
	return &Service{Store: a.Store, Auth: a, Trusted: trusted, fails: map[string][]time.Time{}}
}

var systemPrincipal = &Principal{Kind: KindSystem, Role: v1.Role_ROLE_OWNER, Scopes: []string{ScopeAdmin}, Via: "mesh"}

func (s *Service) caller(ctx context.Context) (*Principal, error) {
	if p := FromContext(ctx); p != nil {
		return p, nil
	}
	if s.Trusted {
		return systemPrincipal, nil
	}
	return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("authentication required"))
}

func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrUserExists):
		return connect.NewError(connect.CodeAlreadyExists, err)
	case errors.Is(err, ErrUserNotFound), store.IsNotFound(err):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, ErrBadLogin):
		return connect.NewError(connect.CodeUnauthenticated, err)
	case errors.Is(err, ErrLastOwner), errors.Is(err, errWeakPassword):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	}
	if _, ok := err.(*connect.Error); ok {
		return err
	}
	return connect.NewError(connect.CodeInvalidArgument, err)
}

const (
	maxFails   = 5
	failWindow = 5 * time.Minute
)

func (s *Service) throttled(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	cut := time.Now().Add(-failWindow)
	l := s.fails[key][:0]
	for _, t := range s.fails[key] {
		if t.After(cut) {
			l = append(l, t)
		}
	}
	s.fails[key] = l
	return len(l) >= maxFails
}

func (s *Service) fail(key string) {
	s.mu.Lock()
	s.fails[key] = append(s.fails[key], time.Now())
	s.mu.Unlock()
}

func (s *Service) auditLogin(ctx context.Context, email, action string, u *v1.User, err error) {
	e := &v1.AuditEntry{ActorEmail: email, Via: "dashboard", Action: action, Ok: err == nil}
	if u != nil {
		e.ActorUserId = u.Id
	}
	if err != nil {
		e.Error = err.Error()
	}
	_ = WriteAudit(ctx, s.Store, e)
}

func (s *Service) login(ctx context.Context, u *v1.User) (*v1.LoginResponse, error) {
	tok, exp, err := NewSession(ctx, s.Store, s.Auth.Signer, u.Id)
	if err != nil {
		return nil, mapErr(err)
	}
	return &v1.LoginResponse{SessionToken: tok, User: u, ExpiresUnix: exp.Unix()}, nil
}

// Login exchanges email/password for a session JWT.
func (s *Service) Login(ctx context.Context, req *connect.Request[v1.LoginRequest]) (*connect.Response[v1.LoginResponse], error) {
	key := strings.ToLower(strings.TrimSpace(req.Msg.Email))
	if s.throttled(key) {
		return nil, connect.NewError(connect.CodeResourceExhausted, errors.New("too many failed logins; try again later"))
	}
	u, err := CheckPassword(ctx, s.Store, req.Msg.Email, req.Msg.Password)
	if err != nil {
		if errors.Is(err, ErrBadLogin) {
			s.fail(key)
		}
		s.auditLogin(ctx, key, "auth.login", nil, err)
		return nil, mapErr(err)
	}
	resp, err := s.login(ctx, u)
	if err != nil {
		return nil, err
	}
	s.auditLogin(ctx, key, "auth.login", u, nil)
	return connect.NewResponse(resp), nil
}

// Logout revokes the session used for the call.
func (s *Service) Logout(ctx context.Context, _ *connect.Request[v1.LogoutRequest]) (*connect.Response[v1.LogoutResponse], error) {
	p, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	if p.Kind == KindSession {
		if c, err := s.Auth.Signer.Parse(p.Bearer); err == nil {
			_ = RevokeSession(ctx, s.Store, c.Sid)
		}
	}
	return connect.NewResponse(&v1.LogoutResponse{}), nil
}

// Me returns the caller.
func (s *Service) Me(ctx context.Context, _ *connect.Request[v1.MeRequest]) (*connect.Response[v1.MeResponse], error) {
	p, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	u := &v1.User{Id: p.UserID, Email: p.Email, Role: p.Role, Name: p.Client}
	if p.UserID != "" {
		if full, err := GetUser(ctx, s.Store, p.UserID); err == nil {
			u = full
		}
	} else {
		u.Name = "cluster admin token"
	}
	return connect.NewResponse(&v1.MeResponse{User: u, Scopes: p.Scopes}), nil
}

func (s *Service) requireOwnerFor(p *Principal, roles ...v1.Role) error {
	for _, r := range roles {
		if r == v1.Role_ROLE_OWNER && !p.IsOwner() {
			return connect.NewError(connect.CodePermissionDenied, errors.New("only an owner can grant, change or remove the owner role"))
		}
	}
	return nil
}

// CreateUser adds a user.
func (s *Service) CreateUser(ctx context.Context, req *connect.Request[v1.CreateUserRequest]) (*connect.Response[v1.CreateUserResponse], error) {
	p, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.requireOwnerFor(p, req.Msg.Role); err != nil {
		return nil, err
	}
	u, err := CreateUser(ctx, s.Store, req.Msg.Email, req.Msg.Name, req.Msg.Password, req.Msg.Role)
	if err != nil {
		return nil, mapErr(err)
	}
	return connect.NewResponse(&v1.CreateUserResponse{User: u}), nil
}

// ListUsers lists users.
func (s *Service) ListUsers(ctx context.Context, _ *connect.Request[v1.ListUsersRequest]) (*connect.Response[v1.ListUsersResponse], error) {
	us, err := ListUsers(ctx, s.Store)
	if err != nil {
		return nil, mapErr(err)
	}
	return connect.NewResponse(&v1.ListUsersResponse{Users: us}), nil
}

// UpdateUser changes a user's name, role or password.
func (s *Service) UpdateUser(ctx context.Context, req *connect.Request[v1.UpdateUserRequest]) (*connect.Response[v1.UpdateUserResponse], error) {
	p, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	cur, err := GetUser(ctx, s.Store, req.Msg.UserId)
	if err != nil {
		return nil, mapErr(err)
	}
	if err := s.requireOwnerFor(p, cur.Role); err != nil {
		return nil, err
	}
	if req.Msg.Role != nil {
		if err := s.requireOwnerFor(p, *req.Msg.Role); err != nil {
			return nil, err
		}
	}
	u, err := UpdateUser(ctx, s.Store, req.Msg.UserId, req.Msg.Name, req.Msg.Role, req.Msg.Password)
	if err != nil {
		return nil, mapErr(err)
	}
	return connect.NewResponse(&v1.UpdateUserResponse{User: u}), nil
}

// DeleteUser removes a user.
func (s *Service) DeleteUser(ctx context.Context, req *connect.Request[v1.DeleteUserRequest]) (*connect.Response[v1.DeleteUserResponse], error) {
	p, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	cur, err := GetUser(ctx, s.Store, req.Msg.UserId)
	if err != nil {
		return nil, mapErr(err)
	}
	if err := s.requireOwnerFor(p, cur.Role); err != nil {
		return nil, err
	}
	if err := DeleteUser(ctx, s.Store, req.Msg.UserId); err != nil {
		return nil, mapErr(err)
	}
	return connect.NewResponse(&v1.DeleteUserResponse{}), nil
}

// CreateApiToken mints a token owned by the caller, never more powerful than
// the caller's own credential.
func (s *Service) CreateApiToken(ctx context.Context, req *connect.Request[v1.CreateApiTokenRequest]) (*connect.Response[v1.CreateApiTokenResponse], error) {
	p, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Msg.Name) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name is required"))
	}
	if !ValidScopes(req.Msg.Scopes) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New(`scopes must be a non-empty subset of "read", "deploy", "admin"`))
	}
	if ScopeLevel(req.Msg.Scopes) > p.MaxLevel() {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("cannot create a token with more access than your own credential"))
	}
	var ttl time.Duration
	if req.Msg.TtlSeconds > 0 {
		ttl = time.Duration(req.Msg.TtlSeconds) * time.Second
	}
	plain, tok, err := CreateUserToken(ctx, s.Store, p.UserID, strings.TrimSpace(req.Msg.Name), req.Msg.Scopes, ttl)
	if err != nil {
		return nil, mapErr(err)
	}
	return connect.NewResponse(&v1.CreateApiTokenResponse{Token: tok, Plaintext: plain}), nil
}

// ListApiTokens lists own tokens (admins: all).
func (s *Service) ListApiTokens(ctx context.Context, _ *connect.Request[v1.ListApiTokensRequest]) (*connect.Response[v1.ListApiTokensResponse], error) {
	p, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	toks, err := store.ListProto[v1.ApiToken](ctx, s.Store, store.TableAPITokens, store.Query{})
	if err != nil {
		return nil, mapErr(err)
	}
	var out []*v1.ApiToken
	for _, t := range toks {
		if p.Allows(LevelAdmin) || (p.UserID != "" && t.UserId == p.UserID) {
			out = append(out, t)
		}
	}
	return connect.NewResponse(&v1.ListApiTokensResponse{Tokens: out}), nil
}

// RevokeApiToken deletes a token.
func (s *Service) RevokeApiToken(ctx context.Context, req *connect.Request[v1.RevokeApiTokenRequest]) (*connect.Response[v1.RevokeApiTokenResponse], error) {
	p, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	t, err := store.GetProto[v1.ApiToken](ctx, s.Store, store.TableAPITokens, req.Msg.TokenId)
	if err != nil {
		return nil, mapErr(err)
	}
	if !p.Allows(LevelAdmin) && (p.UserID == "" || t.UserId != p.UserID) {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("not your token"))
	}
	if err := s.Store.Delete(ctx, store.TableAPITokens, t.Id); err != nil {
		return nil, mapErr(err)
	}
	return connect.NewResponse(&v1.RevokeApiTokenResponse{}), nil
}

// GetSetupStatus tells the dashboard whether to show the first-run screen.
func (s *Service) GetSetupStatus(ctx context.Context, _ *connect.Request[v1.GetSetupStatusRequest]) (*connect.Response[v1.GetSetupStatusResponse], error) {
	has, err := HasUsers(ctx, s.Store)
	if err != nil {
		return nil, mapErr(err)
	}
	return connect.NewResponse(&v1.GetSetupStatusResponse{NeedsSetup: !has}), nil
}

// Bootstrap creates the first owner; it needs the admin token minted at
// cluster init, so a stranger reaching a fresh node cannot claim it.
func (s *Service) Bootstrap(ctx context.Context, req *connect.Request[v1.BootstrapRequest]) (*connect.Response[v1.LoginResponse], error) {
	if s.throttled("bootstrap") {
		return nil, connect.NewError(connect.CodeResourceExhausted, errors.New("too many attempts"))
	}
	tok, err := Verify(ctx, s.Store, req.Msg.AdminToken)
	if err != nil || !HasScope(tok, ScopeAdmin) {
		s.fail("bootstrap")
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("a valid admin API token is required"))
	}
	if has, _ := HasUsers(ctx, s.Store); has {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("already set up"))
	}
	u, err := CreateUser(ctx, s.Store, req.Msg.Email, req.Msg.Name, req.Msg.Password, v1.Role_ROLE_OWNER)
	if err != nil {
		return nil, mapErr(err)
	}
	resp, err := s.login(ctx, u)
	if err != nil {
		return nil, err
	}
	s.auditLogin(ctx, u.Email, "auth.bootstrap", u, nil)
	return connect.NewResponse(resp), nil
}

// ListAuditLog returns the audit trail.
func (s *Service) ListAuditLog(ctx context.Context, req *connect.Request[v1.ListAuditLogRequest]) (*connect.Response[v1.ListAuditLogResponse], error) {
	es, err := ListAudit(ctx, s.Store, req.Msg.ActorUserId, req.Msg.ActionPrefix, int(req.Msg.Limit))
	if err != nil {
		return nil, mapErr(err)
	}
	return connect.NewResponse(&v1.ListAuditLogResponse{Entries: es}), nil
}

// ListOAuthGrants lists active connector grants (own; admins: all).
func (s *Service) ListOAuthGrants(ctx context.Context, _ *connect.Request[v1.ListOAuthGrantsRequest]) (*connect.Response[v1.ListOAuthGrantsResponse], error) {
	p, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	uid := p.UserID
	if p.Allows(LevelAdmin) {
		uid = ""
	}
	gs, err := ListGrants(ctx, s.Store, uid)
	if err != nil {
		return nil, mapErr(err)
	}
	out := make([]*v1.OAuthGrant, 0, len(gs))
	for _, g := range gs {
		og := &v1.OAuthGrant{Id: g.ID, ClientId: g.ClientID, ClientName: g.ClientName, UserId: g.UserID, Scopes: g.Scopes, CreatedAt: timestamppb.New(g.CreatedAt)}
		if !g.LastUsedAt.IsZero() {
			og.LastUsedAt = timestamppb.New(g.LastUsedAt)
		}
		out = append(out, og)
	}
	return connect.NewResponse(&v1.ListOAuthGrantsResponse{Grants: out}), nil
}

// RevokeOAuthGrant revokes a grant everywhere.
func (s *Service) RevokeOAuthGrant(ctx context.Context, req *connect.Request[v1.RevokeOAuthGrantRequest]) (*connect.Response[v1.RevokeOAuthGrantResponse], error) {
	p, err := s.caller(ctx)
	if err != nil {
		return nil, err
	}
	g, err := GetGrant(ctx, s.Store, req.Msg.GrantId)
	if err != nil {
		return nil, mapErr(err)
	}
	if !p.Allows(LevelAdmin) && (p.UserID == "" || g.UserID != p.UserID) {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("not your grant"))
	}
	if err := RevokeGrant(ctx, s.Store, g.ID); err != nil {
		return nil, mapErr(err)
	}
	_ = WriteAudit(ctx, s.Store, &v1.AuditEntry{ActorUserId: p.UserID, ActorEmail: p.Email, Via: p.Via, Client: g.ClientName,
		Action: "oauth.revoke", Ok: true, Attributes: map[string]string{"grant_id": g.ID}})
	return connect.NewResponse(&v1.RevokeOAuthGrantResponse{}), nil
}

// EnsureFirstOwner creates the first owner when no user exists (used with
// AX_ADMIN_EMAIL / AX_ADMIN_PASSWORD for unattended installs). It is a no-op
// otherwise.
func EnsureFirstOwner(ctx context.Context, s store.Store, email, password string) (bool, error) {
	if has, err := HasUsers(ctx, s); err != nil || has {
		return false, err
	}
	_, err := CreateUser(ctx, s, email, "", password, v1.Role_ROLE_OWNER)
	return err == nil, err
}
