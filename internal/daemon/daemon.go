// Package daemon wires every component of aetherixd together:
// store (Corrosion or SQLite) -> mesh -> membership -> runtime/reconciler/
// scheduler controller -> DNS -> registry -> API listeners.
//
// OWNERSHIP of hooks for later workers (see Daemon.Hooks): ingress (Caddy
// config generation), rolling deploys, builds, GitHub webhooks, MCP/OAuth and
// the embedded web UI register themselves by adding HTTP handlers / run
// functions through Hooks without editing this file's core flow.
package daemon

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"sync"
	"time"

	"github.com/aetherix/aetherixcloud/internal/api"
	"github.com/aetherix/aetherixcloud/internal/cluster"
	"github.com/aetherix/aetherixcloud/internal/deploy"
	"github.com/aetherix/aetherixcloud/internal/dns"
	"github.com/aetherix/aetherixcloud/internal/lease"
	"github.com/aetherix/aetherixcloud/internal/membership"
	"github.com/aetherix/aetherixcloud/internal/mesh"
	"github.com/aetherix/aetherixcloud/internal/pki"
	"github.com/aetherix/aetherixcloud/internal/reconciler"
	"github.com/aetherix/aetherixcloud/internal/registry"
	"github.com/aetherix/aetherixcloud/internal/runtime"
	"github.com/aetherix/aetherixcloud/internal/secrets"
	"github.com/aetherix/aetherixcloud/internal/snapshot"
	"github.com/aetherix/aetherixcloud/internal/store"
	"github.com/aetherix/aetherixcloud/internal/upgrade"
	"golang.org/x/net/http2"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Config configures the daemon.
type Config struct {
	DataDir string
	// Store: "corrosion" (default if the binary exists), "sqlite".
	Store        string
	CorrosionBin string
	// ListenPublic is the TLS API address (default :8443).
	ListenPublic string
	// MeshHTTPPort is the plain-HTTP port bound to loopback and the mesh IP.
	MeshHTTPPort int
	// NoMesh skips WireGuard, DNS on the mesh IP and mesh binds (single-node
	// development on a machine without WireGuard).
	NoMesh bool
	// NoDocker disables the container runtime, reconciler and registry.
	NoDocker bool
	// FastMembership shortens failure detection (dev clusters).
	FastMembership bool
	Version        string
	Logger         *slog.Logger
}

// Hooks lets later workers extend the daemon.
type Hooks struct {
	// Handlers are mounted on BOTH HTTP listeners (mesh and public) before the
	// API handler: path prefix -> handler. Used for /mcp, /api/github/webhook,
	// OAuth endpoints and the embedded SPA (mount "/" last).
	Handlers map[string]http.Handler
	// Services run until shutdown once the core is up. They receive the
	// shared dependencies (store, leases, runtime, membership...).
	Services []func(ctx context.Context, d *Deps) error
	// Fallback, when set, wraps the API handler mounted at "/". It receives
	// the API handler as next and decides what to serve itself (the embedded
	// SPA) and what to pass through. Needed because "/" can't be registered
	// twice in Handlers.
	Fallback func(next http.Handler) http.Handler
}

// Deps is what the daemon exposes to hook services.
type Deps struct {
	Store      store.Store
	Leases     *lease.Manager
	Runtime    runtime.Runtime // nil with NoDocker
	Members    *membership.Membership
	State      *cluster.NodeState
	Reconciler *reconciler.Reconciler
	Logger     *slog.Logger
}

// Daemon is a running node.
type Daemon struct {
	cfg   Config
	Hooks Hooks
}

// New creates a daemon.
func New(cfg Config) *Daemon {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.ListenPublic == "" {
		cfg.ListenPublic = fmt.Sprintf(":%d", cluster.DefaultAPIPort)
	}
	if cfg.MeshHTTPPort == 0 {
		cfg.MeshHTTPPort = cluster.DefaultMeshAPIPort
	}
	if cfg.Store == "" {
		cfg.Store = "corrosion"
		if cfg.CorrosionBin == "" {
			if p, err := exec.LookPath("corrosion"); err == nil {
				cfg.CorrosionBin = p
			} else {
				cfg.Store = "sqlite"
			}
		}
	}
	return &Daemon{cfg: cfg, Hooks: Hooks{Handlers: map[string]http.Handler{}}}
}

// Run starts all components and blocks until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	cfg, log := d.cfg, d.cfg.Logger
	state, err := cluster.LoadState(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("load node state (run `aetherixd init` or `aetherixd join` first): %w", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	spawn := func(name string, fn func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(ctx); err != nil && ctx.Err() == nil {
				log.Error("component failed", "component", name, "err", err)
			}
		}()
	}
	meshIP := state.MeshIP

	// ---- mesh
	var mm *mesh.Manager
	if !cfg.NoMesh {
		subnet, _ := parsePrefix(state.Subnet)
		network, _ := parsePrefix(state.Network)
		mcfg := mesh.Config{PrivateKey: state.WGPrivateKey, ListenPort: state.WGPort, Address: mesh.InterfaceAddress(subnet, network)}
		for _, p := range state.Peers {
			if sn, err := parsePrefix(p.Subnet); err == nil {
				mcfg.Peers = append(mcfg.Peers, mesh.Peer{ID: p.MachineID, PublicKey: p.PublicKey, Endpoints: p.Endpoints,
					AllowedIPs: []netipPrefix{sn}, Keepalive: 25 * time.Second})
			}
		}
		mm, err = mesh.NewSystem(mcfg)
		if err != nil {
			return err
		}
		if err := mm.Up(ctx); err != nil {
			return fmt.Errorf("bring up mesh: %w", err)
		}
		log.Info("mesh up", "impl", mm.Implementation(), "ip", meshIP)
		spawn("mesh-rotation", func(ctx context.Context) error { mm.Run(ctx); return nil })
	}

	// ---- store
	var st store.Store
	switch cfg.Store {
	case "corrosion":
		cr := store.CorrosionRuntime{Binary: cfg.CorrosionBin, Dir: filepath.Join(cfg.DataDir, "corrosion"),
			GossipAddr: fmt.Sprintf("%s:%d", meshIPOrAny(meshIP, cfg.NoMesh), cluster.DefaultCorrosionGossip),
			API:        cluster.DefaultCorrosionAPI, Bootstrap: state.StoreBootstrap}
		spawn("corrosion", func(ctx context.Context) error { return cr.Supervise(ctx, log) })
		if err := store.WaitCorrosionReady(ctx, cr.API, 60*time.Second); err != nil {
			return err
		}
		st = store.OpenCorrosion(store.CorrosionConfig{API: "http://" + cr.API})
	default:
		st, err = store.OpenSQLite(filepath.Join(cfg.DataDir, "store.db"))
		if err != nil {
			return err
		}
		if len(state.Peers) > 0 {
			log.Warn("sqlite store does NOT replicate: this machine will not see cluster state; install Corrosion")
		}
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate store: %w", err)
	}
	pending := filepath.Join(cfg.DataDir, snapshot.PendingFile)
	if _, err := os.Stat(pending); err == nil {
		restored, err := state.ApplyPendingRestore(ctx, st, cfg.DataDir)
		if err != nil {
			return fmt.Errorf("restore from snapshot: %w", err)
		}
		if err := state.Save(cfg.DataDir); err != nil {
			return err
		}
		if restored {
			log.Info("cluster restored from snapshot", "cluster", state.ClusterName, "machine", state.MachineName)
		}
	} else if state.Bootstrap != nil {
		if err := state.ApplyBootstrap(ctx, st); err != nil {
			return fmt.Errorf("bootstrap cluster rows: %w", err)
		}
		if err := state.Save(cfg.DataDir); err != nil {
			return err
		}
		log.Info("cluster bootstrapped", "cluster", state.ClusterName)
	}
	key, err := secrets.ParseKey(state.ClusterKey)
	if err != nil {
		return err
	}

	// ---- mesh peer sync from the store
	if mm != nil {
		spawn("mesh-sync", func(ctx context.Context) error { return syncPeers(ctx, st, mm, state.MachineID) })
	}

	// ---- membership
	gossipKey := sha256.Sum256(append([]byte("aetherix-gossip:"), key[:]...))
	mem, err := membership.New(membership.Config{MachineID: state.MachineID, BindAddr: meshIPOrAny(meshIP, cfg.NoMesh),
		SecretKey: gossipKey[:], Fast: cfg.FastMembership, Logger: log})
	if err != nil {
		return err
	}
	defer mem.Leave(2 * time.Second)
	spawn("membership-join", func(ctx context.Context) error { joinPeers(ctx, st, mem, state.MachineID, log); return nil })

	leases := lease.NewManager(st, state.MachineID)

	// ---- runtime + reconciler + controller
	var rt runtime.Runtime
	var rec *reconciler.Reconciler
	if !cfg.NoDocker {
		dk, err := runtime.NewDocker()
		if err != nil {
			return fmt.Errorf("docker: %w (use --no-docker to run without containers)", err)
		}
		rt = dk
		defer rt.Close()
		subnet, _ := parsePrefix(state.Subnet)
		if err := waitDocker(ctx, dk); err != nil {
			return err
		}
		if err := rt.EnsureNetwork(ctx, runtime.NetworkSpec{Name: runtime.NetworkName, Subnet: subnet.String(),
			Gateway: mesh.BridgeGateway(subnet).String(), IPRange: mesh.ContainerRange(subnet).String()}); err != nil {
			return fmt.Errorf("create network: %w", err)
		}
		rec = reconciler.New(reconciler.Config{Store: st, Runtime: rt, MachineID: state.MachineID, ClusterID: state.ClusterID,
			DNSServers: []string{meshIP}, Logger: log, Env: deploy.ResolveEnv(st, key),
			Puller: registry.PeerPuller{Store: st, Self: state.MachineID, Logger: log}})
		spawn("reconciler", func(ctx context.Context) error { return rec.Run(ctx) })
		ctl := &reconciler.Controller{Store: st, Leases: leases, Logger: log, OnChange: nil}
		spawn("controller", func(ctx context.Context) error { ctl.Run(ctx); return nil })
		reg := &registry.Server{Dir: filepath.Join(cfg.DataDir, "registry"), Addrs: []string{registry.LocalHost}, Logger: log}
		if !cfg.NoMesh {
			reg.Addrs = append(reg.Addrs, fmt.Sprintf("%s:%d", meshIP, registry.Port))
		}
		spawn("registry", reg.Run)
	}

	// ---- membership state -> store (single writer under a lease)
	spawn("membership-sync", func(ctx context.Context) error {
		sy := &membership.Syncer{Mem: mem, Store: st, Logger: log}
		leases.Run(ctx, "membership-sync", 15*time.Second, func(ctx context.Context, _ *lease.Lease) { sy.Run(ctx) })
		return nil
	})
	spawn("heartbeat", func(ctx context.Context) error {
		heartbeat(ctx, st, state.MachineID, cfg.Version, goruntime.GOARCH)
		return nil
	})
	spawn("upgrade", func(ctx context.Context) error {
		w := &upgrade.Watcher{Store: st, Self: state.MachineID, Version: cfg.Version, Arch: goruntime.GOARCH, Logger: log}
		return w.Run(ctx)
	})

	// ---- DNS
	if !cfg.NoMesh {
		dsrv := &dns.Server{Store: st, Logger: log}
		spawn("dns", func(ctx context.Context) error { return dsrv.ListenAndServe(ctx, net.JoinHostPort(meshIP, "53")) })
	}

	// ---- API
	deps := api.Deps{Store: st, Leases: leases, State: state, Runtime: rt, Members: mem, Version: cfg.Version, Logger: log}
	if rec != nil {
		deps.OnChange = rec.Trigger
	}
	hdeps := &Deps{Store: st, Leases: leases, Runtime: rt, Members: mem, State: state, Reconciler: rec, Logger: log}
	for _, svc := range d.Hooks.Services {
		spawn("hook-service", func(ctx context.Context) error { return svc(ctx, hdeps) })
	}
	mux := func(inner http.Handler) http.Handler {
		m := http.NewServeMux()
		for p, h := range d.Hooks.Handlers {
			m.Handle(p, h)
		}
		if d.Hooks.Fallback != nil {
			inner = d.Hooks.Fallback(inner)
		}
		m.Handle("/", inner)
		return m
	}
	meshHandler := api.H2CHandler(mux(api.Handler(deps)))
	publicHandler := mux(api.PublicHandler(deps))

	ca, err := pki.LoadCA([]byte(state.CACertPEM), []byte(state.CAKeyPEM))
	if err != nil {
		return err
	}
	hosts := append([]string{"localhost", "127.0.0.1", meshIP}, state.Advertise...)
	cert, err := ca.Issue(state.MachineName, hosts)
	if err != nil {
		return err
	}
	pub := &http.Server{Addr: cfg.ListenPublic, Handler: publicHandler,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12, NextProtos: []string{"h2", "http/1.1"}}}
	_ = http2.ConfigureServer(pub, &http2.Server{})
	spawn("api-public", func(ctx context.Context) error { return serve(ctx, pub, true) })
	binds := []string{"127.0.0.1"}
	if !cfg.NoMesh {
		binds = append(binds, meshIP)
	}
	for _, b := range binds {
		s := &http.Server{Addr: net.JoinHostPort(b, fmt.Sprint(cfg.MeshHTTPPort)), Handler: meshHandler}
		spawn("api-mesh", func(ctx context.Context) error { return serve(ctx, s, false) })
	}
	log.Info("aetherixd running", "machine", state.MachineName, "cluster", state.ClusterName, "store", cfg.Store, "api", cfg.ListenPublic)

	<-ctx.Done()
	cancel()
	wg.Wait()
	if mm != nil {
		_ = mm.Down()
	}
	return nil
}

func serve(ctx context.Context, s *http.Server, tlsOn bool) error {
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Shutdown(c)
	}()
	var err error
	if tlsOn {
		err = s.ListenAndServeTLS("", "")
	} else {
		err = s.ListenAndServe()
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func meshIPOrAny(ip string, noMesh bool) string {
	if noMesh {
		return "0.0.0.0"
	}
	return ip
}

func waitDocker(ctx context.Context, d *runtime.Docker) error {
	var last error
	for i := 0; i < 60; i++ { // dockerd inside a node container may take a while
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		_, last = d.Client().Ping(pctx)
		cancel()
		if last == nil {
			return nil
		}
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return fmt.Errorf("docker not reachable: %w", last)
}

// syncPeers keeps WireGuard peers equal to the machines table.
func syncPeers(ctx context.Context, st store.Store, mm *mesh.Manager, self string) error {
	apply := func() {
		ms, err := store.ListMachines(ctx, st)
		if err != nil {
			return
		}
		peers := mesh.PeersFromMachines(self, ms)
		// Join configures WireGuard from state.Peers before the local store has
		// replicated any rows. An empty PeersFromMachines result must not wipe
		// those join-time peers (otherwise Corrosion/memberlist over the mesh
		// never come up and the wipe becomes permanent).
		if len(peers) == 0 {
			return
		}
		_ = mm.SetPeers(ctx, peers)
	}
	ch, err := st.Watch(ctx, store.TableMachines)
	if err != nil {
		return err
	}
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		apply()
		select {
		case <-ctx.Done():
			return nil
		case <-ch:
			time.Sleep(200 * time.Millisecond) // coalesce bursts
		case <-t.C:
		}
	}
}

// joinPeers keeps trying to contact other machines' gossip ports.
func joinPeers(ctx context.Context, st store.Store, mem *membership.Membership, self string, log *slog.Logger) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		ms, _ := store.ListMachines(ctx, st)
		var addrs []string
		for _, m := range ms {
			if m.Id != self && m.MeshIp != "" && mem.State(m.Id) != membership.StateUp {
				addrs = append(addrs, net.JoinHostPort(m.MeshIp, fmt.Sprint(membership.DefaultPort)))
			}
		}
		if len(addrs) > 0 {
			if err := mem.Join(addrs); err != nil {
				log.Debug("gossip join incomplete", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// heartbeat publishes this machine's capacity, usage, version and last-seen time.
func heartbeat(ctx context.Context, st store.Store, self, version, arch string) {
	// Prime CPU delta so the first published sample is meaningful.
	_ = usage()
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		cap, used := capacity(), usage()
		if m, err := store.GetMachine(ctx, st, self); err == nil {
			// Preserve upgrade_* from a concurrent watcher write: re-read those
			// fields immediately before put so a stale Get doesn't wipe status
			// (which previously helped drive a downloading ↔ pending_restart loop).
			upgradeStatus, upgradeMessage := m.UpgradeStatus, m.UpgradeMessage
			m.Capacity, m.Used, m.LastSeen = cap, used, timestamppb.Now()
			m.AgentVersion, m.Arch = version, arch
			if latest, err2 := store.GetMachine(ctx, st, self); err2 == nil {
				upgradeStatus, upgradeMessage = latest.UpgradeStatus, latest.UpgradeMessage
			}
			m.UpgradeStatus, m.UpgradeMessage = upgradeStatus, upgradeMessage
			_ = store.PutMachine(ctx, st, m)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

var _ = os.Getenv
