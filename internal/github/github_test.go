package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/build"
	"github.com/aetherix/aetherixcloud/internal/secrets"
	"github.com/aetherix/aetherixcloud/internal/store"
	"github.com/stretchr/testify/require"
)

// ---- fake GitHub

type fakeGH struct {
	t      *testing.T
	srv    *httptest.Server
	key    *rsa.PrivateKey
	pemStr string

	mu       sync.Mutex
	calls    []string
	bodies   map[string]string
	statuses []Status
	checks   []CheckRun
	comments []string
	tokens   int
	hookURL  string
}

func newFakeGH(t *testing.T) *fakeGH {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	f := &fakeGH{t: t, key: k, bodies: map[string]string{},
		pemStr: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGH) verifyJWT(h string) bool {
	tok := strings.TrimPrefix(h, "Bearer ")
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return false
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	d := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(&f.key.PublicKey, crypto.SHA256, d[:], sig) != nil {
		return false
	}
	var claims struct {
		Iss string `json:"iss"`
		Exp int64  `json:"exp"`
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	_ = json.Unmarshal(raw, &claims)
	return claims.Iss == "4242" && claims.Exp > time.Now().Unix()
}

func (f *fakeGH) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	f.bodies[r.Method+" "+r.URL.Path] = string(body)
	f.mu.Unlock()
	js := func(v any) { w.Header().Set("Content-Type", "application/json"); _ = json.NewEncoder(w).Encode(v) }
	auth := r.Header.Get("Authorization")
	switch {
	case r.URL.Path == "/app-manifests/CODE/conversions":
		js(map[string]any{"id": 4242, "slug": "ax-test", "html_url": "https://github.com/apps/ax-test", "client_id": "cid",
			"client_secret": "csecret", "webhook_secret": "whsecret", "pem": f.pemStr})
	case strings.HasPrefix(r.URL.Path, "/app/installations/") && strings.HasSuffix(r.URL.Path, "/access_tokens"):
		if !f.verifyJWT(auth) {
			http.Error(w, `{"message":"bad jwt"}`, 401)
			return
		}
		f.mu.Lock()
		f.tokens++
		f.mu.Unlock()
		w.WriteHeader(201)
		js(map[string]any{"token": "ghs_installtoken", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	case r.URL.Path == "/app/installations":
		if !f.verifyJWT(auth) {
			http.Error(w, `{"message":"bad jwt"}`, 401)
			return
		}
		js([]map[string]any{{"id": 7, "account": map[string]any{"login": "octo", "type": "Organization"}, "created_at": "2024-01-02T03:04:05Z"}})
	case r.URL.Path == "/app/hook/config" && r.Method == "PATCH":
		if !f.verifyJWT(auth) {
			http.Error(w, `{"message":"bad jwt"}`, 401)
			return
		}
		var b map[string]string
		_ = json.Unmarshal(body, &b)
		f.mu.Lock()
		f.hookURL = b["url"]
		f.mu.Unlock()
		js(b)
	default:
		if auth != "Bearer ghs_installtoken" {
			http.Error(w, `{"message":"bad credentials"}`, 401)
			return
		}
		switch {
		case r.URL.Path == "/installation/repositories":
			js(map[string]any{"repositories": []map[string]any{
				{"full_name": "octo/web", "default_branch": "main", "private": true},
				{"full_name": "octo/api", "default_branch": "dev"}}})
		case r.URL.Path == "/repos/octo/web/branches":
			js([]map[string]any{{"name": "main"}, {"name": "dev"}})
		case strings.HasPrefix(r.URL.Path, "/repos/octo/web/commits/"):
			js(map[string]any{"sha": "feedfacefeedfacefeedfacefeedfacefeedface", "commit": map[string]any{"message": "fix: thing\n\nbody", "author": map[string]any{"name": "Ada"}}})
		case strings.Contains(r.URL.Path, "/statuses/"):
			var s Status
			_ = json.Unmarshal(body, &s)
			f.mu.Lock()
			f.statuses = append(f.statuses, s)
			f.mu.Unlock()
			w.WriteHeader(201)
			js(map[string]any{})
		case strings.HasSuffix(r.URL.Path, "/check-runs") && r.Method == "POST":
			var c CheckRun
			_ = json.Unmarshal(body, &c)
			f.mu.Lock()
			f.checks = append(f.checks, c)
			f.mu.Unlock()
			w.WriteHeader(201)
			js(map[string]any{"id": 99})
		case strings.Contains(r.URL.Path, "/check-runs/") && r.Method == "PATCH":
			var c CheckRun
			_ = json.Unmarshal(body, &c)
			f.mu.Lock()
			f.checks = append(f.checks, c)
			f.mu.Unlock()
			js(map[string]any{})
		case strings.HasSuffix(r.URL.Path, "/comments"):
			var b map[string]string
			_ = json.Unmarshal(body, &b)
			f.mu.Lock()
			f.comments = append(f.comments, b["body"])
			f.mu.Unlock()
			w.WriteHeader(201)
			js(map[string]any{})
		default:
			http.NotFound(w, r)
		}
	}
}

func (f *fakeGH) called(s string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == s {
			return true
		}
	}
	return false
}

// ---- module fixture

func newModule(t *testing.T) (*Module, *fakeGH, store.Store) {
	t.Helper()
	st, err := store.OpenSQLite(":memory:")
	require.NoError(t, err)
	require.NoError(t, st.Migrate(context.Background()))
	t.Cleanup(func() { st.Close() })
	key, err := secrets.NewKey()
	require.NoError(t, err)
	gh := newFakeGH(t)
	m := &Module{Store: st, Key: key, Self: "m1", APIBase: gh.srv.URL, WebBase: "https://github.example",
		DashboardURL:      func(context.Context) string { return "https://ax.example.com" },
		ManifestStateWait: -1} // no Corrosion-lag poll in unit tests
	return m, gh, st
}

func setupApp(t *testing.T, m *Module) {
	t.Helper()
	_, _, state, err := m.BeginManifest(context.Background(), "https://ax.example.com", "")
	require.NoError(t, err)
	_, _, err = m.CompleteManifest(context.Background(), "CODE", state)
	require.NoError(t, err)
}

func putSvc(t *testing.T, st store.Store, id, name, repo, branch string, previews bool, build *v1.BuildSpec) *v1.Service {
	t.Helper()
	svc := &v1.Service{Id: id, ProjectId: "p1", EnvironmentId: "e1", Name: name, Spec: &v1.ServiceSpec{
		Replicas: 3, Build: build,
		Ports:  []*v1.ServicePort{{ContainerPort: 8080, Mode: "https"}},
		Source: &v1.ServiceSource{Github: &v1.GitHubSource{Repo: repo, Branch: branch, InstallationId: 7, PrPreviews: previews}}}}
	require.NoError(t, store.PutService(context.Background(), st, svc))
	return svc
}

// ---- manifest flow

func TestManifestFlowFull(t *testing.T) {
	m, gh, st := newModule(t)
	ctx := context.Background()
	svc := &Service{M: m}

	resp, err := svc.BeginManifestSetup(ctx, connect.NewRequest(&v1.BeginManifestSetupRequest{BaseUrl: "https://ax.example.com/", Organization: "octo"}))
	require.NoError(t, err)
	require.Equal(t, "https://github.example/organizations/octo/settings/apps/new?state="+resp.Msg.State, resp.Msg.PostUrl)
	var mf struct {
		Name  string               `json:"name"`
		URL   string               `json:"url"`
		Hook  struct{ URL string } `json:"hook_attributes"`
		Redir string               `json:"redirect_url"`
		Perms map[string]string    `json:"default_permissions"`
		Evts  []string             `json:"default_events"`
	}
	require.NoError(t, json.Unmarshal([]byte(resp.Msg.ManifestJson), &mf))
	require.Equal(t, "https://ax.example.com/api/github/webhook", mf.Hook.URL)
	require.Equal(t, "https://ax.example.com/api/github/manifest/callback", mf.Redir)
	require.Equal(t, "write", mf.Perms["statuses"])
	require.Equal(t, "read", mf.Perms["contents"])
	require.ElementsMatch(t, []string{"push", "pull_request"}, mf.Evts)
	require.LessOrEqual(t, len(mf.Name), 34)

	// wrong state
	_, err = svc.CompleteManifestSetup(ctx, connect.NewRequest(&v1.CompleteManifestSetupRequest{Code: "CODE", State: "bogus"}))
	require.Error(t, err)

	done, err := svc.CompleteManifestSetup(ctx, connect.NewRequest(&v1.CompleteManifestSetupRequest{Code: "CODE", State: resp.Msg.State}))
	require.NoError(t, err)
	require.True(t, done.Msg.App.Configured)
	require.EqualValues(t, 4242, done.Msg.App.AppId)
	require.Equal(t, "ax-test", done.Msg.App.Slug)
	require.Equal(t, "https://ax.example.com/api/github/webhook", done.Msg.App.WebhookUrl)

	// Retry is idempotent (GitHub may re-hit the callback).
	again, err := svc.CompleteManifestSetup(ctx, connect.NewRequest(&v1.CompleteManifestSetupRequest{Code: "CODE", State: resp.Msg.State}))
	require.NoError(t, err)
	require.EqualValues(t, 4242, again.Msg.App.AppId)

	// credentials are encrypted at rest
	rec, err := st.Get(ctx, store.TableSettings, appSettingID)
	require.NoError(t, err)
	for _, secret := range []string{"whsecret", "csecret", "BEGIN RSA PRIVATE KEY"} {
		require.NotContains(t, string(rec.Data), secret)
	}
	require.Contains(t, string(rec.Data), "sb1:")
	ws, err := m.WebhookSecret(ctx)
	require.NoError(t, err)
	require.Equal(t, "whsecret", string(ws))

	// installations were synced from the API (JWT verified by the fake)
	ins, err := svc.ListInstallations(ctx, connect.NewRequest(&v1.ListInstallationsRequest{}))
	require.NoError(t, err)
	require.Len(t, ins.Msg.Installations, 1)
	require.Equal(t, "octo", ins.Msg.Installations[0].Account)
	require.True(t, gh.called("GET /app/installations"))
}

func TestExpiredManifestState(t *testing.T) {
	m, _, _ := newModule(t)
	_, _, state, err := m.BeginManifest(context.Background(), "https://x", "")
	require.NoError(t, err)
	m.Now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	_, _, err = m.CompleteManifest(context.Background(), "CODE", state)
	require.ErrorContains(t, err, "expired")
}

func TestManifestExchangeFailureKeepsState(t *testing.T) {
	m, _, _ := newModule(t)
	ctx := context.Background()
	_, _, state, err := m.BeginManifest(ctx, "https://ax.example.com", "")
	require.NoError(t, err)
	_, _, err = m.CompleteManifest(ctx, "BAD", state)
	require.Error(t, err)
	// State must still be pending so a fresh Begin is not required after a
	// failed exchange (old code deleted first and caused "already used").
	cfg, _, err := m.CompleteManifest(ctx, "CODE", state)
	require.NoError(t, err)
	require.EqualValues(t, 4242, cfg.AppID)
}

func TestManifestCallbackIdempotentHTTP(t *testing.T) {
	m, _, _ := newModule(t)
	_, _, state, err := m.BeginManifest(context.Background(), "https://ax.example.com", "")
	require.NoError(t, err)
	h := m.Handlers(nil)[ManifestCallbackPath]
	req := httptest.NewRequest(http.MethodGet, ManifestCallbackPath+"?code=CODE&state="+state, nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	require.Equal(t, http.StatusFound, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), "/apps/ax-test/installations/new")

	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, req)
	require.Equal(t, http.StatusFound, rr2.Code, "retried callback must not 400")
	require.Contains(t, rr2.Header().Get("Location"), "/apps/ax-test/installations/new")
}

func TestManifestStateAppearsAfterLag(t *testing.T) {
	m, _, st := newModule(t)
	m.ManifestStateWait = 2 * time.Second
	state := "laggedtoken"
	key := manifestPrefix + state
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = store.PutJSON(context.Background(), st, store.TableSettings, key, nil,
			manifestState{Expires: time.Now().Add(time.Hour).Unix(), BaseURL: "https://ax.example.com"})
	}()
	cfg, base, err := m.CompleteManifest(context.Background(), "CODE", state)
	require.NoError(t, err)
	require.Equal(t, "https://ax.example.com", base)
	require.EqualValues(t, 4242, cfg.AppID)
}

// ---- API client + service RPCs

func TestRepoBranchListingAndSimulatePush(t *testing.T) {
	m, gh, st := newModule(t)
	ctx := context.Background()
	setupApp(t, m)
	svc := &Service{M: m}

	repos, err := svc.ListRepositories(ctx, connect.NewRequest(&v1.ListRepositoriesRequest{InstallationId: 7, Query: "WEB"}))
	require.NoError(t, err)
	require.Len(t, repos.Msg.Repositories, 1)
	require.Equal(t, "octo/web", repos.Msg.Repositories[0].FullName)
	require.True(t, repos.Msg.Repositories[0].Private)
	require.EqualValues(t, 7, repos.Msg.Repositories[0].InstallationId)

	br, err := svc.ListBranches(ctx, connect.NewRequest(&v1.ListBranchesRequest{InstallationId: 7, Repo: "octo/web"}))
	require.NoError(t, err)
	require.Equal(t, []string{"main", "dev"}, br.Msg.Branches)

	s := putSvc(t, st, "svc1", "web", "octo/web", "main", false, nil)
	sp, err := svc.SimulatePush(ctx, connect.NewRequest(&v1.SimulatePushRequest{ServiceId: s.Id}))
	require.NoError(t, err)
	require.Equal(t, "feedfacefeedfacefeedfacefeedfacefeedface", sp.Msg.CommitSha)
	b, err := build.GetBuild(ctx, st, sp.Msg.BuildId)
	require.NoError(t, err)
	require.Equal(t, build.StatusQueued, b.Status)
	require.Equal(t, "refs/heads/main", b.Ref)
	job, err := build.LoadJob(ctx, st, b.Id)
	require.NoError(t, err)
	require.Equal(t, "fix: thing", job.Message)
	require.Equal(t, "Ada", job.Author)
	require.EqualValues(t, 7, job.InstallationID)
	// installation token cached across calls: one exchange for all of the above
	gh.mu.Lock()
	require.Equal(t, 1, gh.tokens)
	gh.mu.Unlock()

	_, err = svc.SimulatePush(ctx, connect.NewRequest(&v1.SimulatePushRequest{ServiceId: "ghost"}))
	require.Equal(t, connect.CodeNotFound, connect.CodeOf(err))

	// SetWebhookUrl updates GitHub and the stored app
	_, err = svc.SetWebhookUrl(ctx, connect.NewRequest(&v1.SetWebhookUrlRequest{Url: "https://abc.trycloudflare.com/api/github/webhook"}))
	require.NoError(t, err)
	require.Equal(t, "https://abc.trycloudflare.com/api/github/webhook", gh.hookURL)
	app, _ := svc.GetApp(ctx, connect.NewRequest(&v1.GetAppRequest{}))
	require.Equal(t, gh.hookURL, app.Msg.WebhookUrl)
	_, err = svc.SetWebhookUrl(ctx, connect.NewRequest(&v1.SetWebhookUrlRequest{Url: "ftp://x"}))
	require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))
}

func TestNotConfigured(t *testing.T) {
	m, _, _ := newModule(t)
	_, err := (&Service{M: m}).ListRepositories(context.Background(), connect.NewRequest(&v1.ListRepositoriesRequest{}))
	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err))
}

// ---- webhook

func hook(t *testing.T, m *Module, delivery, event, secret string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(payload)
	r := httptest.NewRequest("POST", WebhookPath, bytes.NewReader(body))
	r.Header.Set("X-GitHub-Event", event)
	r.Header.Set("X-GitHub-Delivery", delivery)
	r.Header.Set("X-Hub-Signature-256", Sign([]byte(secret), body))
	w := httptest.NewRecorder()
	m.WebhookHandler().ServeHTTP(w, r)
	return w
}

func queued(t *testing.T, st store.Store) []*v1.Build {
	bs, err := store.ListProto[v1.Build](context.Background(), st, store.TableBuilds, store.Query{Where: map[string]string{"status": "queued"}})
	require.NoError(t, err)
	return bs
}

func TestSignature(t *testing.T) {
	body := []byte(`{"a":1}`)
	good := Sign([]byte("s"), body)
	require.True(t, VerifySignature([]byte("s"), body, good))
	require.False(t, VerifySignature([]byte("t"), body, good))
	require.False(t, VerifySignature([]byte("s"), []byte(`{"a":2}`), good))
	require.False(t, VerifySignature([]byte("s"), body, strings.TrimPrefix(good, "sha256=")))
	require.False(t, VerifySignature([]byte("s"), body, "sha256=zz"))
	require.False(t, VerifySignature([]byte("s"), body, ""))
}

func TestWebhookAuthAndNotConfigured(t *testing.T) {
	m, _, _ := newModule(t)
	r := httptest.NewRequest("POST", WebhookPath, strings.NewReader("{}"))
	w := httptest.NewRecorder()
	m.WebhookHandler().ServeHTTP(w, r)
	require.Equal(t, http.StatusServiceUnavailable, w.Code)

	setupApp(t, m)
	r = httptest.NewRequest("POST", WebhookPath, strings.NewReader("{}"))
	r.Header.Set("X-Hub-Signature-256", Sign([]byte("wrong"), []byte("{}")))
	w = httptest.NewRecorder()
	m.WebhookHandler().ServeHTTP(w, r)
	require.Equal(t, http.StatusUnauthorized, w.Code)

	w = hook(t, m, "d0", "ping", "whsecret", map[string]any{"zen": "x"})
	require.Equal(t, http.StatusAccepted, w.Code)
	w = httptest.NewRecorder()
	m.WebhookHandler().ServeHTTP(w, httptest.NewRequest("GET", WebhookPath, nil))
	require.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

func pushPayload(branch, after string, files ...string) map[string]any {
	return map[string]any{
		"ref": "refs/heads/" + branch, "after": after,
		"repository":   map[string]any{"full_name": "Octo/Web", "default_branch": "main"},
		"installation": map[string]any{"id": 7},
		"head_commit":  map[string]any{"message": "ship it\n\nmore", "author": map[string]any{"name": "Ada"}},
		"commits":      []any{map[string]any{"modified": files}},
	}
}

func TestPushEnqueuesBuildsWithDedup(t *testing.T) {
	m, _, st := newModule(t)
	setupApp(t, m)
	ctx := context.Background()
	putSvc(t, st, "s-main", "web", "octo/web", "main", false, nil)
	putSvc(t, st, "s-dev", "web-dev", "octo/web", "dev", false, nil)
	putSvc(t, st, "s-other", "other", "octo/other", "main", false, nil)

	w := hook(t, m, "del-1", "push", "whsecret", pushPayload("main", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "main.go"))
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	bs := queued(t, st)
	require.Len(t, bs, 1)
	require.Equal(t, "s-main", bs[0].ServiceId)
	require.Equal(t, "refs/heads/main", bs[0].Ref)
	require.Equal(t, build.DeterministicBuildID("del-1/s-main", "s-main"), bs[0].Id)
	job, err := build.LoadJob(ctx, st, bs[0].Id)
	require.NoError(t, err)
	require.Equal(t, "ship it", job.Message)
	require.Equal(t, "Ada", job.Author)
	require.Equal(t, "del-1", job.Delivery)
	require.EqualValues(t, 7, job.InstallationID)
	require.Equal(t, "Octo/Web", job.Repo)

	// redelivery of the same X-GitHub-Delivery is a no-op
	w = hook(t, m, "del-1", "push", "whsecret", pushPayload("main", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "main.go"))
	require.Contains(t, w.Body.String(), `"duplicate"`)
	require.Len(t, queued(t, st), 1)

	// even if the dedup row were missing (two nodes racing), the deterministic ID collapses it
	require.NoError(t, st.Delete(ctx, store.TableSettings, deliveryPrefix+"del-1"))
	hook(t, m, "del-1", "push", "whsecret", pushPayload("main", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "main.go"))
	require.Len(t, queued(t, st), 1)

	// a new delivery to the other branch hits the other service
	hook(t, m, "del-2", "push", "whsecret", pushPayload("dev", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "x"))
	require.Len(t, queued(t, st), 2)

	// branch deletions and tags are ignored
	p := pushPayload("main", "0000000000000000000000000000000000000000")
	p["deleted"] = true
	w = hook(t, m, "del-3", "push", "whsecret", p)
	require.Contains(t, w.Body.String(), "ignored")
	p = pushPayload("main", "cccccccccccccccccccccccccccccccccccccccc")
	p["ref"] = "refs/tags/v1"
	hook(t, m, "del-4", "push", "whsecret", p)
	require.Len(t, queued(t, st), 2)
}

func TestPushWatchPaths(t *testing.T) {
	m, _, st := newModule(t)
	setupApp(t, m)
	putSvc(t, st, "web", "web", "octo/web", "main", false, &v1.BuildSpec{Context: "apps/web", WatchPaths: []string{"packages/ui/**"}})
	putSvc(t, st, "api", "api", "octo/web", "main", false, &v1.BuildSpec{Context: "apps/api"}) // root dir implies watch

	hook(t, m, "w1", "push", "whsecret", pushPayload("main", "dddddddddddddddddddddddddddddddddddddddd", "README.md"))
	require.Len(t, queued(t, st), 0)
	hook(t, m, "w2", "push", "whsecret", pushPayload("main", "dddddddddddddddddddddddddddddddddddddddd", "packages/ui/button.tsx"))
	bs := queued(t, st)
	require.Len(t, bs, 1)
	require.Equal(t, "web", bs[0].ServiceId)
	hook(t, m, "w3", "push", "whsecret", pushPayload("main", "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", "apps/api/main.go", "apps/web/x.ts"))
	require.Len(t, queued(t, st), 2) // earlier web build + api via its root dir (web is not rebuilt: its watch is packages/ui only)
}

func TestWatchPathMatching(t *testing.T) {
	cases := []struct {
		pat, file string
		want      bool
	}{
		{"apps/web", "apps/web/src/a.ts", true},
		{"apps/web/", "apps/web/a.ts", true},
		{"apps/web", "apps/webx/a.ts", false},
		{"apps/web/**", "apps/web/a/b/c.ts", true},
		{"**/*.md", "docs/x/y.md", true},
		{"**/*.md", "x.md", true},
		{"*.md", "docs/x.md", false},
		{"*.md", "x.md", true},
		{"src/*/index.ts", "src/a/index.ts", true},
		{"src/*/index.ts", "src/a/b/index.ts", false},
		{"/lib/?.go", "lib/a.go", true},
		{"packages/(ui)/x", "packages/(ui)/x", true},
	}
	for _, c := range cases {
		require.Equal(t, c.want, MatchWatchPath(c.pat, c.file), "%s vs %s", c.pat, c.file)
	}
	require.True(t, ShouldBuild(nil, nil))
	require.True(t, ShouldBuild(&v1.BuildSpec{Context: "a"}, nil), "unknown file list always builds")
	require.True(t, ShouldBuild(&v1.BuildSpec{}, []string{"anything"}))
	require.False(t, ShouldBuild(&v1.BuildSpec{Context: "./a/"}, []string{"b/x"}))
}

func TestInstallationEvents(t *testing.T) {
	m, _, st := newModule(t)
	setupApp(t, m)
	ctx := context.Background()
	w := hook(t, m, "i1", "installation", "whsecret", map[string]any{"action": "created",
		"installation": map[string]any{"id": 55, "account": map[string]any{"login": "me", "type": "User"}}})
	require.Equal(t, http.StatusAccepted, w.Code)
	ins, _ := listInstallations(ctx, st) // 7 came from the setup sync
	require.Len(t, ins, 2)
	hook(t, m, "i2", "installation", "whsecret", map[string]any{"action": "deleted", "installation": map[string]any{"id": 55}})
	ins, _ = listInstallations(ctx, st)
	require.Len(t, ins, 1)
	require.EqualValues(t, 7, ins[0].Id)
}

func TestPruneDeliveries(t *testing.T) {
	m, _, st := newModule(t)
	ctx := context.Background()
	require.NoError(t, store.PutJSON(ctx, st, store.TableSettings, deliveryPrefix+"old", nil, map[string]int{"a": 1}))
	require.NoError(t, store.PutJSON(ctx, st, store.TableSettings, "other", nil, map[string]int{"a": 1}))
	m.Now = func() time.Time { return time.Now().Add(8 * 24 * time.Hour) }
	n, err := m.PruneDeliveries(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	_, err = st.Get(ctx, store.TableSettings, "other")
	require.NoError(t, err)
}

// ---- PR previews

func prPayload(action string, n int, headRepo string) map[string]any {
	return map[string]any{"action": action, "number": n,
		"repository":   map[string]any{"full_name": "octo/web"},
		"installation": map[string]any{"id": 7},
		"pull_request": map[string]any{"title": "Add feature", "user": map[string]any{"login": "ada"},
			"head": map[string]any{"sha": "1111111111111111111111111111111111111111", "ref": "feature-x", "repo": map[string]any{"full_name": headRepo}}}}
}

func TestPullRequestPreviewLifecycle(t *testing.T) {
	m, gh, st := newModule(t)
	setupApp(t, m)
	ctx := context.Background()
	triggered := 0
	m.Trigger = func() { triggered++ }
	require.NoError(t, store.PutCluster(ctx, st, &v1.Cluster{Id: "c", Name: "c", Domain: "apps.example.com"}))
	parent := putSvc(t, st, "web", "web", "octo/web", "main", true, nil)
	putSvc(t, st, "nopreview", "docs", "octo/web", "main", false, nil)
	require.NoError(t, store.PutProto(ctx, st, store.TableVariables, "v-shared",
		map[string]string{"environment_id": "e1", "service_id": "", "key": "SHARED"}, &v1.Variable{Id: "v-shared", EnvironmentId: "e1", Key: "SHARED", Value: "1"}))
	require.NoError(t, store.PutProto(ctx, st, store.TableVariables, "v-svc",
		map[string]string{"environment_id": "e1", "service_id": "web", "key": "SECRET"}, &v1.Variable{Id: "v-svc", EnvironmentId: "e1", ServiceId: "web", Key: "SECRET", Value: "sb1:xx", Secret: true}))
	require.NoError(t, store.PutProto(ctx, st, store.TableVariables, "v-other",
		map[string]string{"environment_id": "e1", "service_id": "docs", "key": "NOPE"}, &v1.Variable{Id: "v-other", EnvironmentId: "e1", ServiceId: "docs", Key: "NOPE", Value: "x"}))

	// fork PRs never get a preview
	w := hook(t, m, "pr0", "pull_request", "whsecret", prPayload("opened", 5, "evil/web"))
	require.Contains(t, w.Body.String(), "fork")
	require.Empty(t, queued(t, st))

	w = hook(t, m, "pr1", "pull_request", "whsecret", prPayload("opened", 12, "octo/web"))
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	bs := queued(t, st)
	require.Len(t, bs, 1)
	pvID := bs[0].ServiceId
	require.NotEqual(t, parent.Id, pvID)
	require.Equal(t, "refs/pull/12/head", bs[0].Ref)
	require.Equal(t, "1111111111111111111111111111111111111111", bs[0].CommitSha)

	pv, err := store.GetService(ctx, st, pvID)
	require.NoError(t, err)
	require.Equal(t, "web", pv.Name)
	require.Equal(t, "p1", pv.ProjectId)
	require.EqualValues(t, 1, pv.Spec.Replicas, "previews are scaled down")
	require.Equal(t, "web", pv.Spec.Labels[LabelPreviewOf])
	require.Equal(t, "12", pv.Spec.Labels[LabelPreviewPR])
	require.Equal(t, "feature-x", pv.Spec.Source.Github.Branch)
	require.False(t, pv.Spec.Source.Github.PrPreviews)
	require.Equal(t, "pr-12-web.apps.example.com", pv.Spec.Ports[0].Hostname)
	require.Equal(t, int32(8080), pv.Spec.Ports[0].ContainerPort)
	env, err := store.GetProto[v1.Environment](ctx, st, store.TableEnvironments, pv.EnvironmentId)
	require.NoError(t, err)
	require.True(t, env.Ephemeral)
	require.Equal(t, "pr-12", env.Name)
	// the parent service was not modified
	p2, _ := store.GetService(ctx, st, "web")
	require.EqualValues(t, 3, p2.Spec.Replicas)
	require.Empty(t, p2.Spec.Ports[0].Hostname)

	// variables copied (shared + service), the unrelated service's one is not
	vars, _ := store.ListProto[v1.Variable](ctx, st, store.TableVariables, store.Query{Where: map[string]string{"environment_id": env.Id}})
	got := map[string]*v1.Variable{}
	for _, v := range vars {
		got[v.Key] = v
	}
	require.Len(t, got, 2)
	require.Equal(t, "", got["SHARED"].ServiceId)
	require.Equal(t, pvID, got["SECRET"].ServiceId)
	require.True(t, got["SECRET"].Secret)

	// PR comment with the preview URL
	require.Eventually(t, func() bool { gh.mu.Lock(); defer gh.mu.Unlock(); return len(gh.comments) == 1 }, time.Second, 10*time.Millisecond)
	require.Contains(t, gh.comments[0], "https://pr-12-web.apps.example.com")

	// new commit on the PR: same service/environment, one more build, no second comment
	hook(t, m, "pr2", "pull_request", "whsecret", prPayload("synchronize", 12, "octo/web"))
	require.Len(t, queued(t, st), 2)
	svcs, _ := store.ListServices(ctx, st, map[string]string{"environment_id": env.Id})
	require.Len(t, svcs, 1)
	require.Len(t, gh.comments, 1)

	// closing the PR tears everything down
	w = hook(t, m, "pr3", "pull_request", "whsecret", prPayload("closed", 12, "octo/web"))
	require.Contains(t, w.Body.String(), "removed 1")
	_, err = store.GetService(ctx, st, pvID)
	require.True(t, store.IsNotFound(err))
	_, err = store.GetProto[v1.Environment](ctx, st, store.TableEnvironments, env.Id)
	require.True(t, store.IsNotFound(err))
	vars, _ = store.ListProto[v1.Variable](ctx, st, store.TableVariables, store.Query{Where: map[string]string{"environment_id": env.Id}})
	require.Empty(t, vars)
	_, err = store.GetService(ctx, st, "web") // production untouched
	require.NoError(t, err)
	require.Equal(t, 1, triggered)
}

func TestPreviewTeardownStopsReplicas(t *testing.T) {
	m, _, st := newModule(t)
	setupApp(t, m)
	ctx := context.Background()
	putSvc(t, st, "web", "web", "octo/web", "main", true, nil)
	hook(t, m, "a", "pull_request", "whsecret", prPayload("opened", 3, "octo/web"))
	pvID := queued(t, st)[0].ServiceId
	require.NoError(t, store.PutReplica(ctx, st, &v1.Replica{Id: "r1", ServiceId: pvID, MachineId: "m1", DesiredRunning: true, State: v1.ReplicaState_REPLICA_STATE_RUNNING}))
	require.NoError(t, store.PutDomain(ctx, st, &v1.Domain{Id: "d1", ServiceId: pvID, Hostname: "x"}))
	hook(t, m, "b", "pull_request", "whsecret", prPayload("closed", 3, "octo/web"))
	r, err := store.GetReplica(ctx, st, "r1")
	require.NoError(t, err)
	require.False(t, r.DesiredRunning)
	require.Equal(t, v1.ReplicaState_REPLICA_STATE_STOPPING, r.State)
	doms, _ := store.ListDomains(ctx, st, pvID)
	require.Empty(t, doms)
}

// ---- statuses / checks

func TestReporterStatusesAndChecks(t *testing.T) {
	m, gh, _ := newModule(t)
	setupApp(t, m)
	r := &Reporter{Module: m, Sync: true}
	job := &build.JobMeta{Kind: "github", Repo: "octo/web", SHA: strings.Repeat("a", 40), InstallationID: 7}
	r.Report(context.Background(), job, build.Event{Stage: "build", State: "pending", Description: "building", BuildID: "b1"})
	r.Report(context.Background(), job, build.Event{Stage: "build", State: "success", Description: "ok", BuildID: "b1"})
	r.Report(context.Background(), job, build.Event{Stage: "deploy", State: "failure", Description: strings.Repeat("x", 300), BuildID: "b1"})
	// non-GitHub jobs are ignored
	r.Report(context.Background(), &build.JobMeta{Kind: "upload"}, build.Event{Stage: "build", State: "pending"})

	require.Len(t, gh.statuses, 3)
	require.Equal(t, Status{State: "pending", Context: "aetherix/build", Description: "building", TargetURL: "https://ax.example.com/builds/b1"}, gh.statuses[0])
	require.Equal(t, "success", gh.statuses[1].State)
	require.Equal(t, "aetherix/deploy", gh.statuses[2].Context)
	require.LessOrEqual(t, len([]rune(gh.statuses[2].Description)), 140)

	// check run lifecycle for build: created in_progress, then completed(success); deploy created completed(failure)
	require.Len(t, gh.checks, 3)
	require.Equal(t, "in_progress", gh.checks[0].Status)
	require.Equal(t, "completed", gh.checks[1].Status)
	require.Equal(t, "success", gh.checks[1].Conclusion)
	require.Equal(t, "failure", gh.checks[2].Conclusion)
	require.True(t, gh.called("PATCH /repos/octo/web/check-runs/99"))
}

// ---- clone

type recRunner struct {
	mu   sync.Mutex
	cmds []build.Cmd
	fail map[string]bool
}

func (r *recRunner) Run(_ context.Context, c build.Cmd, out io.Writer) error {
	r.mu.Lock()
	r.cmds = append(r.cmds, c)
	r.mu.Unlock()
	fmt.Fprintf(out, "$ %s\n", c.String())
	for i, a := range c.Args {
		if a == "fetch" && r.fail != nil {
			for _, want := range c.Args[i:] {
				if r.fail[want] {
					return fmt.Errorf("fetch failed")
				}
			}
		}
		if strings.HasPrefix(a, "--format=AXINFO") {
			fmt.Fprint(out, "AXINFO\x1fdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef\x1fGrace\x1fthe subject\n")
		}
	}
	return nil
}

func TestGitSourceUsesTokenHeaderNeverURL(t *testing.T) {
	m, _, _ := newModule(t)
	setupApp(t, m)
	rr := &recRunner{}
	src := &GitSource{Module: m, Runner: rr, CloneBase: "https://git.example"}
	job := &build.JobMeta{Repo: "octo/web", InstallationID: 7, Ref: "refs/heads/main", SHA: "abc123"}
	info, err := src.Fetch(context.Background(), job, t.TempDir(), io.Discard)
	require.NoError(t, err)
	require.Equal(t, build.SourceInfo{SHA: "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", Author: "Grace", Message: "the subject"}, info)

	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:ghs_installtoken"))
	var sawFetch bool
	for _, c := range rr.cmds {
		require.Equal(t, "git", c.Name)
		require.Contains(t, c.Env, "GIT_TERMINAL_PROMPT=0")
		joined := strings.Join(c.Args, " ")
		require.NotContains(t, joined, "ghs_installtoken", "token must not be in argv/URL")
		require.NotContains(t, c.String(), basic, "log rendering is redacted")
		if len(c.Args) > 2 && c.Args[2] == "fetch" {
			sawFetch = true
			require.Equal(t, "http.extraheader=AUTHORIZATION: basic "+basic, c.Args[1])
			require.Contains(t, joined, "--depth 1")
			require.Contains(t, joined, "https://git.example/octo/web.git abc123")
		}
	}
	require.True(t, sawFetch)
	require.Equal(t, "checkout", rr.cmds[2].Args[0])
}

func TestGitSourceFallsBackToRef(t *testing.T) {
	m, _, _ := newModule(t)
	setupApp(t, m)
	rr := &recRunner{fail: map[string]bool{"abc123": true}}
	_, err := (&GitSource{Module: m, Runner: rr}).Fetch(context.Background(),
		&build.JobMeta{Repo: "octo/web", InstallationID: 7, Ref: "refs/pull/4/head", SHA: "abc123"}, t.TempDir(), io.Discard)
	require.NoError(t, err)
	var fetches []string
	for _, c := range rr.cmds {
		if len(c.Args) > 2 && c.Args[2] == "fetch" {
			fetches = append(fetches, c.Args[len(c.Args)-1])
		}
	}
	require.Equal(t, []string{"abc123", "refs/pull/4/head"}, fetches)

	_, err = (&GitSource{Module: m, Runner: &recRunner{}}).Fetch(context.Background(), &build.JobMeta{Repo: "octo/web", InstallationID: 7}, t.TempDir(), io.Discard)
	require.Error(t, err)
}

func TestAppJWT(t *testing.T) {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	now := time.Unix(1_700_000_000, 0)
	c := &Client{AppID: 99, Key: k, Now: func() time.Time { return now }}
	tok, err := c.AppJWT()
	require.NoError(t, err)
	parts := strings.Split(tok, ".")
	require.Len(t, parts, 3)
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims map[string]any
	require.NoError(t, json.Unmarshal(raw, &claims))
	require.Equal(t, "99", claims["iss"])
	require.EqualValues(t, now.Unix()-60, claims["iat"])
	require.EqualValues(t, now.Add(9*time.Minute).Unix(), claims["exp"])
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	d := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	require.NoError(t, rsa.VerifyPKCS1v15(&k.PublicKey, crypto.SHA256, d[:], sig))

	// PKCS#8 keys parse too
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	k2, err := ParsePrivateKey(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	require.NoError(t, err)
	require.True(t, k.Equal(k2))
	_, err = ParsePrivateKey([]byte("junk"))
	require.Error(t, err)
}

func TestHandlersMount(t *testing.T) {
	m, _, _ := newModule(t)
	hs := m.Handlers(nil)
	require.Contains(t, hs, WebhookPath)
	require.Contains(t, hs, ManifestCallbackPath)
	require.Contains(t, hs, ServicePath)
	require.ElementsMatch(t, Paths(), []string{WebhookPath, ManifestCallbackPath, ServicePath})
}
