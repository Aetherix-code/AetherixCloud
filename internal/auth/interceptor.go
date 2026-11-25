package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"connectrpc.com/connect"
	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/store"
	"google.golang.org/protobuf/proto"
)

// Headers used by the in-process MCP bridge. The internal secret is random
// per process, so external callers cannot forge "mcp" provenance or suppress
// RPC-level auditing.
const (
	InternalHeader = "X-Ax-Internal"
	ClientHeader   = "X-Ax-Client"
)

var internalSecret = func() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}()

// InternalSecret returns the per-process secret for InternalHeader.
func InternalSecret() string { return internalSecret }

func isInternal(h http.Header) bool {
	return subtle.ConstantTimeCompare([]byte(h.Get(InternalHeader)), []byte(internalSecret)) == 1
}

// Interceptor authenticates and authorises every Connect call (unary and
// streaming) and records mutating calls in the audit log. Install it only on
// the public listener; the mesh/loopback listener is trusted.
type Interceptor struct {
	Auth   *Authenticator
	Logger *slog.Logger
}

// NewInterceptor returns the RBAC interceptor.
func NewInterceptor(a *Authenticator, log *slog.Logger) *Interceptor {
	if log == nil {
		log = slog.Default()
	}
	return &Interceptor{Auth: a, Logger: log}
}

type decision struct {
	ctx      context.Context
	p        *Principal
	rule     Rule
	internal bool
}

func (i *Interceptor) authorize(ctx context.Context, procedure string, h http.Header) (*decision, error) {
	rule := RuleFor(procedure)
	d := &decision{ctx: ctx, rule: rule, internal: isInternal(h)}
	bearer := BearerFromHeader(h.Get("Authorization"))
	if rule.Access == AccessPublic {
		return d, nil
	}
	if bearer == "" {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("missing bearer token"))
	}
	p, err := i.Auth.Authenticate(ctx, bearer)
	if err != nil {
		if errors.Is(err, ErrInvalidToken) {
			return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid or expired token"))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if p.Kind == KindOAuth && !d.internal {
		// MCP connector tokens are only valid on /mcp (audience binding).
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("OAuth connector tokens are only valid for the MCP endpoint"))
	}
	switch {
	case d.internal:
		p.Via = "mcp"
		if c := h.Get(ClientHeader); c != "" {
			p.Client = c
		}
	case p.Kind == KindSession:
		p.Via = "dashboard"
	default:
		p.Via = "api"
	}
	need := rule.Level
	if rule.Access == AccessSelf {
		need = LevelRead
	}
	if !p.Allows(need) {
		return nil, connect.NewError(connect.CodePermissionDenied,
			fmt.Errorf("%s requires %q access; this credential allows at most %q (scope %v, role %s)",
				procedure, need, p.MaxLevel(), p.Scopes, p.Role))
	}
	d.p = p
	d.ctx = WithPrincipal(ctx, p)
	return d, nil
}

func (i *Interceptor) audit(d *decision, procedure string, msg proto.Message, err error) {
	// Reads, self-service and MCP-originated calls (the MCP layer writes
	// tool-level entries itself) are not audited here.
	if d.p == nil || d.rule.Access != AccessLevel || d.rule.Level < LevelDeploy || d.internal {
		return
	}
	e := &v1.AuditEntry{ActorUserId: d.p.UserID, ActorEmail: d.p.Email, CredentialId: d.p.CredentialID,
		Client: d.p.Client, Via: d.p.Via, Action: procedure, Ok: err == nil, Attributes: AttrsOf(msg)}
	if err != nil {
		e.Error = connect.CodeOf(err).String()
	}
	if werr := WriteAudit(context.WithoutCancel(d.ctx), i.Auth.Store, e); werr != nil {
		i.Logger.Warn("audit write failed", "err", werr)
	}
}

// WrapUnary implements connect.Interceptor.
func (i *Interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		proc := req.Spec().Procedure
		d, err := i.authorize(ctx, proc, req.Header())
		if err != nil {
			return nil, err
		}
		resp, err := next(d.ctx, req)
		msg, _ := req.Any().(proto.Message)
		i.audit(d, proc, msg, err)
		return resp, err
	}
}

// WrapStreamingClient implements connect.Interceptor (no-op).
func (i *Interceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// WrapStreamingHandler implements connect.Interceptor.
func (i *Interceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		proc := conn.Spec().Procedure
		d, err := i.authorize(ctx, proc, conn.RequestHeader())
		if err != nil {
			return err
		}
		err = next(d.ctx, conn)
		i.audit(d, proc, nil, err)
		return err
	}
}

// compile-time check
var _ connect.Interceptor = (*Interceptor)(nil)

var _ = store.ErrNotFound
