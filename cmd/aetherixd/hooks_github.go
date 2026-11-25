package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aetherix/aetherixcloud/gen/aetherix/v1/aetherixv1connect"
	"github.com/aetherix/aetherixcloud/internal/api"
	"github.com/aetherix/aetherixcloud/internal/auth"
	"github.com/aetherix/aetherixcloud/internal/build"
	"github.com/aetherix/aetherixcloud/internal/cluster"
	"github.com/aetherix/aetherixcloud/internal/daemon"
	"github.com/aetherix/aetherixcloud/internal/github"
	"github.com/aetherix/aetherixcloud/internal/secrets"
	"github.com/aetherix/aetherixcloud/internal/store"
)

// hookGitHub registers the auto-build pipeline and the GitHub App
// integration (github/build worker):
//
//   - GitHubService RPCs, /api/github/webhook, /api/github/manifest/callback
//   - LogService.StreamBuildLogs, /api/builds/upload (`ax up`), /api/builds/plan
//   - the builder queue worker (leases, least-loaded selection) as a hook service.
//
// Environment overrides: AX_DASHBOARD_URL, AX_BUILDKIT_ADDR,
// AX_RAILPACK_FRONTEND, AX_DATA_DIR.
func hookGitHub(d *daemon.Daemon) {
	routes := &build.Lazy{}
	paths := append(github.Paths(), build.UploadPath, build.PlanPath, build.ImagePath, aetherixv1connect.LogServiceStreamBuildLogsProcedure)
	for _, p := range paths {
		d.Hooks.Handlers[p] = routes
	}
	d.Hooks.Services = append(d.Hooks.Services, func(ctx context.Context, deps *daemon.Deps) error {
		return runGitHub(ctx, deps, routes)
	})
}

func runGitHub(ctx context.Context, deps *daemon.Deps, routes *build.Lazy) error {
	key, err := secrets.ParseKey(deps.State.ClusterKey)
	if err != nil {
		return err
	}
	dataDir := dataDirFromArgs()
	self := deps.State.MachineID
	runner := build.ExecRunner{}
	logs, err := build.NewLogStore(filepath.Join(dataDir, "builds", "logs"))
	if err != nil {
		return err
	}
	workDir := filepath.Join(dataDir, "builds", "tmp")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return err
	}
	pipeline := &build.Pipeline{Runner: runner, WorkDir: workDir, FrontendImage: os.Getenv("AX_RAILPACK_FRONTEND"),
		BK: &build.Buildctl{Runner: runner, Addr: os.Getenv("AX_BUILDKIT_ADDR")}}

	mod := &github.Module{Store: deps.Store, Key: key, Self: self, Runner: runner, Logger: deps.Logger,
		DashboardURL: func(ctx context.Context) string { return dashboardURL(ctx, deps.Store) }}
	if deps.Reconciler != nil {
		mod.Trigger = deps.Reconciler.Trigger
	}
	src := &github.GitSource{Module: mod, Runner: runner}
	worker := &build.Worker{Store: deps.Store, Leases: deps.Leases, Self: self, Pipeline: pipeline, Logs: logs,
		Source: src, Reporter: &github.Reporter{Module: mod}, Key: key, Logger: deps.Logger, WorkDir: workDir}

	authn := build.Authenticator(build.BearerVerifier(func(ctx context.Context, tok string) error {
		_, err := auth.Verify(ctx, deps.Store, tok)
		return err
	}))
	guard := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := authn(r); err != nil {
				http.Error(w, err.Error(), http.StatusUnauthorized)
				return
			}
			h.ServeHTTP(w, r)
		})
	}
	bapi := &build.API{Store: deps.Store, Self: self, Worker: worker, Logs: logs, Pipeline: pipeline, Source: src,
		Authenticate: authn, HTTPClient: api.H2CClient(),
		PeerURL: func(ip string) string {
			return "http://" + net.JoinHostPort(ip, strconv.Itoa(cluster.DefaultMeshAPIPort))
		}}
	all := mod.Handlers(guard)
	for p, h := range bapi.Handlers() {
		all[p] = h
	}
	routes.Set(build.MuxOf(all))

	go func() { // dedup rows older than a week
		t := time.NewTicker(6 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_, _ = mod.PruneDeliveries(ctx)
			}
		}
	}()
	return worker.Run(ctx)
}

func dashboardURL(ctx context.Context, st store.Store) string {
	if u := os.Getenv("AX_DASHBOARD_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	if c, err := store.GetCluster(ctx, st); err == nil && c.Domain != "" {
		return "https://dashboard." + c.Domain
	}
	return ""
}

// dataDirFromArgs finds --data-dir in the process arguments (the daemon does
// not expose its config to hooks yet).
func dataDirFromArgs() string {
	if v := os.Getenv("AX_DATA_DIR"); v != "" {
		return v
	}
	args := os.Args
	for i, a := range args {
		if a == "--data-dir" || a == "-data-dir" {
			if i+1 < len(args) {
				return args[i+1]
			}
		}
		if v, ok := strings.CutPrefix(a, "--data-dir="); ok {
			return v
		}
	}
	return "/var/lib/aetherix"
}
