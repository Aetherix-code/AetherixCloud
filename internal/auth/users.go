package auth

import (
	"context"
	"errors"
	"net/mail"
	"strings"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/ids"
	"github.com/aetherix/aetherixcloud/internal/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Errors returned by the user helpers.
var (
	ErrUserExists   = errors.New("auth: user already exists")
	ErrUserNotFound = errors.New("auth: user not found")
	ErrBadLogin     = errors.New("auth: invalid email or password")
	ErrLastOwner    = errors.New("auth: cannot remove or demote the last owner")
)

// userRow is the stored form (JSON) of a user; the password hash never leaves
// this package.
type userRow struct {
	ID           string    `json:"id"`
	Email        string    `json:"email"`
	Name         string    `json:"name"`
	Role         int32     `json:"role"`
	PasswordHash string    `json:"password_hash"`
	CreatedAt    time.Time `json:"created_at"`
}

func (r *userRow) proto() *v1.User {
	return &v1.User{Id: r.ID, Email: r.Email, Name: r.Name, Role: v1.Role(r.Role), CreatedAt: timestamppb.New(r.CreatedAt)}
}

// NormalizeEmail lowercases and validates an address.
func NormalizeEmail(e string) (string, error) {
	e = strings.ToLower(strings.TrimSpace(e))
	if _, err := mail.ParseAddress(e); err != nil || strings.ContainsAny(e, " <>") {
		return "", errors.New("invalid email address")
	}
	return e, nil
}

func putUser(ctx context.Context, s store.Store, r *userRow) error {
	return store.PutJSON(ctx, s, store.TableUsers, r.ID, map[string]string{"email": r.Email}, r)
}

func getUserRow(ctx context.Context, s store.Store, id string) (*userRow, error) {
	r, err := store.GetJSON[userRow](ctx, s, store.TableUsers, id)
	if store.IsNotFound(err) {
		return nil, ErrUserNotFound
	}
	return r, err
}

func getUserByEmail(ctx context.Context, s store.Store, email string) (*userRow, error) {
	rows, err := store.ListJSON[userRow](ctx, s, store.TableUsers, store.Query{Where: map[string]string{"email": email}})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, ErrUserNotFound
	}
	return &rows[0], nil
}

// CreateUser adds a user. role must be a concrete role.
func CreateUser(ctx context.Context, s store.Store, email, name, password string, role v1.Role) (*v1.User, error) {
	email, err := NormalizeEmail(email)
	if err != nil {
		return nil, err
	}
	if len(password) < MinPasswordLen {
		return nil, errWeakPassword
	}
	if role == v1.Role_ROLE_UNSPECIFIED {
		role = v1.Role_ROLE_DEVELOPER
	}
	if _, err := getUserByEmail(ctx, s, email); err == nil {
		return nil, ErrUserExists
	}
	h, err := HashPassword(password)
	if err != nil {
		return nil, err
	}
	if name == "" {
		name = strings.SplitN(email, "@", 2)[0]
	}
	r := &userRow{ID: ids.New(), Email: email, Name: name, Role: int32(role), PasswordHash: h, CreatedAt: time.Now().UTC()}
	if err := putUser(ctx, s, r); err != nil {
		return nil, err
	}
	return r.proto(), nil
}

// GetUser loads a user by ID.
func GetUser(ctx context.Context, s store.Store, id string) (*v1.User, error) {
	r, err := getUserRow(ctx, s, id)
	if err != nil {
		return nil, err
	}
	return r.proto(), nil
}

// ListUsers returns all users ordered by creation.
func ListUsers(ctx context.Context, s store.Store) ([]*v1.User, error) {
	rows, err := store.ListJSON[userRow](ctx, s, store.TableUsers, store.Query{})
	if err != nil {
		return nil, err
	}
	out := make([]*v1.User, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].proto())
	}
	return out, nil
}

// HasUsers reports whether any user exists.
func HasUsers(ctx context.Context, s store.Store) (bool, error) {
	rows, err := s.List(ctx, store.TableUsers, store.Query{Limit: 1})
	return len(rows) > 0, err
}

func countOwners(ctx context.Context, s store.Store, except string) (int, error) {
	rows, err := store.ListJSON[userRow](ctx, s, store.TableUsers, store.Query{})
	n := 0
	for _, r := range rows {
		if r.ID != except && v1.Role(r.Role) == v1.Role_ROLE_OWNER {
			n++
		}
	}
	return n, err
}

// UpdateUser applies the non-nil changes.
func UpdateUser(ctx context.Context, s store.Store, id string, name *string, role *v1.Role, password *string) (*v1.User, error) {
	r, err := getUserRow(ctx, s, id)
	if err != nil {
		return nil, err
	}
	if name != nil {
		r.Name = *name
	}
	if role != nil && *role != v1.Role(r.Role) {
		if v1.Role(r.Role) == v1.Role_ROLE_OWNER {
			if n, _ := countOwners(ctx, s, id); n == 0 {
				return nil, ErrLastOwner
			}
		}
		r.Role = int32(*role)
	}
	if password != nil {
		if len(*password) < MinPasswordLen {
			return nil, errWeakPassword
		}
		if r.PasswordHash, err = HashPassword(*password); err != nil {
			return nil, err
		}
	}
	if err := putUser(ctx, s, r); err != nil {
		return nil, err
	}
	return r.proto(), nil
}

// DeleteUser removes a user, their API tokens and sessions.
func DeleteUser(ctx context.Context, s store.Store, id string) error {
	r, err := getUserRow(ctx, s, id)
	if err != nil {
		return err
	}
	if v1.Role(r.Role) == v1.Role_ROLE_OWNER {
		if n, _ := countOwners(ctx, s, id); n == 0 {
			return ErrLastOwner
		}
	}
	toks, _ := s.List(ctx, store.TableAPITokens, store.Query{Where: map[string]string{"user_id": id}})
	for _, t := range toks {
		_ = s.Delete(ctx, store.TableAPITokens, t.ID)
	}
	grants, _ := ListGrants(ctx, s, id)
	for _, g := range grants {
		_ = RevokeGrant(ctx, s, g.ID)
	}
	sess, _ := store.ListJSON[Session](ctx, s, store.TableSessions, store.Query{Where: map[string]string{"user_id": id}})
	for _, x := range sess {
		_ = RevokeSession(ctx, s, x.ID)
	}
	return s.Delete(ctx, store.TableUsers, id)
}

// CheckPassword authenticates email/password (constant work for unknown users).
func CheckPassword(ctx context.Context, s store.Store, email, password string) (*v1.User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	r, err := getUserByEmail(ctx, s, email)
	if err != nil {
		VerifyPassword(password, dummyHash)
		if errors.Is(err, ErrUserNotFound) {
			return nil, ErrBadLogin
		}
		return nil, err
	}
	if !VerifyPassword(password, r.PasswordHash) {
		return nil, ErrBadLogin
	}
	return r.proto(), nil
}

// ---- sessions

// Session is a login session. Rows are tombstoned (Revoked) rather than
// deleted so that a revocation beats store replication lag.
type Session struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Revoked   bool      `json:"revoked,omitempty"`
}

// SessionTTL is the lifetime of dashboard sessions.
const SessionTTL = 12 * time.Hour

// RevokeSession tombstones a session.
func RevokeSession(ctx context.Context, s store.Store, id string) error {
	x, err := store.GetJSON[Session](ctx, s, store.TableSessions, id)
	if err != nil {
		if store.IsNotFound(err) {
			return nil
		}
		return err
	}
	x.Revoked = true
	return store.PutJSON(ctx, s, store.TableSessions, id, map[string]string{"user_id": x.UserID}, x)
}

// NewSession mints a session JWT for the user.
func NewSession(ctx context.Context, s store.Store, sg *Signer, userID string) (token string, exp time.Time, err error) {
	now := time.Now().UTC()
	exp = now.Add(SessionTTL)
	x := &Session{ID: ids.New(), UserID: userID, CreatedAt: now, ExpiresAt: exp}
	if err = store.PutJSON(ctx, s, store.TableSessions, x.ID, map[string]string{"user_id": userID}, x); err != nil {
		return "", time.Time{}, err
	}
	token = sg.Sign(Claims{Sub: userID, Sid: x.ID, Typ: KindSession, Scope: "read deploy admin", Iat: now.Unix(), Exp: exp.Unix()})
	return token, exp, nil
}
