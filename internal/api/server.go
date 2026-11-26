// Package api serves the Connect-RPC services (also reachable as gRPC and
// gRPC-web, so the browser and the CLI use the same endpoints).
//
// Phase A implements: ClusterService, MachineService, ProjectService,
// ServiceService, DeployService (basic, non-health-gated), LogService
// (container logs) and the exec stream. AuthService, GitHubService and
// StreamBuildLogs return Unimplemented and are owned by later workers.
//
// Two listeners use the same handlers (see Handler / PublicHandler):
//
//   - mesh/loopback HTTP (no auth; reachable only through WireGuard or
//     localhost): node-to-node calls, Caddy upstream, local CLI.
//   - public HTTPS with cluster-CA certificate: every call except
//     MachineService.JoinMachine requires "Authorization: Bearer <api token>".
package api

import (
	"context"
	"log/slog"
	"net"
	"net/http"

	"connectrpc.com/connect"
	"github.com/aetherix/aetherixcloud/gen/aetherix/v1/aetherixv1connect"
	"github.com/aetherix/aetherixcloud/internal/auth"
	"github.com/aetherix/aetherixcloud/internal/cluster"
	"github.com/aetherix/aetherixcloud/internal/lease"
	"github.com/aetherix/aetherixcloud/internal/membership"
	"github.com/aetherix/aetherixcloud/internal/runtime"
	"github.com/aetherix/aetherixcloud/internal/secrets"
	"github.com/aetherix/aetherixcloud/internal/store"
)

// Deps are the dependencies of the API handlers.
type Deps struct {
	Store   store.Store
	Leases  *lease.Manager
	State   *cluster.NodeState
	Runtime runtime.Runtime        // optional: logs/exec on local replicas
	Members *membership.Membership // optional
	// OnChange is called after mutations that the local reconciler should act
	// on immediately (optional).
	OnChange func()
	Version  string
	Logger   *slog.Logger
	// HTTPClient used to proxy node-specific RPCs to peers over the mesh.
	HTTPClient *http.Client
	// PeerURL resolves a machine to its mesh HTTP base URL. Default:
	// http://<mesh_ip>:8080.
	PeerURL func(meshIP string) string
}

type server struct {
	Deps
	key secrets.Key
}

func (d *Deps) defaults() {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.HTTPClient == nil {
		d.HTTPClient = H2CClient()
	}
	if d.PeerURL == nil {
		d.PeerURL = func(ip string) string { return "http://" + net.JoinHostPort(ip, "8080") }
	}
	if d.Version == "" {
		d.Version = "dev"
	}
}

// Handler returns the unauthenticated handler (mesh/loopback listeners).
func Handler(d Deps) http.Handler { return handler(d, false) }

// handler builds the mux. public=true installs the auth/RBAC interceptor
// (internal/auth) on every service; the trusted listener has none.
func handler(d Deps, public bool) http.Handler {
	d.defaults()
	s := &server{Deps: d}
	var authn *auth.Authenticator
	if d.State != nil {
		if k, err := secrets.ParseKey(d.State.ClusterKey); err == nil {
			s.key = k
		}
		authn, _ = auth.NewAuthenticator(d.Store, d.State.ClusterKey)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	opts := []connect.HandlerOption{}
	if public {
		if authn == nil {
			// no cluster key: refuse everything rather than serve unauthenticated
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "authentication unavailable", http.StatusServiceUnavailable)
			})
		}
		opts = append(opts, connect.WithInterceptors(auth.NewInterceptor(authn, d.Logger)))
	}
	mount := func(path string, h http.Handler) { mux.Handle(path, h) }
	mount(aetherixv1connect.NewClusterServiceHandler(&clusterSvc{s}, opts...))
	mount(aetherixv1connect.NewMachineServiceHandler(&machineSvc{s}, opts...))
	mount(aetherixv1connect.NewProjectServiceHandler(&projectSvc{s}, opts...))
	mount(aetherixv1connect.NewServiceServiceHandler(&serviceSvc{s}, opts...))
	mount(aetherixv1connect.NewDeployServiceHandler(&deploySvc{s}, opts...))
	mount(aetherixv1connect.NewLogServiceHandler(&logSvc{s}, opts...))
	if authn != nil {
		mount(aetherixv1connect.NewAuthServiceHandler(auth.NewService(authn, !public), opts...))
	} else {
		mount(aetherixv1connect.NewAuthServiceHandler(aetherixv1connect.UnimplementedAuthServiceHandler{}, opts...))
	}
	mount(aetherixv1connect.NewGitHubServiceHandler(aetherixv1connect.UnimplementedGitHubServiceHandler{}, opts...))
	// Browser terminal: WebSocket (ReadableStream duplex fails on HTTP/1.1 / Safari).
	mux.Handle(ExecWSPath, execWSHandler(s, authn, public))
	mux.Handle(HostExecWSPath, hostExecWSHandler(s, authn, public))
	// Browser snapshot restore: raw POST (Connect client-streaming upload fails in fetch).
	mux.Handle(RestoreHTTPPath, restoreHTTPHandler(s, authn, public))
	return mux
}

// PublicHandler is the internet-facing handler: every RPC goes through the
// auth interceptor (bearer API token, session JWT or OAuth token, then RBAC).
func PublicHandler(d Deps) http.Handler { return handler(d, true) }

// ---- helpers shared by the services

func (s *server) changed() {
	if s.OnChange != nil {
		s.OnChange()
	}
}

func errNotFound(what, id string) error {
	return connect.NewError(connect.CodeNotFound, errorf("%s %q not found", what, id))
}

func errInvalid(format string, a ...any) error {
	return connect.NewError(connect.CodeInvalidArgument, errorf(format, a...))
}

func internalErr(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := err.(*connect.Error); ok {
		return err
	}
	return connect.NewError(connect.CodeInternal, err)
}

var _ = context.Background
