package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/aetherix/aetherixcloud/internal/cluster"
	"github.com/aetherix/aetherixcloud/internal/daemon"
	"github.com/aetherix/aetherixcloud/internal/deploy"
	"github.com/aetherix/aetherixcloud/internal/ingress"
	"github.com/aetherix/aetherixcloud/internal/lease"
	"github.com/aetherix/aetherixcloud/internal/reconciler"
	"github.com/aetherix/aetherixcloud/internal/reschedule"
	"github.com/aetherix/aetherixcloud/internal/secrets"
	"github.com/aetherix/aetherixcloud/internal/store"
)

// hookIngress registers the ingress/rolling-deploy/reschedule worker's parts
// (hooks_ingress.go):
//
//   - the rolling-deploy engine and the reschedule MachineView plug into the
//     placement Controller (reconciler.DefaultRollout / DefaultMachineView);
//   - the Caddy storage endpoint is mounted on the node API;
//   - services: Caddy supervisor + config controller, reschedule controller.
//
// Environment: AX_INGRESS=off disables Caddy; AX_CADDY_BIN, AX_CADDY_ADMIN,
// AX_DEV, AX_ACME_EMAIL, AX_ACME_CA, AX_DNS_CLOUDFLARE_TOKEN, AX_DNS_WILDCARD
// (see ingress.OptionsFromEnv); AX_RESCHEDULE_GRACE (default 60s).
func hookIngress(d *daemon.Daemon) {
	sh := &ingress.StorageHandler{}
	d.Hooks.Handlers[ingress.StoragePath] = sh
	reconciler.DefaultRollout = deploy.NewRollout(deploy.RolloutConfig{})
	reconciler.DefaultMachineView = reschedule.MachineView
	d.Hooks.Services = append(d.Hooks.Services,
		func(ctx context.Context, deps *daemon.Deps) error { return runIngress(ctx, deps, sh) },
		runReschedule,
	)
}

func runReschedule(ctx context.Context, deps *daemon.Deps) error {
	cfg := reschedule.Config{Logger: deps.Logger}
	if v := os.Getenv("AX_RESCHEDULE_GRACE"); v != "" {
		g, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("AX_RESCHEDULE_GRACE: %w", err)
		}
		cfg.Grace = g
	}
	reschedule.New(cfg, deps.Store, deps.Leases, deps.State.MachineID).Run(ctx)
	return nil
}

func runIngress(ctx context.Context, deps *daemon.Deps, sh *ingress.StorageHandler) error {
	log := deps.Logger
	key, err := secrets.ParseKey(deps.State.ClusterKey)
	if err != nil {
		return err
	}
	storage := &ingress.Storage{Store: deps.Store, Key: key, Leases: lease.NewManager(deps.Store, deps.State.MachineID)}
	sh.Bind(storage) // always serve the storage protocol, even if this node has no Caddy

	// TCP proxy does not need Caddy; run whenever this node is an ingress edge
	// (or when roles are empty / unset, matching ingress.IsIngress).
	if os.Getenv("AX_INGRESS") != "off" {
		go (&ingress.TCPProxy{Store: deps.Store, Self: deps.State.MachineID, Logger: log}).Run(ctx)
	}
	if os.Getenv("AX_INGRESS") == "off" {
		return nil
	}
	bin := os.Getenv("AX_CADDY_BIN")
	if bin == "" {
		bin, _ = exec.LookPath("caddy")
	}
	if bin == "" {
		log.Info("caddy binary not found: HTTP ingress disabled on this node (set AX_CADDY_BIN); TCP proxy still runs")
		<-ctx.Done()
		return nil
	}
	domain := ""
	if cl, err := store.GetCluster(ctx, deps.Store); err == nil {
		domain = cl.Domain
	}
	opts := ingress.OptionsFromEnv(os.Getenv, domain)
	if ingress.HasStorageModule(ctx, bin) {
		opts.Storage = &ingress.StorageRef{
			Endpoint: fmt.Sprintf("http://127.0.0.1:%d%s", cluster.DefaultMeshAPIPort, ingress.StoragePath),
			Token:    ingress.StorageToken(key),
		}
	} else {
		log.Warn("caddy was built without the aetherix storage module: certificates are node-local (see deploy/caddy/build.sh)")
	}
	ctl := &ingress.Controller{Store: deps.Store, Leases: lease.NewManager(deps.Store, deps.State.MachineID),
		Self: deps.State.MachineID, Options: opts, Admin: ingress.NewAdminClient(opts.AdminAddr),
		Storage: storage, Logger: log}
	sup := &ingress.Supervisor{Binary: bin, Dir: filepath.Join(os.TempDir(), "aetherix-caddy-"+deps.State.MachineID),
		AdminAddr: opts.AdminAddr, Logger: log, OnStart: ctl.Invalidate}
	if dir := os.Getenv("AX_CADDY_DIR"); dir != "" {
		sup.Dir = dir
	}
	go func() {
		if err := sup.Run(ctx); err != nil && ctx.Err() == nil {
			log.Error("caddy supervisor", "err", err)
		}
	}()
	ctl.Run(ctx)
	return nil
}
