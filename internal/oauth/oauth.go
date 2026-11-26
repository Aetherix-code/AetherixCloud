// Package oauth is the OAuth 2.1 authorization server embedded in every
// aetherixd node, used by MCP connectors (ChatGPT, Claude, Cursor...).
//
// Implemented (per the MCP authorization spec):
//
//   - RFC 9728 protected-resource metadata and RFC 8414 authorization-server
//     metadata
//   - RFC 7591 dynamic client registration (public and confidential clients)
//   - authorization-code flow with mandatory PKCE (S256), exact redirect URI
//     matching and resource indicators (RFC 8707)
//   - refresh-token rotation with reuse detection (a replayed old refresh
//     token revokes the whole grant)
//   - RFC 7009 revocation
//
// The consent screen is rendered by the dashboard SPA at /oauth/consent. It
// uses the logged-in user's session JWT against the JSON endpoints
//
//	GET  /oauth/api/consent?request=<id>   -> request details
//	POST /oauth/api/consent {request, approve, scopes} -> {redirect_url}
//
// and then navigates the browser to redirect_url.
//
// Access tokens are JWTs signed with the cluster key; codes, refresh tokens
// and grants live in the replicated store, so everything works on any node.
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/auth"
	"github.com/aetherix/aetherixcloud/internal/ids"
	"github.com/aetherix/aetherixcloud/internal/store"
)

// Paths.
const (
	PathProtectedResource = "/.well-known/oauth-protected-resource"
	PathAuthServerMeta    = "/.well-known/oauth-authorization-server"
	PathAuthorize         = "/oauth/authorize"
	PathToken             = "/oauth/token"
	PathRegister          = "/oauth/register"
	PathRevoke            = "/oauth/revoke"
	PathConsentAPI        = "/oauth/api/consent"
	PathConsentPage       = "/oauth/consent" // rendered by the dashboard
	DefaultResourcePath   = "/mcp"
)

// RefreshPrefix marks refresh tokens.
const RefreshPrefix = "axr_"

// Server is the authorization server.
type Server struct {
	Store  store.Store
	Auth   *auth.Authenticator
	Logger *slog.Logger
	// BaseURL overrides the externally visible origin (e.g.
	// "https://dash.example.com"); by default it is derived from the request
	// (honouring X-Forwarded-Proto/Host set by Caddy).
	BaseURL string
	// ResourcePath is the protected resource (default /mcp).
	ResourcePath string

	AccessTTL  time.Duration // default 1h
	RefreshTTL time.Duration // default 30d
	CodeTTL    time.Duration // default 60s
	RequestTTL time.Duration // pending consent request, default 10m

	regLimit *auth.RateLimiter
}

// New creates a Server.
func New(st store.Store, a *auth.Authenticator, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{Store: st, Auth: a, Logger: log, regLimit: auth.NewRateLimiter(30, 10)}
}

func (s *Server) accessTTL() time.Duration  { return or(s.AccessTTL, time.Hour) }
func (s *Server) refreshTTL() time.Duration { return or(s.RefreshTTL, 30*24*time.Hour) }
func (s *Server) codeTTL() time.Duration    { return or(s.CodeTTL, 5*time.Minute) }
func (s *Server) requestTTL() time.Duration { return or(s.RequestTTL, 10*time.Minute) }
func (s *Server) resPath() string {
	if s.ResourcePath == "" {
		return DefaultResourcePath
	}
	return s.ResourcePath
}
func or(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// Handlers returns the path -> handler map to mount on the daemon muxes.
func (s *Server) Handlers() map[string]http.Handler {
	return map[string]http.Handler{
		PathProtectedResource:                             http.HandlerFunc(s.protectedResource),
		PathProtectedResource + "/":                       http.HandlerFunc(s.protectedResource),
		PathProtectedResource + DefaultResourcePath:       http.HandlerFunc(s.protectedResource), // RFC 9728 path form
		PathProtectedResource + DefaultResourcePath + "/": http.HandlerFunc(s.protectedResource),
		PathAuthServerMeta:                                http.HandlerFunc(s.authServerMeta),
		PathAuthorize:                                     http.HandlerFunc(s.authorize),
		PathToken:                                         http.HandlerFunc(s.token),
		PathRegister:                                      http.HandlerFunc(s.register),
		PathRevoke:                                        http.HandlerFunc(s.revoke),
		PathConsentAPI:                                    http.HandlerFunc(s.consentAPI),
	}
}

// BaseURLFor returns the external origin for r.
func (s *Server) BaseURLFor(r *http.Request) string {
	if s.BaseURL != "" {
		return strings.TrimRight(s.BaseURL, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p == "https" || p == "http" {
		scheme = p
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = strings.TrimSpace(strings.Split(h, ",")[0])
	}
	return scheme + "://" + host
}

// ResourceMetadataURL is advertised in WWW-Authenticate challenges (RFC 9728
// path-inserted form for /mcp).
func (s *Server) ResourceMetadataURL(r *http.Request) string {
	return s.BaseURLFor(r) + PathProtectedResource + s.resPath()
}

// Scopes supported.
var scopesSupported = []string{auth.ScopeRead, auth.ScopeDeploy, auth.ScopeAdmin}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	// Cloudflare Browser Cache TTL "override" ignores Cache-Control alone;
	// CDN-Cache-Control is what keeps OAuth metadata off the edge cache.
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("CDN-Cache-Control", "no-store")
	w.Header().Set("Cloudflare-CDN-Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func oauthErr(w http.ResponseWriter, code int, e, desc string) {
	writeJSON(w, code, map[string]string{"error": e, "error_description": desc})
}

func (s *Server) tokenFail(w http.ResponseWriter, r *http.Request, code int, e, desc string) {
	s.Logger.Info("oauth token failed", "error", e, "desc", desc, "ua", r.UserAgent(), "grant_type", r.PostFormValue("grant_type"))
	_ = auth.WriteAudit(r.Context(), s.Store, &v1.AuditEntry{
		Via: "mcp", Action: "oauth.token", Ok: false, Error: e + ": " + desc,
		Client:     r.PostFormValue("client_id"),
		Attributes: map[string]string{"ua": r.UserAgent(), "grant_type": r.PostFormValue("grant_type")},
	})
	oauthErr(w, code, e, desc)
}

func (s *Server) cors(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, MCP-Protocol-Version")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

// sameResource compares RFC 8707 resource indicators (ignore one trailing slash).
func sameResource(a, b string) bool {
	return strings.TrimRight(strings.TrimSpace(a), "/") == strings.TrimRight(strings.TrimSpace(b), "/")
}

// ---- metadata

func (s *Server) protectedResource(w http.ResponseWriter, r *http.Request) {
	if s.cors(w, r) {
		return
	}
	base := s.BaseURLFor(r)
	writeJSON(w, 200, map[string]any{
		"resource":                 base + s.resPath(),
		"authorization_servers":    []string{base},
		"scopes_supported":         scopesSupported,
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "AetherixCloud",
	})
}

func (s *Server) authServerMeta(w http.ResponseWriter, r *http.Request) {
	if s.cors(w, r) {
		return
	}
	base := s.BaseURLFor(r)
	writeJSON(w, 200, map[string]any{
		"issuer":                                         base,
		"authorization_endpoint":                         base + PathAuthorize,
		"token_endpoint":                                 base + PathToken,
		"registration_endpoint":                          base + PathRegister,
		"revocation_endpoint":                            base + PathRevoke,
		"response_types_supported":                       []string{"code"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          []string{"none", "client_secret_post", "client_secret_basic"},
		"scopes_supported":                               scopesSupported,
		"authorization_response_iss_parameter_supported": true,
	})
}

// ---- clients

// Client is a registered OAuth client (stored in oauth_clients).
type Client struct {
	ID           string    `json:"id"`
	SecretHash   string    `json:"secret_hash,omitempty"`
	Name         string    `json:"name"`
	RedirectURIs []string  `json:"redirect_uris"`
	AuthMethod   string    `json:"auth_method"`
	CreatedAt    time.Time `json:"created_at"`
}

func hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func randString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func validRedirect(u string) bool {
	p, err := url.Parse(u)
	if err != nil || p.Scheme == "" || p.Fragment != "" || p.User != nil {
		return false
	}
	switch strings.ToLower(p.Scheme) {
	case "javascript", "data", "file", "vbscript", "about", "blob":
		return false
	case "https":
		return p.Host != ""
	case "http":
		h := p.Hostname()
		ip := net.ParseIP(h)
		return h == "localhost" || (ip != nil && ip.IsLoopback())
	}
	return true // custom schemes of native apps (cursor://, vscode://)
}

func clientIP(r *http.Request) string {
	if x := r.Header.Get("X-Forwarded-For"); x != "" {
		return strings.TrimSpace(strings.Split(x, ",")[0])
	}
	h, _, _ := net.SplitHostPort(r.RemoteAddr)
	return h
}

const maxClients = 2000

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	if s.cors(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		oauthErr(w, 405, "invalid_request", "POST required")
		return
	}
	if ok, _ := s.regLimit.Allow(clientIP(r)); !ok {
		oauthErr(w, 429, "temporarily_unavailable", "too many registrations")
		return
	}
	var in struct {
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
		AuthMethod   string   `json:"token_endpoint_auth_method"`
		GrantTypes   []string `json:"grant_types"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		oauthErr(w, 400, "invalid_client_metadata", "invalid JSON")
		return
	}
	if len(in.RedirectURIs) == 0 || len(in.RedirectURIs) > 10 {
		oauthErr(w, 400, "invalid_redirect_uri", "1-10 redirect_uris required")
		return
	}
	for _, u := range in.RedirectURIs {
		if !validRedirect(u) {
			oauthErr(w, 400, "invalid_redirect_uri", "unacceptable redirect URI: "+u)
			return
		}
	}
	for _, g := range in.GrantTypes {
		if g != "authorization_code" && g != "refresh_token" {
			oauthErr(w, 400, "invalid_client_metadata", "unsupported grant type "+g)
			return
		}
	}
	method := in.AuthMethod
	switch method {
	case "":
		method = "none"
	case "none", "client_secret_post", "client_secret_basic":
	default:
		oauthErr(w, 400, "invalid_client_metadata", "unsupported token_endpoint_auth_method")
		return
	}
	existing, _ := s.Store.List(r.Context(), store.TableOAuthClients, store.Query{})
	if len(existing) >= maxClients {
		oauthErr(w, 429, "temporarily_unavailable", "client registry full")
		return
	}
	name := strings.TrimSpace(in.ClientName)
	if name == "" {
		name = "Unnamed client"
	}
	if len(name) > 100 {
		name = name[:100]
	}
	c := &Client{ID: "axc_" + ids.New(), Name: name, RedirectURIs: in.RedirectURIs, AuthMethod: method, CreatedAt: time.Now().UTC()}
	resp := map[string]any{
		"client_id": c.ID, "client_id_issued_at": c.CreatedAt.Unix(), "client_name": c.Name,
		"redirect_uris": c.RedirectURIs, "token_endpoint_auth_method": method,
		"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"},
	}
	if method != "none" {
		secret := randString(32)
		c.SecretHash = hash(secret)
		resp["client_secret"] = secret
		resp["client_secret_expires_at"] = 0
	}
	if err := store.PutJSON(r.Context(), s.Store, store.TableOAuthClients, c.ID, nil, c); err != nil {
		oauthErr(w, 500, "server_error", "could not store client")
		return
	}
	writeJSON(w, 201, resp)
}

func (s *Server) client(ctx context.Context, id string) (*Client, error) {
	return store.GetJSON[Client](ctx, s.Store, store.TableOAuthClients, id)
}

// authenticateClient implements client authentication at the token endpoint.
func (s *Server) authenticateClient(r *http.Request) (*Client, error) {
	id, secret, basic := r.BasicAuth()
	if !basic {
		id, secret = r.PostFormValue("client_id"), r.PostFormValue("client_secret")
	}
	if id == "" {
		return nil, errors.New("client_id required")
	}
	c, err := s.client(r.Context(), id)
	if err != nil {
		return nil, errors.New("unknown client")
	}
	if c.AuthMethod != "none" {
		if secret == "" || subtle.ConstantTimeCompare([]byte(hash(secret)), []byte(c.SecretHash)) != 1 {
			return nil, errors.New("bad client credentials")
		}
	}
	return c, nil
}

// ---- authorization endpoint + consent

// pending is a stored authorization request / code (oauth_grants, kind
// "request" or "code").
type pending struct {
	Kind        string    `json:"kind"` // request | code
	ClientID    string    `json:"client_id"`
	RedirectURI string    `json:"redirect_uri"`
	State       string    `json:"state,omitempty"`
	Challenge   string    `json:"challenge"`
	Scopes      []string  `json:"scopes"`
	Resource    string    `json:"resource,omitempty"`
	UserID      string    `json:"user_id,omitempty"`
	Expires     time.Time `json:"expires"`
	Used        bool      `json:"used,omitempty"`
	GrantID     string    `json:"grant_id,omitempty"`
}

func (s *Server) putPending(ctx context.Context, id string, p *pending) error {
	return store.PutJSON(ctx, s.Store, store.TableOAuthGrants, id, nil, p)
}

func (s *Server) getPending(ctx context.Context, id, kind string) (*pending, error) {
	p, err := store.GetJSON[pending](ctx, s.Store, store.TableOAuthGrants, id)
	if err != nil {
		return nil, err
	}
	if p.Kind != kind {
		return nil, store.ErrNotFound
	}
	return p, nil
}

func parseScopes(s string) []string {
	var out []string
	for _, x := range strings.Fields(s) {
		if slices.Contains(scopesSupported, x) && !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

func errPage(w http.ResponseWriter, msg string) {
	http.Error(w, "authorization error: "+msg, http.StatusBadRequest)
}

func redirectErr(w http.ResponseWriter, r *http.Request, s *Server, redirect, state, code, desc string) {
	u, _ := url.Parse(redirect)
	q := u.Query()
	q.Set("error", code)
	q.Set("error_description", desc)
	if s != nil {
		q.Set("iss", s.BaseURLFor(r))
	}
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	c, err := s.client(r.Context(), q.Get("client_id"))
	if err != nil {
		errPage(w, "unknown client_id")
		return
	}
	redirect := q.Get("redirect_uri")
	if redirect == "" && len(c.RedirectURIs) == 1 {
		redirect = c.RedirectURIs[0]
	}
	if !slices.Contains(c.RedirectURIs, redirect) { // exact match only
		errPage(w, "redirect_uri is not registered for this client")
		return
	}
	state := q.Get("state")
	switch {
	case q.Get("response_type") != "code":
		redirectErr(w, r, s, redirect, state, "unsupported_response_type", "only response_type=code is supported")
		return
	case q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256":
		redirectErr(w, r, s, redirect, state, "invalid_request", "PKCE with code_challenge_method=S256 is required")
		return
	}
	// Connectors (Claude, ChatGPT) often omit scope or send only offline_access.
	// Offer every scope we support; the consent screen lets the user narrow to
	// what their role allows.
	scopes := parseScopes(q.Get("scope"))
	if len(scopes) == 0 {
		scopes = append([]string(nil), scopesSupported...)
	}
	resource := strings.TrimRight(strings.TrimSpace(q.Get("resource")), "/")
	id := "req:" + randString(24)
	p := &pending{Kind: "request", ClientID: c.ID, RedirectURI: redirect, State: state, Challenge: q.Get("code_challenge"),
		Scopes: scopes, Resource: resource, Expires: time.Now().Add(s.requestTTL())}
	if err := s.putPending(r.Context(), id, p); err != nil {
		errPage(w, "storage error")
		return
	}
	http.Redirect(w, r, PathConsentPage+"?request="+url.QueryEscape(strings.TrimPrefix(id, "req:")), http.StatusFound)
}

func (s *Server) consentAPI(w http.ResponseWriter, r *http.Request) {
	if s.cors(w, r) {
		return
	}
	p, err := s.Auth.Authenticate(r.Context(), auth.BearerFromHeader(r.Header.Get("Authorization")))
	if err != nil || p.Kind != auth.KindSession {
		oauthErr(w, 401, "login_required", "a dashboard session is required")
		return
	}
	switch r.Method {
	case http.MethodGet:
		req, c, ok := s.loadRequest(w, r, r.URL.Query().Get("request"))
		if !ok {
			return
		}
		ru, _ := url.Parse(req.RedirectURI)
		allowed := allowedScopes(p)
		// Offer every scope the user's role may grant. Clients often request only
		// "read"; the user can still add deploy/admin for full MCP control.
		offer := unionScopes(req.Scopes, allowed)
		writeJSON(w, 200, map[string]any{
			"client_id": c.ID, "client_name": c.Name, "redirect_host": ru.Host, "redirect_scheme": ru.Scheme,
			"requested_scopes": offer, "allowed_scopes": allowed, "resource": req.Resource,
			"user": map[string]string{"id": p.UserID, "email": p.Email},
		})
	case http.MethodPost:
		var in struct {
			Request string   `json:"request"`
			Approve bool     `json:"approve"`
			Scopes  []string `json:"scopes"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
			oauthErr(w, 400, "invalid_request", "invalid JSON")
			return
		}
		req, _, ok := s.loadRequest(w, r, in.Request)
		if !ok {
			return
		}
		_ = s.Store.Delete(r.Context(), store.TableOAuthGrants, "req:"+in.Request) // single use
		iss := s.BaseURLFor(r)
		if !in.Approve {
			writeJSON(w, 200, map[string]string{"redirect_url": errURL(req.RedirectURI, req.State, iss, "access_denied", "the user denied the request")})
			return
		}
		allowed := allowedScopes(p)
		var granted []string
		if in.Scopes == nil {
			granted = intersect(req.Scopes, allowed)
		} else {
			// Honor the user's checkbox selection up to their role (not capped to
			// the client's original scope list — connectors rarely know our scopes).
			granted = intersect(in.Scopes, allowed)
		}
		if len(granted) == 0 {
			writeJSON(w, 200, map[string]string{"redirect_url": errURL(req.RedirectURI, req.State, iss, "access_denied", "no permitted scopes")})
			return
		}
		code := randString(32)
		cp := &pending{Kind: "code", ClientID: req.ClientID, RedirectURI: req.RedirectURI, Challenge: req.Challenge, Scopes: granted,
			Resource: req.Resource, UserID: p.UserID, Expires: time.Now().Add(s.codeTTL())}
		if err := s.putPending(r.Context(), "code:"+hash(code), cp); err != nil {
			oauthErr(w, 500, "server_error", "storage error")
			return
		}
		u, _ := url.Parse(req.RedirectURI)
		qq := u.Query()
		qq.Set("code", code)
		qq.Set("iss", iss) // RFC 9207 — Claude validates this against AS metadata issuer
		if req.State != "" {
			qq.Set("state", req.State)
		}
		u.RawQuery = qq.Encode()
		_ = auth.WriteAudit(r.Context(), s.Store, &v1.AuditEntry{ActorUserId: p.UserID, ActorEmail: p.Email, Via: "dashboard", Action: "oauth.consent",
			Ok: true, Client: req.ClientID, Attributes: map[string]string{"scopes": strings.Join(granted, " ")}})
		writeJSON(w, 200, map[string]string{"redirect_url": u.String()})
	default:
		oauthErr(w, 405, "invalid_request", "GET or POST")
	}
}

func errURL(redirect, state, iss, code, desc string) string {
	u, _ := url.Parse(redirect)
	q := u.Query()
	q.Set("error", code)
	q.Set("error_description", desc)
	if iss != "" {
		q.Set("iss", iss)
	}
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func (s *Server) loadRequest(w http.ResponseWriter, r *http.Request, id string) (*pending, *Client, bool) {
	req, err := s.getPending(r.Context(), "req:"+id, "request")
	if err != nil || time.Now().After(req.Expires) {
		oauthErr(w, 404, "invalid_request", "unknown or expired authorization request")
		return nil, nil, false
	}
	c, err := s.client(r.Context(), req.ClientID)
	if err != nil {
		oauthErr(w, 404, "invalid_request", "client no longer exists")
		return nil, nil, false
	}
	return req, c, true
}

// allowedScopes are the scopes the user's role permits.
func allowedScopes(p *auth.Principal) []string {
	var out []string
	for i, sc := range scopesSupported {
		if auth.RoleLevel(p.Role) >= auth.Level(i+1) {
			out = append(out, sc)
		}
	}
	return out
}

func intersect(a, b []string) []string {
	var out []string
	for _, x := range a {
		if slices.Contains(b, x) && !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

// unionScopes returns scopesSupported order for every scope in a or b.
func unionScopes(a, b []string) []string {
	var out []string
	for _, sc := range scopesSupported {
		if slices.Contains(a, sc) || slices.Contains(b, sc) {
			out = append(out, sc)
		}
	}
	return out
}

// ---- token endpoint

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if s.cors(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		oauthErr(w, 405, "invalid_request", "POST required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		oauthErr(w, 400, "invalid_request", "bad form")
		return
	}
	c, err := s.authenticateClient(r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Basic realm="oauth"`)
		s.tokenFail(w, r, 401, "invalid_client", err.Error())
		return
	}
	switch r.PostFormValue("grant_type") {
	case "authorization_code":
		s.exchangeCode(w, r, c)
	case "refresh_token":
		s.refresh(w, r, c)
	default:
		s.tokenFail(w, r, 400, "unsupported_grant_type", "authorization_code or refresh_token")
	}
}

func (s *Server) issue(w http.ResponseWriter, r *http.Request, g *auth.Grant, refresh string) {
	// Opaque access tokens (not JWTs): Claude and other brokers sometimes try
	// to validate JWT shape/signature and discard HS256 tokens without JWKS.
	at := auth.MintAccessToken(g, s.accessTTL())
	if err := auth.PutGrant(r.Context(), s.Store, g); err != nil {
		oauthErr(w, 500, "server_error", "storage error")
		return
	}
	s.Logger.Info("oauth token issued", "grant", g.ID, "client", g.ClientName, "user", g.UserID,
		"scopes", strings.Join(g.Scopes, " "), "resource", g.Resource, "ua", r.UserAgent())
	_ = auth.WriteAudit(r.Context(), s.Store, &v1.AuditEntry{
		ActorUserId: g.UserID, Via: "mcp", Client: g.ClientName, Action: "oauth.token", Ok: true,
		Attributes: map[string]string{"grant_id": g.ID, "scopes": strings.Join(g.Scopes, " "), "resource": g.Resource, "ua": r.UserAgent()},
	})
	writeJSON(w, 200, map[string]any{"access_token": at, "token_type": "Bearer", "expires_in": int(s.accessTTL().Seconds()),
		"refresh_token": refresh, "scope": strings.Join(g.Scopes, " ")})
}

func (s *Server) exchangeCode(w http.ResponseWriter, r *http.Request, c *Client) {
	ctx := r.Context()
	code := r.PostFormValue("code")
	id := "code:" + hash(code)
	cp, err := s.getPending(ctx, id, "code")
	if err != nil || code == "" {
		s.tokenFail(w, r, 400, "invalid_grant", "unknown code")
		return
	}
	if cp.Used {
		// replay: revoke the grant the first exchange created
		if cp.GrantID != "" {
			_ = auth.RevokeGrant(ctx, s.Store, cp.GrantID)
		}
		s.tokenFail(w, r, 400, "invalid_grant", "code already used")
		return
	}
	if ru := r.PostFormValue("redirect_uri"); time.Now().After(cp.Expires) || cp.ClientID != c.ID || (ru != "" && ru != cp.RedirectURI) {
		s.tokenFail(w, r, 400, "invalid_grant", "code expired or does not match the client")
		return
	}
	v := r.PostFormValue("code_verifier")
	sum := sha256.Sum256([]byte(v))
	if len(v) < 43 || len(v) > 128 || subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(cp.Challenge)) != 1 {
		s.tokenFail(w, r, 400, "invalid_grant", "PKCE verification failed")
		return
	}
	// RFC 8707: resource on the token request must match the one from authorize
	// (when present). Claude binds the access token to this audience.
	resource := strings.TrimSpace(r.PostFormValue("resource"))
	switch {
	case resource != "" && cp.Resource != "" && !sameResource(resource, cp.Resource):
		s.tokenFail(w, r, 400, "invalid_target", "resource does not match the authorization request")
		return
	case resource != "":
		cp.Resource = strings.TrimRight(resource, "/")
	case cp.Resource != "":
		cp.Resource = strings.TrimRight(cp.Resource, "/")
	default:
		cp.Resource = s.BaseURLFor(r) + s.resPath()
	}
	u, err := auth.GetUser(ctx, s.Store, cp.UserID)
	if err != nil {
		s.tokenFail(w, r, 400, "invalid_grant", "user no longer exists")
		return
	}
	scopes := intersect(cp.Scopes, allowedScopesForRole(u.Role))
	secret := randString(32)
	g := &auth.Grant{ID: ids.New(), ClientID: c.ID, ClientName: c.Name, UserID: u.Id, Scopes: scopes, Resource: cp.Resource,
		CreatedAt: time.Now().UTC(), RefreshHash: hash(secret), RefreshExpires: time.Now().Add(s.refreshTTL())}
	if err := auth.PutGrant(ctx, s.Store, g); err != nil {
		s.tokenFail(w, r, 500, "server_error", "storage error")
		return
	}
	cp.Used, cp.GrantID = true, g.ID
	_ = s.putPending(ctx, id, cp)
	s.issue(w, r, g, RefreshPrefix+g.ID+"."+secret)
}

func allowedScopesForRole(r v1.Role) []string {
	var out []string
	for i, sc := range scopesSupported {
		if auth.RoleLevel(r) >= auth.Level(i+1) {
			out = append(out, sc)
		}
	}
	return out
}

func splitRefresh(t string) (grantID, secret string, ok bool) {
	if !strings.HasPrefix(t, RefreshPrefix) {
		return "", "", false
	}
	grantID, secret, ok = strings.Cut(strings.TrimPrefix(t, RefreshPrefix), ".")
	return grantID, secret, ok && grantID != "" && secret != ""
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request, c *Client) {
	ctx := r.Context()
	gid, secret, ok := splitRefresh(r.PostFormValue("refresh_token"))
	if !ok {
		oauthErr(w, 400, "invalid_grant", "malformed refresh token")
		return
	}
	g, err := auth.GetGrant(ctx, s.Store, gid)
	if err != nil || g.Revoked || g.ClientID != c.ID || time.Now().After(g.RefreshExpires) {
		oauthErr(w, 400, "invalid_grant", "refresh token is invalid, expired or revoked")
		return
	}
	h := hash(secret)
	switch {
	case g.PrevRefresh != "" && subtle.ConstantTimeCompare([]byte(h), []byte(g.PrevRefresh)) == 1:
		// An already-rotated token came back: assume theft, kill the grant.
		_ = auth.RevokeGrant(ctx, s.Store, g.ID)
		_ = auth.WriteAudit(ctx, s.Store, &v1.AuditEntry{ActorUserId: g.UserID, Via: "mcp", Client: g.ClientName, Action: "oauth.refresh_reuse",
			Ok: false, Error: "refresh token reuse detected; grant revoked", Attributes: map[string]string{"grant_id": g.ID}})
		oauthErr(w, 400, "invalid_grant", "refresh token reuse detected")
		return
	case subtle.ConstantTimeCompare([]byte(h), []byte(g.RefreshHash)) != 1:
		oauthErr(w, 400, "invalid_grant", "refresh token is invalid")
		return
	}
	u, err := auth.GetUser(ctx, s.Store, g.UserID)
	if err != nil {
		oauthErr(w, 400, "invalid_grant", "user no longer exists")
		return
	}
	g.Scopes = intersect(g.Scopes, allowedScopesForRole(u.Role))
	if req := parseScopes(r.PostFormValue("scope")); len(req) > 0 {
		g.Scopes = intersect(req, g.Scopes)
	}
	next := randString(32)
	g.PrevRefresh, g.RefreshHash = g.RefreshHash, hash(next)
	g.RefreshExpires = time.Now().Add(s.refreshTTL())
	if err := auth.PutGrant(ctx, s.Store, g); err != nil {
		oauthErr(w, 500, "server_error", "storage error")
		return
	}
	s.issue(w, r, g, RefreshPrefix+g.ID+"."+next)
}

// ---- revocation (RFC 7009)

func (s *Server) revoke(w http.ResponseWriter, r *http.Request) {
	if s.cors(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		oauthErr(w, 405, "invalid_request", "POST required")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	_ = r.ParseForm()
	c, err := s.authenticateClient(r)
	if err != nil {
		oauthErr(w, 401, "invalid_client", err.Error())
		return
	}
	tok := r.PostFormValue("token")
	gid := ""
	if id, _, ok := splitRefresh(tok); ok {
		gid = id
	} else if id, _, ok := auth.SplitOAuthAccess(tok); ok {
		gid = id
	} else if cl, err := s.Auth.Signer.Parse(tok); err == nil && cl.Typ == auth.KindOAuth {
		gid = cl.Sid // legacy JWT access tokens
	}
	if gid != "" {
		if g, err := auth.GetGrant(r.Context(), s.Store, gid); err == nil && g.ClientID == c.ID {
			_ = auth.RevokeGrant(r.Context(), s.Store, gid)
		}
	}
	w.WriteHeader(http.StatusOK) // always 200, even for unknown tokens
}

// Run deletes expired codes/requests hourly.
func (s *Server) Run(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		recs, _ := s.Store.List(ctx, store.TableOAuthGrants, store.Query{})
		for _, rec := range recs {
			if strings.HasPrefix(rec.ID, "req:") || strings.HasPrefix(rec.ID, "code:") {
				var p pending
				if json.Unmarshal(rec.Data, &p) == nil && time.Since(p.Expires) > time.Hour {
					_ = s.Store.Delete(ctx, store.TableOAuthGrants, rec.ID)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
