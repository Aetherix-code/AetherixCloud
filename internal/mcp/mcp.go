// Package mcp is the AetherixCloud MCP server: Streamable HTTP at /mcp on
// every node, built with the official Go SDK.
//
//   - Auth: scoped API tokens (axt_...) and OAuth 2.1 access tokens issued by
//     internal/oauth, both resolved by internal/auth (so the RBAC role of the
//     user applies on top of the token scope).
//   - Tools are thin wrappers over the same Connect handlers the dashboard
//     uses (see bridge.go); the caller's own token is forwarded, so scope and
//     RBAC enforcement happens in one place (the API interceptor).
//   - Destructive tools are gated behind confirm:true with a dry-run summary
//     (confirm.go); secrets are redacted (redact.go); calls are rate limited
//     per token and audited with the agent's client name.
//   - Transport is stateful Streamable HTTP (Mcp-Session-Id) so Claude can
//     open the optional GET SSE stream. Prefer sticky sessions when you run
//     more than one node.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/auth"
	"github.com/aetherix/aetherixcloud/internal/oauth"
	"github.com/aetherix/aetherixcloud/internal/store"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

var errNotReady = errors.New("api not ready")

// Path is where the handler is mounted.
const Path = "/mcp"

// Config configures the server.
type Config struct {
	Store store.Store
	Auth  *auth.Authenticator
	// OAuth supplies the resource-metadata URL for 401 challenges (optional).
	OAuth *oauth.Server
	// API returns the handler tools call in-process (Connect RPCs plus hook
	// HTTP paths such as /api/backups). It may return nil until ready.
	API func() http.Handler
	// RatePerMinute / Burst are the per-token limits (defaults 120 / 40).
	RatePerMinute int
	Burst         int
	Version       string
	Logger        *slog.Logger
}

// Server is the MCP server.
type Server struct {
	cfg  Config
	srv  *sdk.Server
	rate *auth.RateLimiter
	log  *slog.Logger
}

// New builds the server and registers tools, resources and prompts.
func New(cfg Config) *Server {
	if cfg.RatePerMinute <= 0 {
		cfg.RatePerMinute = 120
	}
	if cfg.Burst <= 0 {
		cfg.Burst = 40
	}
	if cfg.Version == "" {
		cfg.Version = "dev"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	s := &Server{cfg: cfg, rate: auth.NewRateLimiter(cfg.RatePerMinute, cfg.Burst), log: cfg.Logger}
	s.srv = sdk.NewServer(&sdk.Implementation{Name: "aetherixcloud", Title: "AetherixCloud", Version: cfg.Version}, &sdk.ServerOptions{
		Instructions: "Full control of an AetherixCloud cluster: projects, services, deployments, logs, machines, users, " +
			"GitHub App, upgrades, S3 backup targets, build concurrency/images, and cluster settings. " +
			"Secret values are never returned. Destructive tools do nothing until called again with confirm=true; " +
			"the first call returns a dry-run summary.",
	})
	s.srv.AddReceivingMiddleware(s.filterTools)
	s.registerReadTools()
	s.registerDeployTools()
	s.registerAdminTools()
	s.registerSettingsTools()
	s.registerResources()
	s.registerPrompts()
	return s
}

// SDK exposes the underlying MCP server (tests, in-memory transports).
func (s *Server) SDK() *sdk.Server { return s.srv }

// ---- HTTP

// Handler is the /mcp endpoint.
func (s *Server) Handler() http.Handler {
	// DisableLocalhostProtection: aetherixd is reverse-proxied by Caddy to
	// 127.0.0.1:8443 with the public Host header. The SDK's DNS-rebinding
	// guard then returns 403 after a valid bearer — Claude saw that as
	// "forbidden or insufficient scope" despite a clean oauth.token.
	inner := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return s.srv },
		&sdk.StreamableHTTPOptions{JSONResponse: true, DisableLocalhostProtection: true, Logger: nil})
	limited := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := principalOf(sdkauth.TokenInfoFromContext(r.Context())); p != nil {
			if ok, wait := s.rate.Allow(p.RateKey()); !ok {
				w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
				http.Error(w, "rate limit exceeded for this token", http.StatusTooManyRequests)
				return
			}
		}
		inner.ServeHTTP(w, r)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, MCP-Protocol-Version, Mcp-Session-Id")
		w.Header().Set("Access-Control-Expose-Headers", "WWW-Authenticate, Mcp-Session-Id")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
		w.Header().Set("CDN-Cache-Control", "no-store")
		w.Header().Set("Cloudflare-CDN-Cache-Control", "no-store")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		// Claude sometimes omits Accept; the Streamable SDK requires both
		// application/json and text/event-stream on POST.
		normalizeMCPAccept(r)
		bearer := auth.BearerFromHeader(r.Header.Get("Authorization"))
		hasAuth := bearer != ""
		opts := &sdkauth.RequireBearerTokenOptions{AllowMissingExpiration: true, Scopes: []string{auth.ScopeRead}}
		if s.cfg.OAuth != nil {
			opts.ResourceMetadataURL = s.cfg.OAuth.ResourceMetadataURL(r)
		}
		rec := &statusRecorder{ResponseWriter: w, code: 200}
		var (
			verified bool
			userID   string
			client   string
		)
		verify := func(ctx context.Context, token string, req *http.Request) (*sdkauth.TokenInfo, error) {
			ti, err := s.verify(ctx, token, req)
			if err != nil {
				rec.err = err.Error()
				s.log.Info("mcp auth failed", "err", err.Error(), "token_kind", tokenPrefix(token))
				return nil, err
			}
			verified = true
			if ti != nil {
				userID = ti.UserID
				if p := principalOf(ti); p != nil {
					client = p.Client
				}
			}
			return ti, nil
		}
		sdkauth.RequireBearerToken(verify, opts)(limited).ServeHTTP(rec, r)
		if hasAuth {
			kind := tokenPrefix(bearer)
			body := strings.TrimSpace(string(rec.body))
			// mcp.auth tracks credential acceptance only. Protocol 4xx (bad
			// Accept, GET-without-session, Host checks, etc.) must not look
			// like a bad token.
			authOK := verified && rec.err == "" && rec.code != http.StatusUnauthorized && rec.code != http.StatusForbidden
			authErr := rec.err
			if !authOK && authErr == "" {
				if body != "" {
					authErr = body
					if len(authErr) > 160 {
						authErr = authErr[:160]
					}
				} else {
					switch rec.code {
					case http.StatusUnauthorized:
						authErr = "unauthorized"
					case http.StatusForbidden:
						authErr = "forbidden or insufficient scope"
					}
				}
			}
			s.log.Info("mcp request", "method", r.Method, "status", rec.code, "ua", r.UserAgent(),
				"proto", r.Header.Get("MCP-Protocol-Version"), "auth_ok", authOK, "token_kind", kind, "err", authErr)
			attrs := map[string]string{
				"status": strconv.Itoa(rec.code), "ua": r.UserAgent(),
				"method": r.Method, "proto": r.Header.Get("MCP-Protocol-Version"),
				"token_kind": kind, "host": r.Host,
			}
			if authErr != "" {
				attrs["error"] = authErr
			}
			_ = auth.WriteAudit(r.Context(), s.cfg.Store, &v1.AuditEntry{
				ActorUserId: userID, Via: "mcp", Client: client, Action: "mcp.auth",
				Ok: authOK, Error: authErr, Attributes: attrs,
			})
		}
	})
}

// normalizeMCPAccept ensures Streamable HTTP Accept requirements are met when
// the client sends an empty or overly broad value.
func normalizeMCPAccept(r *http.Request) {
	if r.Method == http.MethodGet {
		if a := r.Header.Get("Accept"); a == "" || a == "*/*" {
			r.Header.Set("Accept", "text/event-stream")
		}
		return
	}
	if r.Method != http.MethodPost {
		return
	}
	a := r.Header.Get("Accept")
	if a == "" || a == "*/*" {
		r.Header.Set("Accept", "application/json, text/event-stream")
		return
	}
	low := strings.ToLower(a)
	needJSON := !strings.Contains(low, "application/json") && !strings.Contains(low, "application/*") && !strings.Contains(low, "*/*")
	needSSE := !strings.Contains(low, "text/event-stream") && !strings.Contains(low, "text/*") && !strings.Contains(low, "*/*")
	switch {
	case needJSON && needSSE:
		r.Header.Set("Accept", a+", application/json, text/event-stream")
	case needJSON:
		r.Header.Set("Accept", a+", application/json")
	case needSSE:
		r.Header.Set("Accept", a+", text/event-stream")
	}
}

type statusRecorder struct {
	http.ResponseWriter
	code int
	err  string
	body []byte
}

func (s *statusRecorder) WriteHeader(code int) {
	s.code = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.code >= 400 && len(s.body) < 240 {
		n := min(len(b), 240-len(s.body))
		s.body = append(s.body, b[:n]...)
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// verify implements sdkauth.TokenVerifier on top of internal/auth.
func (s *Server) verify(ctx context.Context, token string, _ *http.Request) (*sdkauth.TokenInfo, error) {
	p, err := s.cfg.Auth.Authenticate(ctx, token)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidToken) {
			return nil, fmt.Errorf("%w: %v", sdkauth.ErrInvalidToken, err)
		}
		return nil, err
	}
	var scopes []string
	for _, l := range []auth.Level{auth.LevelRead, auth.LevelDeploy, auth.LevelAdmin} {
		if p.Allows(l) {
			scopes = append(scopes, l.String())
		}
	}
	if len(scopes) == 0 {
		return nil, fmt.Errorf("%w: no usable scopes on credential", sdkauth.ErrInvalidToken)
	}
	ti := &sdkauth.TokenInfo{Scopes: scopes, UserID: p.UserID, Extra: map[string]any{"principal": p}}
	if !p.Expires.IsZero() {
		ti.Expiration = p.Expires
	}
	return ti, nil
}

func tokenPrefix(t string) string {
	switch {
	case strings.HasPrefix(t, auth.OAuthAccessPrefix):
		return auth.OAuthAccessPrefix + "…"
	case strings.HasPrefix(t, auth.TokenPrefix):
		return auth.TokenPrefix + "…"
	case strings.Count(t, ".") == 2:
		return "jwt…"
	default:
		if len(t) > 12 {
			return t[:12] + "…"
		}
		return t
	}
}

func principalOf(ti *sdkauth.TokenInfo) *auth.Principal {
	if ti == nil {
		return nil
	}
	p, _ := ti.Extra["principal"].(*auth.Principal)
	return p
}

// filterTools hides tools the caller's credential cannot use.
func (s *Server) filterTools(next sdk.MethodHandler) sdk.MethodHandler {
	return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
		res, err := next(ctx, method, req)
		if err != nil || method != "tools/list" {
			return res, err
		}
		lr, ok := res.(*sdk.ListToolsResult)
		if !ok || req.GetExtra() == nil {
			return res, err
		}
		p := principalOf(req.GetExtra().TokenInfo)
		if p == nil {
			return res, err
		}
		out := *lr
		out.Tools = nil
		for _, t := range lr.Tools {
			if lv, known := toolLevels[t.Name]; !known || p.Allows(lv) {
				out.Tools = append(out.Tools, t)
			}
		}
		return &out, nil
	}
}

// ---- per-call context

// call is the identity and API access of one tool invocation.
type call struct {
	s      *Server
	p      *auth.Principal
	client string
	api    *apiClients
}

func (s *Server) callFor(extra *sdk.RequestExtra, session sdk.Session) (*call, error) {
	if extra == nil {
		return nil, errors.New("unauthenticated")
	}
	p := principalOf(extra.TokenInfo)
	if p == nil {
		return nil, errors.New("unauthenticated")
	}
	client := p.Client
	if p.Kind != auth.KindOAuth { // registered OAuth names are authoritative
		if ss, ok := session.(*sdk.ServerSession); ok && ss != nil {
			if ip := ss.InitializeParams(); ip != nil && ip.ClientInfo != nil && ip.ClientInfo.Name != "" {
				client = ip.ClientInfo.Name
			}
		}
		if client == "" && extra.Header != nil {
			client = extra.Header.Get("User-Agent")
		}
	}
	if client == "" {
		client = "unknown"
	}
	if len(client) > 80 {
		client = client[:80]
	}
	cp := *p
	cp.Via = "mcp"
	return &call{s: s, p: &cp, client: client, api: s.newAPI(p.Bearer, client)}, nil
}

func (c *call) audit(ctx context.Context, action string, attrs map[string]string, err error) {
	if attrs == nil {
		attrs = map[string]string{}
	}
	if c.p.Kind == auth.KindAPIToken {
		attrs["token_name"] = c.p.Client
	}
	e := &v1.AuditEntry{ActorUserId: c.p.UserID, ActorEmail: c.p.Email, CredentialId: c.p.CredentialID, Client: c.client,
		Via: "mcp", Action: action, Ok: err == nil, Attributes: attrs}
	if err != nil {
		e.Error = RedactText(err.Error())
		if len(e.Error) > 300 {
			e.Error = e.Error[:300]
		}
	}
	if werr := auth.WriteAudit(context.WithoutCancel(ctx), c.s.cfg.Store, e); werr != nil {
		c.s.log.Warn("mcp audit write failed", "err", werr)
	}
}

// toolLevels maps tool name -> required level (filled by register).
var toolLevels = map[string]auth.Level{}

// annotations
func ro(title string) *sdk.ToolAnnotations {
	return &sdk.ToolAnnotations{Title: title, ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(false)}
}

func write(title string, idempotent bool) *sdk.ToolAnnotations {
	return &sdk.ToolAnnotations{Title: title, DestructiveHint: ptr(false), IdempotentHint: idempotent, OpenWorldHint: ptr(false)}
}

func destructive(title string) *sdk.ToolAnnotations {
	return &sdk.ToolAnnotations{Title: title, DestructiveHint: ptr(true), OpenWorldHint: ptr(false)}
}

func ptr[T any](v T) *T { return &v }

// register adds a typed tool. h returns a JSON-able object.
func register[In any](s *Server, name, desc string, level auth.Level, ann *sdk.ToolAnnotations,
	h func(ctx context.Context, c *call, in In) (any, error)) {
	toolLevels[name] = level
	sdk.AddTool[In, any](s.srv, &sdk.Tool{Name: name, Title: ann.Title, Description: desc + " [requires " + level.String() + " access]", Annotations: ann},
		func(ctx context.Context, req *sdk.CallToolRequest, in In) (*sdk.CallToolResult, any, error) {
			c, err := s.callFor(req.Extra, req.Session)
			if err != nil {
				return nil, nil, err
			}
			attrs := argAttrs(in)
			if !c.p.Allows(level) {
				err := fmt.Errorf("permission denied: tool %q needs %s access; this credential allows at most %s", name, level, c.p.MaxLevel())
				c.audit(ctx, "mcp."+name, attrs, err)
				return nil, nil, err
			}
			out, err := h(ctx, c, in)
			c.audit(ctx, "mcp."+name, attrs, err)
			if err != nil {
				return nil, nil, friendly(err)
			}
			return nil, out, nil
		})
}

func argAttrs(in any) map[string]string {
	b, _ := json.Marshal(in)
	m := map[string]any{}
	_ = json.Unmarshal(b, &m)
	return auditAttrs(m)
}

func friendly(err error) error {
	msg := err.Error()
	return errors.New(RedactText(strings.TrimSpace(msg)))
}

var _ = time.Second
