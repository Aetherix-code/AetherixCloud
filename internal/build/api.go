package build

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/gen/aetherix/v1/aetherixv1connect"
	"github.com/aetherix/aetherixcloud/internal/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Paths served by API.Handlers.
const (
	UploadPath = "/api/builds/upload"
	PlanPath   = "/api/builds/plan"
	// ImagePath receives a `docker save` stream (ax build --local / ax image push).
	ImagePath = "/api/builds/image"
	// ImagesConfigPath is the cluster-wide S3 archive setting for build images.
	ImagesConfigPath = "/api/build-images"
	// ConcurrencyConfigPath is max parallel builds per builder (excess stay queued).
	ConcurrencyConfigPath = "/api/build-concurrency"
	// ResultPrefix starts the final line of an upload response body.
	ResultPrefix = "AX-RESULT "
)

const proxiedHeader = "Ax-Proxied"

// API serves build logs (LogService.StreamBuildLogs), `ax up` uploads and the
// detected-plan preview.
type API struct {
	Store    store.Store
	Self     string
	Worker   *Worker
	Logs     *LogStore
	Pipeline *Pipeline
	// Source fetches GitHub sources for plan previews of repo-backed services.
	Source Source
	// Authenticate validates a request; nil allows everything. Mesh-internal
	// calls (plain HTTP, no X-Forwarded-For) are the caller's concern.
	Authenticate func(r *http.Request) error
	// RequireAdmin is checked for mutating settings (optional).
	RequireAdmin func(r *http.Request) error
	// HTTPClient / PeerURL reach other nodes over the mesh (h2c).
	HTTPClient *http.Client
	PeerURL    func(meshIP string) string
}

// Handlers returns the path -> handler map for daemon.Hooks.Handlers.
func (a *API) Handlers() map[string]http.Handler {
	logs := connect.NewServerStreamHandler(aetherixv1connect.LogServiceStreamBuildLogsProcedure, a.streamBuildLogs)
	return map[string]http.Handler{
		aetherixv1connect.LogServiceStreamBuildLogsProcedure: a.guard(logs),
		UploadPath:       a.guard(http.HandlerFunc(a.handleUpload)),
		PlanPath:         a.guard(http.HandlerFunc(a.handlePlan)),
		ImagePath:        a.guard(http.HandlerFunc(a.handleImage)),
		ImagesConfigPath:       a.guard(http.HandlerFunc(a.handleImagesConfig)),
	ConcurrencyConfigPath:  a.guard(http.HandlerFunc(a.handleConcurrencyConfig)),
	}
}

func (a *API) guard(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.Authenticate != nil {
			if err := a.Authenticate(r); err != nil {
				http.Error(w, err.Error(), http.StatusUnauthorized)
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

// ---- StreamBuildLogs

func (a *API) streamBuildLogs(ctx context.Context, req *connect.Request[v1.StreamBuildLogsRequest], stream *connect.ServerStream[v1.LogEntry]) error {
	id := req.Msg.BuildId
	b, err := GetBuild(ctx, a.Store, id)
	if err != nil {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("build %q not found", id))
	}
	proxied := req.Header().Get(proxiedHeader) != ""
	send := func(l LogLine) error {
		return stream.Send(&v1.LogEntry{Time: timestamppb.New(l.Time), MachineId: a.Self, Stream: l.Stream, Line: l.Line})
	}
	systemLine := func(s string) error { return send(LogLine{Time: time.Now(), Stream: "system", Line: s}) }
	waited := false
	for {
		b, err = GetBuild(ctx, a.Store, id)
		if err != nil {
			return connect.NewError(connect.CodeNotFound, err)
		}
		if b.BuilderMachineId == "" && b.Status == StatusQueued {
			if !req.Msg.Follow {
				return nil
			}
			if !waited {
				waited = true
				if err := systemLine("waiting for a builder to pick up the build..."); err != nil {
					return err
				}
			}
			select {
			case <-time.After(time.Second):
				continue
			case <-ctx.Done():
				return nil
			}
		}
		break
	}
	if b.BuilderMachineId == "" || b.BuilderMachineId == a.Self {
		return a.localBuildLogs(ctx, b, req.Msg.Follow, send)
	}
	if proxied {
		return connect.NewError(connect.CodeNotFound, errors.New("build log is on another node"))
	}
	m, err := store.GetMachine(ctx, a.Store, b.BuilderMachineId)
	if err != nil {
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("builder %s unknown: %w", b.BuilderMachineId, err))
	}
	c := aetherixv1connect.NewLogServiceClient(a.HTTPClient, a.PeerURL(m.MeshIp), connect.WithGRPC())
	creq := connect.NewRequest(&v1.StreamBuildLogsRequest{BuildId: id, Follow: req.Msg.Follow})
	creq.Header().Set(proxiedHeader, "1")
	st, err := c.StreamBuildLogs(ctx, creq)
	if err != nil {
		return connect.NewError(connect.CodeUnavailable, fmt.Errorf("builder %s unreachable: %w", m.Name, err))
	}
	defer st.Close()
	for st.Receive() {
		if err := stream.Send(st.Msg()); err != nil {
			return err
		}
	}
	if err := st.Err(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func (a *API) localBuildLogs(ctx context.Context, b *v1.Build, follow bool, send func(LogLine) error) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := a.Logs.Stream(ctx, b.Id, follow, send)
		if !errors.Is(err, ErrNoLog) {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		// The build was just assigned: the writer may not exist yet.
		if b.Status == StatusFailed || b.Status == StatusSucceeded || time.Now().After(deadline) {
			return connect.NewError(connect.CodeNotFound, fmt.Errorf("log of build %s is not available on this node", b.Id))
		}
		select {
		case <-time.After(200 * time.Millisecond):
		case <-ctx.Done():
			return nil
		}
		if nb, err := GetBuild(ctx, a.Store, b.Id); err == nil {
			b = nb
		}
	}
}

// ---- service resolution

func (a *API) resolveService(ctx context.Context, ref, project string) (*v1.Service, error) {
	if ref == "" {
		return nil, errors.New("missing service")
	}
	if s, err := store.GetService(ctx, a.Store, ref); err == nil {
		return s, nil
	}
	list, err := store.ListServices(ctx, a.Store, map[string]string{"name": ref})
	if err != nil {
		return nil, err
	}
	var match []*v1.Service
	for _, s := range list {
		if project == "" || s.ProjectId == project {
			match = append(match, s)
			continue
		}
		if p, err := store.GetProto[v1.Project](ctx, a.Store, store.TableProjects, s.ProjectId); err == nil && p.Name == project {
			match = append(match, s)
		}
	}
	switch len(match) {
	case 0:
		return nil, fmt.Errorf("service %q not found", ref)
	case 1:
		return match[0], nil
	}
	return nil, fmt.Errorf("service name %q is ambiguous: pass project", ref)
}

// forwardToBuilder proxies r to the best builder when this node is not it.
// It returns true if the request was handled.
func (a *API) forwardToBuilder(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get(proxiedHeader) != "" {
		return false
	}
	ms, err := store.ListMachines(r.Context(), a.Store)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return true
	}
	order := RankBuilders(ms, a.Worker.load(r.Context()))
	if len(order) == 0 {
		http.Error(w, "no builder node available", http.StatusServiceUnavailable)
		return true
	}
	if order[0] == a.Self {
		return false
	}
	var target *v1.Machine
	for _, m := range ms {
		if m.Id == order[0] {
			target = m
		}
	}
	u, err := url.Parse(a.PeerURL(target.MeshIp))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return true
	}
	rp := httputil.NewSingleHostReverseProxy(u)
	rp.FlushInterval = -1
	rp.Director = func(pr *http.Request) {
		pr.URL.Scheme, pr.URL.Host = u.Scheme, u.Host
		pr.Host = u.Host
		pr.Header.Set(proxiedHeader, "1")
		pr.Header.Del("Authorization") // mesh listener trusts the mesh
	}
	rp.ServeHTTP(w, r)
	return true
}

// ---- POST /api/builds/upload?service=<id|name>[&project=<id|name>]

// UploadResult is the JSON after ResultPrefix on the final line.
type UploadResult struct {
	BuildID      string `json:"build_id"`
	Image        string `json:"image,omitempty"`
	DeploymentID string `json:"deployment_id,omitempty"`
	Error        string `json:"error,omitempty"`
}

func (a *API) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST a tar.gz of the source", http.StatusMethodNotAllowed)
		return
	}
	if a.forwardToBuilder(w, r) {
		return
	}
	svc, err := a.resolveService(r.Context(), r.URL.Query().Get("service"), r.URL.Query().Get("project"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	dir, err := os.MkdirTemp(a.Worker.WorkDir, "ax-up-")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(dir)
	if err := ExtractTar(r.Body, dir, 0); err != nil {
		http.Error(w, "invalid upload: "+err.Error(), http.StatusBadRequest)
		return
	}
	a.streamRun(w, r, svc, func(ctx context.Context, fw io.Writer) (*v1.Build, error) {
		return a.Worker.RunUpload(ctx, svc, dir, fw)
	})
}

// handleImage: POST ?service=<id|name> with a `docker save` stream.
func (a *API) handleImage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST a docker save stream", http.StatusMethodNotAllowed)
		return
	}
	if a.forwardToBuilder(w, r) {
		return
	}
	svc, err := a.resolveService(r.Context(), r.URL.Query().Get("service"), r.URL.Query().Get("project"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	a.streamRun(w, r, svc, func(ctx context.Context, fw io.Writer) (*v1.Build, error) {
		return a.Worker.RunImageUpload(ctx, svc, r.Body, fw)
	})
}

// streamRun streams run's log as the response body and ends it with the
// ResultPrefix line.
func (a *API) streamRun(w http.ResponseWriter, r *http.Request, svc *v1.Service, run func(context.Context, io.Writer) (*v1.Build, error)) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	fw := &flushWriter{w: w}
	if f, ok := w.(http.Flusher); ok {
		fw.f = f
	}
	w.WriteHeader(http.StatusOK)
	b, err := run(r.Context(), fw)
	res := UploadResult{}
	if b != nil {
		res.BuildID, res.Image = b.Id, b.Image
		if cur, e := store.GetService(r.Context(), a.Store, svc.Id); e == nil {
			res.DeploymentID = cur.ActiveDeploymentId
		}
	}
	if err != nil {
		res.Error = err.Error()
	}
	js, _ := json.Marshal(res)
	fmt.Fprintf(fw, "\n%s%s\n", ResultPrefix, js)
}

type flushWriter struct {
	w io.Writer
	f http.Flusher
}

func (f *flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if f.f != nil {
		f.f.Flush()
	}
	return n, err
}

// ---- detected-plan preview

// PlanResponse is the body of /api/builds/plan.
type PlanResponse struct {
	*Preview
	Commit string `json:"commit,omitempty"`
}

// handlePlan: GET ?service=<id> previews a repo-backed service (clones its
// branch head); POST with a tar.gz body previews that source. Query
// overrides: builder, root, dockerfile, build_cmd, start_cmd.
func (a *API) handlePlan(w http.ResponseWriter, r *http.Request) {
	if a.forwardToBuilder(w, r) {
		return
	}
	ctx := r.Context()
	q := r.URL.Query()
	svc, err := a.resolveService(ctx, q.Get("service"), q.Get("project"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	s := SettingsFromSpec(svc.Spec.GetBuild(), svc.Name)
	for k, dst := range map[string]*string{"builder": &s.Builder, "root": &s.RootDir, "dockerfile": &s.Dockerfile,
		"build_cmd": &s.BuildCommand, "start_cmd": &s.StartCommand} {
		if v := q.Get(k); v != "" {
			*dst = v
		}
	}
	dir, err := os.MkdirTemp(a.Worker.WorkDir, "ax-plan-src-")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(dir)
	resp := PlanResponse{}
	switch {
	case r.Method == http.MethodPost:
		if err := ExtractTar(r.Body, dir, 0); err != nil {
			http.Error(w, "invalid upload: "+err.Error(), http.StatusBadRequest)
			return
		}
	case svc.Spec.GetSource().GetGithub().GetRepo() != "" && a.Source != nil:
		gh := svc.Spec.Source.Github
		job := &JobMeta{ServiceID: svc.Id, Kind: "github", Repo: gh.Repo, InstallationID: gh.InstallationId, Ref: "refs/heads/" + gh.Branch}
		info, err := a.Source.Fetch(ctx, job, dir, io.Discard)
		if err != nil {
			http.Error(w, "fetch source: "+err.Error(), http.StatusBadGateway)
			return
		}
		resp.Commit = info.SHA
	default:
		http.Error(w, "service has no GitHub source: POST a tar.gz of the source", http.StatusBadRequest)
		return
	}
	pv, err := a.Pipeline.Preview(ctx, dir, s, a.Worker.buildSecrets(ctx, svc))
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	resp.Preview = pv
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// ---- lazy handler (Deps arrive after the daemon starts)

// Lazy is an http.Handler whose target is set once the daemon dependencies
// exist; until then it answers 503.
type Lazy struct{ h atomic.Pointer[http.Handler] }

// Set installs the handler.
func (l *Lazy) Set(h http.Handler) { l.h.Store(&h) }

// ServeHTTP implements http.Handler.
func (l *Lazy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := l.h.Load()
	if h == nil {
		http.Error(w, "starting", http.StatusServiceUnavailable)
		return
	}
	(*h).ServeHTTP(w, r)
}

// MuxOf turns a path map into one handler (exact longest-prefix via ServeMux).
func MuxOf(m map[string]http.Handler) http.Handler {
	mux := http.NewServeMux()
	for p, h := range m {
		mux.Handle(p, h)
	}
	return mux
}

func (a *API) handleImagesConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg, err := GetImageArchiveConfig(r.Context(), a.Store)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cfg)
	case http.MethodPut:
		if a.RequireAdmin != nil {
			if err := a.RequireAdmin(r); err != nil {
				http.Error(w, err.Error(), http.StatusForbidden)
				return
			}
		}
		var cfg ImageArchiveConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err := PutImageArchiveConfig(r.Context(), a.Store, cfg); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (a *API) handleConcurrencyConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg, err := GetConcurrencyConfig(r.Context(), a.Store)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cfg)
	case http.MethodPut:
		if a.RequireAdmin != nil {
			if err := a.RequireAdmin(r); err != nil {
				http.Error(w, err.Error(), http.StatusForbidden)
				return
			}
		}
		var cfg ConcurrencyConfig
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err := PutConcurrencyConfig(r.Context(), a.Store, cfg); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

// Authenticator returns an Authenticate func: mesh-internal requests (plain
// HTTP without proxy headers) pass, everything else must pass verify.
func Authenticator(verify func(r *http.Request) error) func(*http.Request) error {
	return func(r *http.Request) error {
		if r.TLS == nil && r.Header.Get("X-Forwarded-For") == "" && r.Header.Get("Forwarded") == "" {
			return nil
		}
		if verify == nil {
			return errors.New("unauthorized")
		}
		return verify(r)
	}
}

// BearerVerifier checks "Authorization: Bearer <token>" with check.
func BearerVerifier(check func(ctx context.Context, token string) error) func(r *http.Request) error {
	return func(r *http.Request) error {
		h := r.Header.Get("Authorization")
		tok := strings.TrimPrefix(h, "Bearer ")
		if tok == "" || tok == h {
			return errors.New("missing bearer token")
		}
		if err := check(r.Context(), tok); err != nil {
			return errors.New("invalid token")
		}
		return nil
	}
}
