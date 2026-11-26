package main

import (
	"context"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/aetherix/aetherixcloud/internal/api"
	"github.com/aetherix/aetherixcloud/internal/auth"
	"github.com/aetherix/aetherixcloud/internal/daemon"
	"github.com/aetherix/aetherixcloud/internal/events"
	"github.com/aetherix/aetherixcloud/internal/mcp"
	"github.com/aetherix/aetherixcloud/internal/oauth"
)

// authRuntime is built once the daemon core is up (Hooks.Services run with
// the shared deps) and published to the lazily-dispatching HTTP handlers.
type authRuntime struct {
	oauth *oauth.Server
	mcp   *mcp.Server
}

// registerAuthHooks wires the auth-api/mcp worker's pieces into the daemon:
//
//   - /mcp                      Streamable HTTP MCP server (every node)
//   - /.well-known/oauth-*, /oauth/{authorize,token,register,revoke},
//     /oauth/api/consent        OAuth 2.1 authorization server
//   - a service that publishes the above, seeds the first owner from
//     AX_ADMIN_EMAIL/AX_ADMIN_PASSWORD when no user exists, and prunes
//     events/audit/OAuth leftovers
//
// RBAC itself is installed inside api.PublicHandler (auth interceptor).
func registerAuthHooks(d *daemon.Daemon) {
	var rt atomic.Pointer[authRuntime]
	lazy := func(pick func(*authRuntime) http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			x := rt.Load()
			if x == nil {
				http.Error(w, "starting", http.StatusServiceUnavailable)
				return
			}
			pick(x).ServeHTTP(w, r)
		})
	}
	d.Hooks.Handlers[mcp.Path] = lazy(func(x *authRuntime) http.Handler { return x.mcp.Handler() })
	for path := range (&oauth.Server{}).Handlers() {
		d.Hooks.Handlers[path] = lazy(func(x *authRuntime) http.Handler { return x.oauth.Handlers()[path] })
	}

	d.Hooks.Services = append(d.Hooks.Services, func(ctx context.Context, dp *daemon.Deps) error {
		authn, err := auth.NewAuthenticator(dp.Store, dp.State.ClusterKey)
		if err != nil {
			return err
		}
		apiDeps := api.Deps{Store: dp.Store, Leases: dp.Leases, State: dp.State, Runtime: dp.Runtime, Members: dp.Members, Logger: dp.Logger}
		if dp.Reconciler != nil {
			apiDeps.OnChange = dp.Reconciler.Trigger
		}
		// In-process handler for MCP tools: Connect RPCs plus hook HTTP APIs
		// (/api/backups, /api/s3-targets, /api/build-*, …). Skip /mcp so a
		// tool never recursively hits the MCP endpoint.
		pub := api.PublicHandler(apiDeps)
		inner := http.NewServeMux()
		for p, h := range d.Hooks.Handlers {
			if p == mcp.Path {
				continue
			}
			inner.Handle(p, h)
		}
		inner.Handle("/", pub)
		oa := oauth.New(dp.Store, authn, dp.Logger)
		m := mcp.New(mcp.Config{Store: dp.Store, Auth: authn, OAuth: oa, API: func() http.Handler { return inner }, Logger: dp.Logger})
		rt.Store(&authRuntime{oauth: oa, mcp: m})

		if email, pw := os.Getenv("AX_ADMIN_EMAIL"), os.Getenv("AX_ADMIN_PASSWORD"); email != "" && pw != "" {
			if made, err := auth.EnsureFirstOwner(ctx, dp.Store, email, pw); err != nil {
				dp.Logger.Warn("first-owner bootstrap failed", "err", err)
			} else if made {
				dp.Logger.Info("created first owner from AX_ADMIN_EMAIL", "email", email)
			}
		}
		go oa.Run(ctx)
		go events.Run(ctx, dp.Store, func(ctx context.Context) error {
			_, err := auth.PruneAudit(ctx, dp.Store, 90*24*time.Hour)
			return err
		})
		<-ctx.Done()
		return nil
	})
}
