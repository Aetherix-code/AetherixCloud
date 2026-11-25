package build

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/gen/aetherix/v1/aetherixv1connect"
	"github.com/aetherix/aetherixcloud/internal/api"
	"github.com/aetherix/aetherixcloud/internal/deploy"
	"github.com/aetherix/aetherixcloud/internal/store"
	"github.com/stretchr/testify/require"
)

func mkTar(t *testing.T, name, content string) io.Reader {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}))
	_, err := tw.Write([]byte(content))
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	return &buf
}

func newAPI(t *testing.T) (*API, *httptest.Server, store.Store, *Worker) {
	w, st := newWorker(t, &fakeBK{}, fakeSource{dockerfile: true}, nil)
	w.defaults()
	w.Deploy = func(ctx context.Context, s store.Store, svc *v1.Service, o deploy.StartOptions) (*v1.Deployment, error) {
		dep := &v1.Deployment{Id: "dep1", ServiceId: svc.Id, Spec: svc.Spec}
		svc.ActiveDeploymentId = dep.Id
		_ = store.PutService(ctx, s, svc)
		return dep, store.PutDeployment(ctx, s, dep)
	}
	a := &API{Store: st, Self: "m1", Worker: w, Logs: w.Logs, Pipeline: w.Pipeline, HTTPClient: api.H2CClient(),
		PeerURL: func(ip string) string { return "http://" + ip }}
	mux := http.NewServeMux()
	for p, h := range a.Handlers() {
		mux.Handle(p, h)
	}
	srv := httptest.NewServer(api.H2CHandler(mux))
	t.Cleanup(srv.Close)
	return a, srv, st, w
}

func TestUploadBuildsDeploysAndStreamsLogs(t *testing.T) {
	_, srv, st, _ := newAPI(t)
	svc := putSvc(t, st)

	// tarball with a Dockerfile
	var buf bytes.Buffer
	src := t.TempDir()
	write(t, src, "Dockerfile", "FROM scratch")
	require.NoError(t, TarDir(&buf, src, nil))
	resp, err := http.Post(srv.URL+UploadPath+"?service=web&project=proj", "application/gzip", &buf)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode, string(body))
	out := string(body)
	require.Contains(t, out, "detected build type: dockerfile")
	idx := strings.LastIndex(out, ResultPrefix)
	require.GreaterOrEqual(t, idx, 0, out)
	var res UploadResult
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(out[idx+len(ResultPrefix):])), &res))
	require.Empty(t, res.Error)
	require.Equal(t, "dep1", res.DeploymentID)
	require.Contains(t, res.Image, "127.0.0.1:5000/proj/web:up-")

	b, err := GetBuild(context.Background(), st, res.BuildID)
	require.NoError(t, err)
	require.Equal(t, StatusSucceeded, b.Status)
	require.Equal(t, svc.Id, b.ServiceId)

	// StreamBuildLogs over Connect replays the stored log
	c := aetherixv1connect.NewLogServiceClient(api.H2CClient(), srv.URL, connect.WithGRPC())
	st2, err := c.StreamBuildLogs(context.Background(), connect.NewRequest(&v1.StreamBuildLogsRequest{BuildId: res.BuildID}))
	require.NoError(t, err)
	var lines []string
	for st2.Receive() {
		lines = append(lines, st2.Msg().Line)
		require.Equal(t, "m1", st2.Msg().MachineId)
	}
	require.NoError(t, st2.Err())
	require.Contains(t, strings.Join(lines, "\n"), "build succeeded")

	// unknown build -> NotFound
	st3, err := c.StreamBuildLogs(context.Background(), connect.NewRequest(&v1.StreamBuildLogsRequest{BuildId: "nope"}))
	require.NoError(t, err)
	for st3.Receive() {
	}
	require.Equal(t, connect.CodeNotFound, connect.CodeOf(st3.Err()))
}

func TestUploadRejectsTraversalAndUnknownService(t *testing.T) {
	_, srv, st, _ := newAPI(t)
	putSvc(t, st)
	resp, err := http.Post(srv.URL+UploadPath+"?service=web", "application/x-tar", mkTar(t, "../x", "y"))
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp, err = http.Post(srv.URL+UploadPath+"?service=ghost", "application/x-tar", mkTar(t, "Dockerfile", "FROM scratch"))
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestPlanPreviewEndpoint(t *testing.T) {
	_, srv, st, _ := newAPI(t)
	putSvc(t, st)
	// Railpack project (no Dockerfile): the plan comes from the fake railpack
	resp, err := http.Post(srv.URL+PlanPath+"?service=web&start_cmd=node+x.js", "application/x-tar", mkTar(t, "package.json", "{}"))
	require.NoError(t, err)
	var pr PlanResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&pr))
	resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	require.Equal(t, ProviderRailpack, pr.Plan.Provider)
	require.Equal(t, "node", pr.Detected.Language)
}

func TestAuthGuard(t *testing.T) {
	a, _, _, _ := newAPI(t)
	a.Authenticate = Authenticator(BearerVerifier(func(_ context.Context, tok string) error {
		if tok != "good" {
			return io.ErrUnexpectedEOF
		}
		return nil
	}))
	h := a.guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	do := func(mod func(r *http.Request)) int {
		r := httptest.NewRequest("GET", "/", nil)
		mod(r)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	require.Equal(t, 204, do(func(r *http.Request) {}), "mesh-internal plain HTTP passes")
	require.Equal(t, 401, do(func(r *http.Request) { r.Header.Set("X-Forwarded-For", "1.2.3.4") }), "proxied requests need a token")
	require.Equal(t, 401, do(func(r *http.Request) {
		r.Header.Set("X-Forwarded-For", "1.2.3.4")
		r.Header.Set("Authorization", "Bearer bad")
	}))
	require.Equal(t, 204, do(func(r *http.Request) {
		r.Header.Set("X-Forwarded-For", "1.2.3.4")
		r.Header.Set("Authorization", "Bearer good")
	}))
}

func TestImageUploadLoadsTagsPushesAndDeploys(t *testing.T) {
	a, srv, st, w := newAPI(t)
	rr := &fakeRunner{fn: func(c Cmd, out io.Writer) error {
		switch c.Args[0] {
		case "load":
			io.WriteString(out, "Loaded image: myapp:latest\n")
		case "push":
			io.WriteString(out, "latest: digest: sha256:"+strings.Repeat("ab", 32)+" size: 1234\n")
		}
		return nil
	}}
	w.Pipeline.Runner = rr
	_ = a
	putSvc(t, st)

	resp, err := http.Post(srv.URL+ImagePath+"?service=web", "application/x-tar", strings.NewReader("fake image bytes"))
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	out := string(body)
	idx := strings.LastIndex(out, ResultPrefix)
	require.GreaterOrEqual(t, idx, 0, out)
	var res UploadResult
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(out[idx+len(ResultPrefix):])), &res))
	require.Empty(t, res.Error, out)
	require.Contains(t, res.Image, "127.0.0.1:5000/proj/web:up-")
	require.Equal(t, "dep1", res.DeploymentID)

	var names []string
	for _, c := range rr.cmds {
		names = append(names, c.Args[0])
	}
	require.Equal(t, []string{"load", "tag", "push"}, names)
	require.Equal(t, []string{"tag", "myapp:latest", res.Image}, rr.cmds[1].Args)
	b, _ := GetBuild(context.Background(), st, res.BuildID)
	require.Equal(t, StatusSucceeded, b.Status)
	require.Equal(t, "image", b.Provider)
}

func TestImageUploadFailure(t *testing.T) {
	_, srv, st, w := newAPI(t)
	w.Pipeline.Runner = &fakeRunner{fn: func(c Cmd, out io.Writer) error { return io.ErrUnexpectedEOF }}
	putSvc(t, st)
	resp, err := http.Post(srv.URL+ImagePath+"?service=web", "application/x-tar", strings.NewReader("x"))
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Contains(t, string(body), `"error":"unexpected EOF"`)
}
