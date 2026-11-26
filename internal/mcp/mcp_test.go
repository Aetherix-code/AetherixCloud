package mcp_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/gen/aetherix/v1/aetherixv1connect"
	"github.com/aetherix/aetherixcloud/internal/api"
	"github.com/aetherix/aetherixcloud/internal/auth"
	"github.com/aetherix/aetherixcloud/internal/cluster"
	"github.com/aetherix/aetherixcloud/internal/lease"
	"github.com/aetherix/aetherixcloud/internal/mcp"
	"github.com/aetherix/aetherixcloud/internal/oauth"
	"github.com/aetherix/aetherixcloud/internal/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

type env struct {
	st    store.Store
	admin string // cluster admin API token
	srv   *httptest.Server
	authn *auth.Authenticator
	oa    *oauth.Server
	mcp   *mcp.Server
}

func newEnv(t *testing.T, cfg func(*mcp.Config)) *env {
	ctx := context.Background()
	st := store.NewMemory()
	state, admin, err := cluster.Init(cluster.InitOptions{ClusterName: "t", MachineName: "node-1", Advertise: []string{"127.0.0.1"}})
	require.NoError(t, err)
	require.NoError(t, state.ApplyBootstrap(ctx, st))
	authn, err := auth.NewAuthenticator(st, state.ClusterKey)
	require.NoError(t, err)
	pub := api.PublicHandler(api.Deps{Store: st, State: state, Leases: &lease.Manager{Store: st, Self: state.MachineID, Now: time.Now}})
	oa := oauth.New(st, authn, nil)
	c := mcp.Config{Store: st, Auth: authn, OAuth: oa, API: func() http.Handler { return pub }}
	if cfg != nil {
		cfg(&c)
	}
	m := mcp.New(c)
	mux := http.NewServeMux()
	mux.Handle(mcp.Path, m.Handler())
	for p, h := range oa.Handlers() {
		mux.Handle(p, h)
	}
	mux.Handle("/", pub)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &env{st: st, admin: admin, srv: srv, authn: authn, oa: oa, mcp: m}
}

type bearerRT struct{ tok, ua string }

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.tok != "" {
		r.Header.Set("Authorization", "Bearer "+b.tok)
	}
	if b.ua != "" {
		r.Header.Set("User-Agent", b.ua)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func (e *env) connect(t *testing.T, tok string) *sdk.ClientSession {
	cl := sdk.NewClient(&sdk.Implementation{Name: "test-agent", Version: "1"}, nil)
	cs, err := cl.Connect(context.Background(), &sdk.StreamableClientTransport{Endpoint: e.srv.URL + mcp.Path,
		HTTPClient: &http.Client{Transport: bearerRT{tok: tok, ua: "test-agent/1"}}, DisableStandaloneSSE: true}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// user creates a user and an API token with the given scopes.
func (e *env) token(t *testing.T, role v1.Role, scopes ...string) string {
	u, err := auth.CreateUser(context.Background(), e.st, strings.ToLower(role.String())+time.Now().Format("150405.000000")+"@x.io", "", "password123", role)
	require.NoError(t, err)
	plain, _, err := auth.CreateUserToken(context.Background(), e.st, u.Id, "tok-"+strings.Join(scopes, "+"), scopes, 0)
	require.NoError(t, err)
	return plain
}

func call(t *testing.T, cs *sdk.ClientSession, name string, args map[string]any) (map[string]any, *sdk.CallToolResult) {
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err)
	var out map[string]any
	if m, ok := res.StructuredContent.(map[string]any); ok {
		out = m
	}
	return out, res
}

func text(res *sdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func (e *env) project(t *testing.T, name string) *v1.Project {
	pc := aetherixv1connect.NewProjectServiceClient(http.DefaultClient, e.srv.URL)
	r := connect.NewRequest(&v1.CreateProjectRequest{Name: name})
	r.Header().Set("Authorization", "Bearer "+e.admin)
	resp, err := pc.CreateProject(context.Background(), r)
	require.NoError(t, err)
	return resp.Msg.Project
}

func TestToolCatalogAnnotationsAndScopeFiltering(t *testing.T) {
	e := newEnv(t, nil)
	admin := e.connect(t, e.admin)
	tools, err := admin.ListTools(context.Background(), nil)
	require.NoError(t, err)
	by := map[string]*sdk.Tool{}
	for _, tl := range tools.Tools {
		by[tl.Name] = tl
	}
	for _, n := range []string{"list_projects", "list_services", "get_service", "list_deployments", "get_deployment", "get_logs", "get_build_logs",
		"get_metrics", "list_machines", "get_machine", "get_cluster_status", "list_domains", "list_variables",
		"get_build_concurrency", "list_s3_targets", "list_backups", "list_api_tokens",
		"create_service", "deploy_service", "redeploy", "rollback", "scale_service", "set_variables", "add_domain", "restart_service",
		"stop_service", "start_service", "update_service", "remove_domain", "cancel_deployment", "create_project", "deploy_compose",
		"create_join_command", "drain_machine", "cordon_machine", "remove_machine", "update_machine", "delete_service", "delete_project",
		"update_cluster", "create_user", "list_users", "set_build_concurrency", "create_s3_target"} {
		require.Contains(t, by, n, "tool %s", n)
		require.NotNil(t, by[n].Annotations, n)
		require.NotNil(t, by[n].InputSchema, n)
	}
	require.True(t, by["list_projects"].Annotations.ReadOnlyHint)
	require.True(t, by["get_logs"].Annotations.ReadOnlyHint)
	require.False(t, by["delete_service"].Annotations.ReadOnlyHint)
	require.True(t, *by["delete_service"].Annotations.DestructiveHint)
	require.True(t, *by["remove_machine"].Annotations.DestructiveHint)
	require.False(t, *by["deploy_service"].Annotations.DestructiveHint)

	names := func(tok string) map[string]bool {
		cs := e.connect(t, tok)
		ts, err := cs.ListTools(context.Background(), nil)
		require.NoError(t, err)
		m := map[string]bool{}
		for _, x := range ts.Tools {
			m[x.Name] = true
		}
		return m
	}
	read := names(e.token(t, v1.Role_ROLE_OWNER, "read"))
	require.True(t, read["list_projects"])
	require.False(t, read["deploy_service"])
	require.False(t, read["delete_service"])
	dep := names(e.token(t, v1.Role_ROLE_OWNER, "deploy"))
	require.True(t, dep["deploy_service"])
	require.False(t, dep["delete_service"])
	require.True(t, names(e.token(t, v1.Role_ROLE_OWNER, "admin"))["delete_service"])
	// RBAC caps an admin-scope token of a developer
	devAdmin := names(e.token(t, v1.Role_ROLE_DEVELOPER, "admin"))
	require.True(t, devAdmin["deploy_service"])
	require.False(t, devAdmin["delete_project"])
}

func TestAuthRequired(t *testing.T) {
	e := newEnv(t, nil)
	do := func(tok string) *http.Response {
		req, _ := http.NewRequest("POST", e.srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		return resp
	}
	resp := do("")
	require.Equal(t, 401, resp.StatusCode)
	require.Contains(t, resp.Header.Get("WWW-Authenticate"), "resource_metadata=")
	require.Contains(t, resp.Header.Get("WWW-Authenticate"), "/.well-known/oauth-protected-resource")
	require.Equal(t, 401, do("axt_bogus").StatusCode)
}

func TestReadWriteFlowAndRedaction(t *testing.T) {
	e := newEnv(t, nil)
	e.project(t, "shop")
	cs := e.connect(t, e.admin)

	out, res := call(t, cs, "create_service", map[string]any{"project": "shop", "name": "web", "image": "nginx:1.27", "port": 80, "deploy": false,
		"env": map[string]any{"DB_PASSWORD": "hunter2-super", "PLAIN": "ok", "DATABASE_URL": "postgres://u:pw123@db:5432/x"}})
	require.False(t, res.IsError, text(res))
	svc := out["service"].(map[string]any)
	require.NotEmpty(t, svc["id"])
	// env secrets never leave the server
	require.NotContains(t, text(res), "hunter2-super")
	require.NotContains(t, text(res), "pw123")
	require.Contains(t, text(res), "ok")

	out, res = call(t, cs, "get_service", map[string]any{"service": "web", "project": "shop"})
	require.False(t, res.IsError, text(res))
	require.NotContains(t, text(res), "hunter2-super")
	require.Equal(t, "web", out["service"].(map[string]any)["name"])

	out, res = call(t, cs, "list_services", map[string]any{"project": "shop"})
	require.False(t, res.IsError, text(res))
	require.Len(t, out["services"], 1)

	// variables: plain + secret; secret is write-only
	_, res = call(t, cs, "set_variables", map[string]any{"project": "shop", "service": "web",
		"variables": map[string]any{"MODE": "prod"}, "secrets": map[string]any{"API_KEY": "sk-live-123456789"}})
	require.False(t, res.IsError, text(res))
	require.NotContains(t, text(res), "sk-live-123456789")
	out, res = call(t, cs, "list_variables", map[string]any{"project": "shop", "service": "web"})
	require.False(t, res.IsError, text(res))
	require.NotContains(t, text(res), "sk-live-123456789")
	require.Contains(t, text(res), "prod")
	var apiKey map[string]any
	for _, v := range out["variables"].([]any) {
		if v.(map[string]any)["key"] == "API_KEY" {
			apiKey = v.(map[string]any)
		}
	}
	require.NotNil(t, apiKey)
	require.Equal(t, true, apiKey["redacted"])

	out, res = call(t, cs, "list_projects", nil)
	require.False(t, res.IsError)
	require.Len(t, out["projects"], 1)
	out, res = call(t, cs, "get_cluster_status", nil)
	require.False(t, res.IsError, text(res))
	require.EqualValues(t, 1, out["machines_total"])
	require.NotContains(t, text(res), "BEGIN CERTIFICATE")
	_, res = call(t, cs, "list_machines", nil)
	require.False(t, res.IsError)
	_, res = call(t, cs, "list_deployments", map[string]any{"service": "web", "project": "shop"})
	require.False(t, res.IsError, text(res))
	_, res = call(t, cs, "list_domains", map[string]any{"service": "web", "project": "shop"})
	require.False(t, res.IsError, text(res))
	_, res = call(t, cs, "get_metrics", map[string]any{})
	require.False(t, res.IsError, text(res))
	_, res = call(t, cs, "list_events", nil)
	require.False(t, res.IsError, text(res))
	_, res = call(t, cs, "scale_service", map[string]any{"service": "web", "project": "shop", "replicas": 2})
	require.False(t, res.IsError, text(res))
	_, res = call(t, cs, "restart_service", map[string]any{"service": "web", "project": "shop"})
	require.False(t, res.IsError, text(res))

	// templates generate secrets server side
	out, res = call(t, cs, "create_service", map[string]any{"project": "shop", "name": "db", "template": "postgres", "deploy": false})
	require.False(t, res.IsError, text(res))
	require.Contains(t, out["generated_secret_variables"], "POSTGRES_PASSWORD")
	_, res = call(t, cs, "create_service", map[string]any{"project": "shop", "name": "bad", "template": "nope"})
	require.True(t, res.IsError)

	// errors are tool errors, not protocol failures
	_, res = call(t, cs, "get_service", map[string]any{"service": "ghost", "project": "shop"})
	require.True(t, res.IsError)
	require.Contains(t, text(res), "not found")
}

func TestScopeAndRBACEnforcementOnCalls(t *testing.T) {
	e := newEnv(t, nil)
	e.project(t, "p")
	// read scope cannot call deploy tools even if it guesses the name
	ro := e.connect(t, e.token(t, v1.Role_ROLE_OWNER, "read"))
	_, res := call(t, ro, "create_service", map[string]any{"project": "p", "name": "x", "image": "nginx"})
	require.True(t, res.IsError)
	require.Contains(t, text(res), "permission denied")
	_, res = call(t, ro, "list_projects", nil)
	require.False(t, res.IsError)
	// a viewer with an admin-scope token: RBAC wins
	v := e.connect(t, e.token(t, v1.Role_ROLE_VIEWER, "admin"))
	_, res = call(t, v, "create_service", map[string]any{"project": "p", "name": "x", "image": "nginx"})
	require.True(t, res.IsError)
	require.Contains(t, text(res), "permission denied")
	// developer with admin scope cannot delete
	d := e.connect(t, e.token(t, v1.Role_ROLE_DEVELOPER, "admin"))
	_, res = call(t, d, "delete_project", map[string]any{"project": "p", "confirm": true})
	require.True(t, res.IsError)
	require.Contains(t, text(res), "permission denied")
}

func TestConfirmationGating(t *testing.T) {
	e := newEnv(t, nil)
	e.project(t, "p")
	admin := e.connect(t, e.admin)
	for _, n := range []string{"a", "b"} {
		_, res := call(t, admin, "create_service", map[string]any{"project": "p", "name": n, "image": "nginx", "deploy": false})
		require.False(t, res.IsError, text(res))
	}
	exists := func(name string) bool {
		_, res := call(t, admin, "get_service", map[string]any{"service": name, "project": "p"})
		return !res.IsError
	}

	// admin scope: dry run without confirm, executes with confirm
	out, res := call(t, admin, "delete_service", map[string]any{"service": "a", "project": "p"})
	require.False(t, res.IsError, text(res))
	require.Equal(t, true, out["dry_run"])
	require.Equal(t, false, out["executed"])
	require.Contains(t, out["summary"], "service")
	require.True(t, exists("a"), "dry run must not delete")
	out, res = call(t, admin, "delete_service", map[string]any{"service": "a", "project": "p", "confirm": true})
	require.False(t, res.IsError, text(res))
	require.Equal(t, true, out["executed"])
	require.False(t, exists("a"))

	// non-admin scope (deploy) scaling to zero: confirm alone is not enough
	dep := e.connect(t, e.token(t, v1.Role_ROLE_OWNER, "deploy"))
	args := map[string]any{"service": "b", "project": "p", "replicas": 0}
	out, res = call(t, dep, "scale_service", args)
	require.False(t, res.IsError, text(res))
	require.Equal(t, true, out["dry_run"])
	token, _ := out["confirmation_token"].(string)
	require.NotEmpty(t, token)

	args["confirm"] = true
	out, res = call(t, dep, "scale_service", args)
	require.False(t, res.IsError)
	require.Equal(t, true, out["dry_run"], "confirm without token must stay a dry run")
	args["confirmation_token"] = "9999999999.forged"
	_, res = call(t, dep, "scale_service", args)
	require.True(t, res.IsError)
	// a token minted for another credential is rejected
	other := e.connect(t, e.token(t, v1.Role_ROLE_OWNER, "deploy"))
	args["confirmation_token"] = token
	_, res = call(t, other, "scale_service", args)
	require.True(t, res.IsError)
	out, res = call(t, dep, "scale_service", args)
	require.False(t, res.IsError, text(res))
	require.Equal(t, true, out["executed"])
	// non-destructive scale needs no confirmation
	_, res = call(t, dep, "scale_service", map[string]any{"service": "b", "project": "p", "replicas": 3})
	require.False(t, res.IsError, text(res))

	// delete_project: dry run lists what would go
	_, _ = call(t, admin, "create_service", map[string]any{"project": "p", "name": "c", "image": "nginx", "deploy": false})
	out, res = call(t, admin, "delete_project", map[string]any{"project": "p"})
	require.False(t, res.IsError, text(res))
	require.Equal(t, true, out["dry_run"])
	require.Contains(t, text(res), "services_to_delete")
	_, res = call(t, admin, "list_projects", nil)
	require.Contains(t, text(res), `"p"`)
	// machines
	out, res = call(t, admin, "remove_machine", map[string]any{"machine": "node-1"})
	require.False(t, res.IsError, text(res))
	require.Equal(t, true, out["dry_run"])
	out, res = call(t, admin, "drain_machine", map[string]any{"machine": "node-1"})
	require.False(t, res.IsError, text(res))
	require.Equal(t, true, out["dry_run"])
	out, res = call(t, admin, "deploy_compose", map[string]any{"project": "p", "compose_yaml": "services:\n  w:\n    image: nginx\n", "prune": true})
	require.False(t, res.IsError, text(res))
	require.Equal(t, true, out["dry_run"])
	_, res = call(t, admin, "create_join_command", map[string]any{"machine_name": "w2"})
	require.False(t, res.IsError, text(res))
	require.Contains(t, text(res), "axjoin1.")
}

func TestDeployLifecycleToolsAndDomain(t *testing.T) {
	e := newEnv(t, nil)
	e.project(t, "shop")
	admin := e.connect(t, e.admin)
	_, res := call(t, admin, "create_service", map[string]any{"project": "shop", "name": "web", "image": "nginx:1.27", "port": 80, "deploy": false})
	require.False(t, res.IsError, text(res))

	out, res := call(t, admin, "deploy_service", map[string]any{"service": "web", "project": "shop"})
	require.False(t, res.IsError, text(res))
	dep := out["deployment"].(map[string]any)
	require.NotEmpty(t, dep["id"])
	depID := dep["id"].(string)

	out, res = call(t, admin, "get_deployment", map[string]any{"deployment_id": depID})
	require.False(t, res.IsError, text(res))
	require.Equal(t, depID, out["deployment"].(map[string]any)["id"])

	_, res = call(t, admin, "redeploy", map[string]any{"service": "web", "project": "shop"})
	require.False(t, res.IsError, text(res))
	// second deploy creates history so rollback has a target
	_, res = call(t, admin, "deploy_service", map[string]any{"service": "web", "project": "shop"})
	require.False(t, res.IsError, text(res))
	_, res = call(t, admin, "rollback", map[string]any{"service": "web", "project": "shop", "deployment_id": depID})
	require.False(t, res.IsError, text(res))

	out, res = call(t, admin, "add_domain", map[string]any{"service": "web", "project": "shop", "hostname": "web.example.com"})
	require.False(t, res.IsError, text(res))
	require.Equal(t, "web.example.com", out["domain"].(map[string]any)["hostname"])

	_, res = call(t, admin, "get_logs", map[string]any{"service": "web", "project": "shop"})
	require.False(t, res.IsError, text(res))
	_, res = call(t, admin, "get_build_logs", map[string]any{"build_id": "missing"})
	require.True(t, res.IsError)

	// read scope cannot mutate
	ro := e.connect(t, e.token(t, v1.Role_ROLE_OWNER, "read"))
	_, res = call(t, ro, "deploy_service", map[string]any{"service": "web", "project": "shop"})
	require.True(t, res.IsError)
	require.Contains(t, text(res), "permission denied")
	_, res = call(t, ro, "add_domain", map[string]any{"service": "web", "project": "shop", "hostname": "x.example.com"})
	require.True(t, res.IsError)
}

func TestAuditAndRateLimit(t *testing.T) {
	e := newEnv(t, func(c *mcp.Config) { c.RatePerMinute = 60; c.Burst = 5 })
	e.project(t, "p")
	user, err := auth.CreateUser(context.Background(), e.st, "u@x.io", "", "password123", v1.Role_ROLE_OWNER)
	require.NoError(t, err)
	tok, _, _ := auth.CreateUserToken(context.Background(), e.st, user.Id, "cursor-laptop", []string{"admin"}, 0)
	cs := e.connect(t, tok)
	_, res := call(t, cs, "create_service", map[string]any{"project": "p", "name": "web", "image": "nginx", "deploy": false,
		"env": map[string]any{"SECRET_KEY": "topsecret"}})
	require.False(t, res.IsError, text(res))
	_, _ = call(t, cs, "get_service", map[string]any{"service": "ghost", "project": "p"})

	entries, err := auth.ListAudit(context.Background(), e.st, user.Id, "mcp.", 50)
	require.NoError(t, err)
	byAction := map[string]*v1.AuditEntry{}
	for _, en := range entries {
		byAction[en.Action] = en
	}
	cr := byAction["mcp.create_service"]
	require.NotNil(t, cr)
	require.Equal(t, "mcp", cr.Via)
	require.Equal(t, "test-agent", cr.Client) // client name from MCP initialize/UA
	require.Equal(t, "web", cr.Attributes["name"])
	require.Equal(t, "cursor-laptop", cr.Attributes["token_name"])
	require.True(t, cr.Ok)
	require.NotContains(t, cr.String(), "topsecret")
	require.False(t, byAction["mcp.get_service"].Ok)
	// the nested RPC was not double-logged as an API call
	all, _ := auth.ListAudit(context.Background(), e.st, user.Id, "/aetherix", 50)
	require.Empty(t, all)

	// rate limit: burst 5 then HTTP 429 for this token only
	hit429 := false
	for i := 0; i < 30 && !hit429; i++ {
		req, _ := http.NewRequest("POST", e.srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		if resp.StatusCode == 429 {
			hit429 = true
			require.NotEmpty(t, resp.Header.Get("Retry-After"))
		}
		resp.Body.Close()
	}
	require.True(t, hit429)
	cs2 := e.connect(t, e.admin) // another token is unaffected
	_, res = call(t, cs2, "list_projects", nil)
	require.False(t, res.IsError)
}

func TestResourcesAndPrompts(t *testing.T) {
	e := newEnv(t, nil)
	p := e.project(t, "shop")
	cs := e.connect(t, e.admin)
	_, res := call(t, cs, "create_service", map[string]any{"project": "shop", "name": "web", "image": "nginx", "deploy": false,
		"env": map[string]any{"API_TOKEN": "tok-abcdef-123456"}})
	require.False(t, res.IsError)
	sid := ""
	{
		out, _ := call(t, cs, "get_service", map[string]any{"service": "web", "project": "shop"})
		sid = out["service"].(map[string]any)["id"].(string)
	}

	rs, err := cs.ListResources(context.Background(), nil)
	require.NoError(t, err)
	require.Len(t, rs.Resources, 1)
	rts, err := cs.ListResourceTemplates(context.Background(), nil)
	require.NoError(t, err)
	var tpls []string
	for _, x := range rts.ResourceTemplates {
		tpls = append(tpls, x.URITemplate)
	}
	require.ElementsMatch(t, []string{"aetherix://project/{id}", "aetherix://service/{id}", "aetherix://service/{id}/logs"}, tpls)

	read := func(uri string) string {
		r, err := cs.ReadResource(context.Background(), &sdk.ReadResourceParams{URI: uri})
		require.NoError(t, err, uri)
		return r.Contents[0].Text
	}
	require.Contains(t, read("aetherix://cluster/status"), "machines_total")
	pr := read("aetherix://project/" + p.Id)
	require.Contains(t, pr, `"web"`)
	require.NotContains(t, pr, "tok-abcdef-123456")
	sv := read("aetherix://service/" + sid)
	require.Contains(t, sv, "nginx")
	require.NotContains(t, sv, "tok-abcdef-123456")
	_ = read("aetherix://service/" + sid + "/logs")
	_, err = cs.ReadResource(context.Background(), &sdk.ReadResourceParams{URI: "aetherix://service/nope"})
	require.Error(t, err)
	// a read-only credential can read resources too, but not when invalid
	require.Error(t, func() error {
		_, err := e.connect(t, e.token(t, v1.Role_ROLE_VIEWER, "read")).ReadResource(context.Background(), &sdk.ReadResourceParams{URI: "aetherix://project/zzz"})
		return err
	}())

	ps, err := cs.ListPrompts(context.Background(), nil)
	require.NoError(t, err)
	var pn []string
	for _, x := range ps.Prompts {
		pn = append(pn, x.Name)
	}
	require.Contains(t, pn, "diagnose_failing_deployment")
	require.Contains(t, pn, "deploy_repo")
	gp, err := cs.GetPrompt(context.Background(), &sdk.GetPromptParams{Name: "diagnose_failing_deployment", Arguments: map[string]string{"service": "web"}})
	require.NoError(t, err)
	require.Contains(t, gp.Messages[0].Content.(*sdk.TextContent).Text, "get_logs")
	_, err = cs.GetPrompt(context.Background(), &sdk.GetPromptParams{Name: "deploy_repo"})
	require.Error(t, err)
}

func TestRedactText(t *testing.T) {
	in := strings.Join([]string{
		"connecting to postgres://app:s3cr3tpw@db:5432/app",
		"Authorization: Bearer abcdefghijklmnop1234567890",
		"PASSWORD=hunter2 token: abc123def456 ok",
		"key axt_" + strings.Repeat("A", 40),
		"jwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.abcdefghij",
		"-----BEGIN RSA PRIVATE KEY-----\nMIIB\n-----END RSA PRIVATE KEY-----",
		"plain line with nothing secret",
	}, "\n")
	out := mcp.RedactText(in)
	for _, leak := range []string{"s3cr3tpw", "abcdefghijklmnop1234567890", "hunter2", "abc123def456", strings.Repeat("A", 40), "eyJzdWIi", "MIIB"} {
		require.NotContains(t, out, leak)
	}
	require.Contains(t, out, "plain line with nothing secret")
	require.Contains(t, out, "postgres://app:"+mcp.Redacted+"@db")
}

// ---- OAuth 2.1 flow for connectors

func pkce() (verifier, challenge string) {
	verifier = strings.Repeat("v", 50)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

func noRedirect() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func postJSON(t *testing.T, c *http.Client, u, tok string, body any) (int, map[string]any) {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", u, strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func tokenReq(t *testing.T, e *env, form url.Values) (int, map[string]any) {
	resp, err := http.PostForm(e.srv.URL+"/oauth/token", form)
	require.NoError(t, err)
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestOAuthFlowForConnectors(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	e.project(t, "p")

	// metadata
	get := func(path string) map[string]any {
		resp, err := http.Get(e.srv.URL + path)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, 200, resp.StatusCode)
		var m map[string]any
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&m))
		return m
	}
	prm := get("/.well-known/oauth-protected-resource")
	require.Equal(t, e.srv.URL+"/mcp", prm["resource"])
	require.Equal(t, []any{e.srv.URL}, prm["authorization_servers"])
	require.Equal(t, e.srv.URL+"/mcp", get("/.well-known/oauth-protected-resource/mcp")["resource"])
	asm := get("/.well-known/oauth-authorization-server")
	require.Equal(t, e.srv.URL+"/oauth/register", asm["registration_endpoint"])
	require.Equal(t, []any{"S256"}, asm["code_challenge_methods_supported"])

	// dynamic client registration
	code, _ := postJSON(t, http.DefaultClient, e.srv.URL+"/oauth/register", "", map[string]any{"client_name": "x", "redirect_uris": []string{"javascript:alert(1)"}})
	require.Equal(t, 400, code)
	code, _ = postJSON(t, http.DefaultClient, e.srv.URL+"/oauth/register", "", map[string]any{"client_name": "x", "redirect_uris": []string{"http://evil.example/cb"}})
	require.Equal(t, 400, code)
	redirect := "https://chatgpt.example/callback"
	code, reg := postJSON(t, http.DefaultClient, e.srv.URL+"/oauth/register", "", map[string]any{"client_name": "ChatGPT", "redirect_uris": []string{redirect}})
	require.Equal(t, 201, code)
	cid := reg["client_id"].(string)
	require.Nil(t, reg["client_secret"]) // public client

	// user + dashboard session (login via the Connect API)
	_, err := auth.CreateUser(ctx, e.st, "dev@x.io", "Dev", "password123", v1.Role_ROLE_DEVELOPER)
	require.NoError(t, err)
	as := aetherixv1connect.NewAuthServiceClient(http.DefaultClient, e.srv.URL)
	lg, err := as.Login(ctx, connect.NewRequest(&v1.LoginRequest{Email: "dev@x.io", Password: "password123"}))
	require.NoError(t, err)
	session := lg.Msg.SessionToken

	verifier, challenge := pkce()
	authz := func(q url.Values) *http.Response {
		resp, err := noRedirect().Get(e.srv.URL + "/oauth/authorize?" + q.Encode())
		require.NoError(t, err)
		resp.Body.Close()
		return resp
	}
	q := url.Values{"response_type": {"code"}, "client_id": {cid}, "redirect_uri": {redirect}, "state": {"st8"},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "scope": {"read deploy admin"}}
	// unregistered redirect and missing PKCE are refused
	bad := url.Values{}
	for k, v := range q {
		bad[k] = v
	}
	bad.Set("redirect_uri", "https://evil.example/cb")
	require.Equal(t, 400, authz(bad).StatusCode)
	bad.Set("redirect_uri", redirect)
	bad.Del("code_challenge")
	require.Equal(t, 302, authz(bad).StatusCode)
	require.Contains(t, authz(bad).Header.Get("Location"), "error=invalid_request")

	resp := authz(q)
	require.Equal(t, 302, resp.StatusCode)
	loc, _ := url.Parse(resp.Header.Get("Location"))
	require.Equal(t, "/oauth/consent", loc.Path) // rendered by the dashboard
	reqID := loc.Query().Get("request")
	require.NotEmpty(t, reqID)

	// consent API needs a dashboard session
	r, _ := http.NewRequest("GET", e.srv.URL+"/oauth/api/consent?request="+reqID, nil)
	resp2, _ := http.DefaultClient.Do(r)
	require.Equal(t, 401, resp2.StatusCode)
	r.Header.Set("Authorization", "Bearer "+session)
	resp2, err = http.DefaultClient.Do(r)
	require.NoError(t, err)
	var info map[string]any
	require.NoError(t, json.NewDecoder(resp2.Body).Decode(&info))
	require.Equal(t, "ChatGPT", info["client_name"])
	require.Equal(t, []any{"read", "deploy"}, info["allowed_scopes"]) // developer role: no admin

	code, dec := postJSON(t, http.DefaultClient, e.srv.URL+"/oauth/api/consent", session, map[string]any{"request": reqID, "approve": true})
	require.Equal(t, 200, code)
	ru, _ := url.Parse(dec["redirect_url"].(string))
	require.Equal(t, "chatgpt.example", ru.Host)
	require.Equal(t, "st8", ru.Query().Get("state"))
	authCode := ru.Query().Get("code")
	require.NotEmpty(t, authCode)
	// request is single use
	code, _ = postJSON(t, http.DefaultClient, e.srv.URL+"/oauth/api/consent", session, map[string]any{"request": reqID, "approve": true})
	require.Equal(t, 404, code)

	// token exchange: wrong verifier fails, right one works once
	form := url.Values{"grant_type": {"authorization_code"}, "code": {authCode}, "client_id": {cid}, "redirect_uri": {redirect}, "code_verifier": {strings.Repeat("w", 50)}}
	code, _ = tokenReq(t, e, form)
	require.Equal(t, 400, code)
	form.Set("code_verifier", verifier)
	code, tk := tokenReq(t, e, form)
	require.Equal(t, 200, code, tk)
	access := tk["access_token"].(string)
	require.Equal(t, "Bearer", tk["token_type"])
	require.Equal(t, "read deploy", tk["scope"])
	// replaying the code fails and revokes what it produced
	code, _ = tokenReq(t, e, form)
	require.Equal(t, 400, code)
	req, _ := http.NewRequest("POST", e.srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	r2, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, 401, r2.StatusCode, "code replay must revoke the grant it created")
}

func TestOAuthFlowEndToEnd(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	e.project(t, "p")
	redirect := "http://127.0.0.1:33333/cb"
	_, reg := postJSON(t, http.DefaultClient, e.srv.URL+"/oauth/register", "", map[string]any{"client_name": "Claude", "redirect_uris": []string{redirect}})
	cid := reg["client_id"].(string)
	_, err := auth.CreateUser(ctx, e.st, "o@x.io", "", "password123", v1.Role_ROLE_OWNER)
	require.NoError(t, err)
	lg, err := aetherixv1connect.NewAuthServiceClient(http.DefaultClient, e.srv.URL).Login(ctx, connect.NewRequest(&v1.LoginRequest{Email: "o@x.io", Password: "password123"}))
	require.NoError(t, err)
	session := lg.Msg.SessionToken

	verifier, challenge := pkce()
	resp, err := noRedirect().Get(e.srv.URL + "/oauth/authorize?" + url.Values{"response_type": {"code"}, "client_id": {cid}, "redirect_uri": {redirect},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "scope": {"read deploy"}}.Encode())
	require.NoError(t, err)
	loc, _ := url.Parse(resp.Header.Get("Location"))
	// the user narrows the scopes on the consent screen
	_, dec := postJSON(t, http.DefaultClient, e.srv.URL+"/oauth/api/consent", session, map[string]any{"request": loc.Query().Get("request"), "approve": true, "scopes": []string{"read"}})
	ru, _ := url.Parse(dec["redirect_url"].(string))
	code, tk := tokenReq(t, e, url.Values{"grant_type": {"authorization_code"}, "code": {ru.Query().Get("code")}, "client_id": {cid},
		"redirect_uri": {redirect}, "code_verifier": {verifier}})
	require.Equal(t, 200, code, tk)
	access, refresh := tk["access_token"].(string), tk["refresh_token"].(string)
	require.Equal(t, "read", tk["scope"])

	// the MCP SDK client works with the OAuth access token
	cs := e.connect(t, access)
	tools, err := cs.ListTools(ctx, nil)
	require.NoError(t, err)
	for _, x := range tools.Tools {
		require.True(t, x.Annotations.ReadOnlyHint, "read-only grant must only see read tools: %s", x.Name)
	}
	_, res := call(t, cs, "list_projects", nil)
	require.False(t, res.IsError, text(res))
	_, res = call(t, cs, "create_service", map[string]any{"project": "p", "name": "x", "image": "nginx"})
	require.True(t, res.IsError)
	// audit names the OAuth client
	es, _ := auth.ListAudit(ctx, e.st, "", "mcp.list_projects", 10)
	require.NotEmpty(t, es)
	require.Equal(t, "Claude", es[0].Client)

	// OAuth tokens are not accepted on the plain API (only via MCP)
	_, err = aetherixv1connect.NewProjectServiceClient(http.DefaultClient, e.srv.URL).ListProjects(ctx, func() *connect.Request[v1.ListProjectsRequest] {
		r := connect.NewRequest(&v1.ListProjectsRequest{})
		r.Header().Set("Authorization", "Bearer "+access)
		return r
	}())
	require.Equal(t, connect.CodePermissionDenied, connect.CodeOf(err))

	// refresh rotation
	code, tk2 := tokenReq(t, e, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {cid}})
	require.Equal(t, 200, code, tk2)
	refresh2, access2 := tk2["refresh_token"].(string), tk2["access_token"].(string)
	require.NotEqual(t, refresh, refresh2)
	// a different client cannot use it
	_, reg2 := postJSON(t, http.DefaultClient, e.srv.URL+"/oauth/register", "", map[string]any{"client_name": "Other", "redirect_uris": []string{redirect}})
	code, _ = tokenReq(t, e, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh2}, "client_id": {reg2["client_id"].(string)}})
	require.Equal(t, 400, code)
	// reuse of the old refresh token => theft: whole grant revoked
	code, _ = tokenReq(t, e, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {cid}})
	require.Equal(t, 400, code)
	code, _ = tokenReq(t, e, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh2}, "client_id": {cid}})
	require.Equal(t, 400, code, "reuse detection must revoke the grant")
	req, _ := http.NewRequest("POST", e.srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Authorization", "Bearer "+access2)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	r2, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, 401, r2.StatusCode)
}

func TestOAuthRevokeAndGrantList(t *testing.T) {
	e := newEnv(t, nil)
	ctx := context.Background()
	redirect := "https://app.example/cb"
	_, reg := postJSON(t, http.DefaultClient, e.srv.URL+"/oauth/register", "", map[string]any{"client_name": "ChatGPT", "redirect_uris": []string{redirect}})
	cid := reg["client_id"].(string)
	_, err := auth.CreateUser(ctx, e.st, "o@x.io", "", "password123", v1.Role_ROLE_OWNER)
	require.NoError(t, err)
	as := aetherixv1connect.NewAuthServiceClient(http.DefaultClient, e.srv.URL)
	lg, _ := as.Login(ctx, connect.NewRequest(&v1.LoginRequest{Email: "o@x.io", Password: "password123"}))
	session := lg.Msg.SessionToken
	verifier, challenge := pkce()
	resp, _ := noRedirect().Get(e.srv.URL + "/oauth/authorize?" + url.Values{"response_type": {"code"}, "client_id": {cid}, "redirect_uri": {redirect},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "scope": {"read"}}.Encode())
	loc, _ := url.Parse(resp.Header.Get("Location"))
	_, dec := postJSON(t, http.DefaultClient, e.srv.URL+"/oauth/api/consent", session, map[string]any{"request": loc.Query().Get("request"), "approve": true})
	ru, _ := url.Parse(dec["redirect_url"].(string))
	_, tk := tokenReq(t, e, url.Values{"grant_type": {"authorization_code"}, "code": {ru.Query().Get("code")}, "client_id": {cid}, "code_verifier": {verifier}})
	access := tk["access_token"].(string)

	// the dashboard lists the grant, then the user revokes it
	sreq := func() *connect.Request[v1.ListOAuthGrantsRequest] {
		r := connect.NewRequest(&v1.ListOAuthGrantsRequest{})
		r.Header().Set("Authorization", "Bearer "+session)
		return r
	}
	gl, err := as.ListOAuthGrants(ctx, sreq())
	require.NoError(t, err)
	require.Len(t, gl.Msg.Grants, 1)
	require.Equal(t, "ChatGPT", gl.Msg.Grants[0].ClientName)
	_, res := call(t, e.connect(t, access), "list_projects", nil)
	require.False(t, res.IsError)
	rr := connect.NewRequest(&v1.RevokeOAuthGrantRequest{GrantId: gl.Msg.Grants[0].Id})
	rr.Header().Set("Authorization", "Bearer "+session)
	_, err = as.RevokeOAuthGrant(ctx, rr)
	require.NoError(t, err)
	gl, _ = as.ListOAuthGrants(ctx, sreq())
	require.Empty(t, gl.Msg.Grants)
	req, _ := http.NewRequest("POST", e.srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	r2, _ := http.DefaultClient.Do(req)
	require.Equal(t, 401, r2.StatusCode)

	// RFC 7009 revocation endpoint is always 200
	rv, err := http.PostForm(e.srv.URL+"/oauth/revoke", url.Values{"token": {"axr_unknown.x"}, "client_id": {cid}})
	require.NoError(t, err)
	require.Equal(t, 200, rv.StatusCode)
}
