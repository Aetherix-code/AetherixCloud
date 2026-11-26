package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/aetherix/aetherixcloud/internal/api"
	"github.com/aetherix/aetherixcloud/internal/auth"
	"github.com/aetherix/aetherixcloud/internal/backup"
	"github.com/aetherix/aetherixcloud/internal/cluster"
	"github.com/aetherix/aetherixcloud/internal/cron"
	"github.com/aetherix/aetherixcloud/internal/daemon"
	"github.com/aetherix/aetherixcloud/internal/metrics"
	"github.com/aetherix/aetherixcloud/internal/notify"
	"github.com/aetherix/aetherixcloud/internal/snapshot"
	"github.com/aetherix/aetherixcloud/internal/store"
	"github.com/aetherix/aetherixcloud/internal/templates"
)

// hookExtras registers polish features (templates, metrics, volume backups,
// cron jobs, notifications). Audit log is owned by the auth worker.
func hookExtras(d *daemon.Daemon) {
	var h atomic.Pointer[http.Handler]
	dispatch := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if x := h.Load(); x != nil {
			(*x).ServeHTTP(w, r)
			return
		}
		http.Error(w, "starting", http.StatusServiceUnavailable)
	})
	// Exact paths plus trailing-slash prefixes so /api/cron/{id} etc. match
	// (Go ServeMux only treats "/foo/" as a subtree).
	for _, p := range []string{
		templates.Path, metrics.Path,
		backup.Path, backup.Path + "/",
		backup.TargetsPath, backup.TargetsPath + "/",
		cron.Path, cron.Path + "/",
		notify.Path, notify.Path + "/",
	} {
		d.Hooks.Handlers[p] = dispatch
	}
	d.Hooks.Services = append(d.Hooks.Services, func(ctx context.Context, deps *daemon.Deps) error {
		return runExtras(ctx, deps, &h)
	})
}

func runExtras(ctx context.Context, deps *daemon.Deps, h *atomic.Pointer[http.Handler]) error {
	authn, err := auth.NewAuthenticator(deps.Store, deps.State.ClusterKey)
	if err != nil {
		return err
	}
	bearer := func(r *http.Request) error {
		if r.Header.Get("Ax-Proxied") != "" {
			return nil
		}
		tok := auth.BearerFromHeader(r.Header.Get("Authorization"))
		if tok == "" {
			return fmt.Errorf("unauthorized")
		}
		_, err := authn.Authenticate(r.Context(), tok)
		return err
	}
	admin := func(r *http.Request) error {
		tok := auth.BearerFromHeader(r.Header.Get("Authorization"))
		if tok == "" {
			return fmt.Errorf("unauthorized")
		}
		p, err := authn.Authenticate(r.Context(), tok)
		if err != nil {
			return err
		}
		if !p.Allows(auth.LevelAdmin) {
			return fmt.Errorf("admin required")
		}
		return nil
	}
	coll := metrics.NewCollector(deps.Store, deps.Runtime, deps.State.MachineID, deps.Logger)
	mapi := &metrics.API{
		Collector: coll, Store: deps.Store, Self: deps.State.MachineID, Authenticate: bearer,
		PeerFetch: func(ctx context.Context, machineID, serviceID string) ([]metrics.SeriesView, error) {
			m, err := store.GetMachine(ctx, deps.Store, machineID)
			if err != nil {
				return nil, err
			}
			u := "http://" + net.JoinHostPort(m.MeshIp, strconv.Itoa(cluster.DefaultMeshAPIPort)) +
				metrics.Path + "?service_id=" + serviceID + "&local=1"
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
			if err != nil {
				return nil, err
			}
			req.Header.Set("Ax-Proxied", "1")
			res, err := api.H2CClient().Do(req)
			if err != nil {
				return nil, err
			}
			defer res.Body.Close()
			if res.StatusCode/100 != 2 {
				return nil, fmt.Errorf("peer metrics: %s", res.Status)
			}
			var body metrics.Response
			if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
				return nil, err
			}
			return body.Series, nil
		},
	}
	bm := backup.NewManager(deps.Store, deps.Runtime, deps.State.MachineID, deps.Logger)
	bm.Leases = deps.Leases
	bm.Identity = func() *snapshot.Identity {
		if deps.State == nil {
			return nil
		}
		return &snapshot.Identity{
			ClusterID:  deps.State.ClusterID,
			ClusterKey: deps.State.ClusterKey,
			CACertPEM:  deps.State.CACertPEM,
			CAKeyPEM:   deps.State.CAKeyPEM,
		}
	}
	bapi := &backup.API{Store: deps.Store, Manager: bm, Authenticate: bearer, RequireAdmin: admin}
	tapi := &backup.TargetsAPI{Store: deps.Store, Authenticate: bearer, RequireAdmin: admin}
	capi := &cron.API{Store: deps.Store, Authenticate: bearer}
	napi := &notify.API{Store: deps.Store, Authenticate: bearer}

	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == templates.Path || strings.HasPrefix(r.URL.Path, templates.Path+"/"):
			templates.Handler(bearer).ServeHTTP(w, r)
		case r.URL.Path == metrics.Path || strings.HasPrefix(r.URL.Path, metrics.Path+"/"):
			mapi.Handler().ServeHTTP(w, r)
		case r.URL.Path == backup.TargetsPath || strings.HasPrefix(r.URL.Path, backup.TargetsPath+"/"):
			tapi.Handler().ServeHTTP(w, r)
		case r.URL.Path == backup.Path || strings.HasPrefix(r.URL.Path, backup.Path+"/"):
			bapi.Handler().ServeHTTP(w, r)
		case r.URL.Path == cron.Path || strings.HasPrefix(r.URL.Path, cron.Path+"/"):
			capi.Handler().ServeHTTP(w, r)
		case r.URL.Path == notify.Path || strings.HasPrefix(r.URL.Path, notify.Path+"/"):
			napi.Handler().ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	})
	var hh http.Handler = root
	h.Store(&hh)

	go coll.Run(ctx)
	go bm.Run(ctx)
	go cron.NewRunner(deps.Store, deps.State.MachineID, deps.Logger).Run(ctx)
	go notify.NewDispatcher(deps.Store, deps.Logger).Run(ctx)

	<-ctx.Done()
	return nil
}
