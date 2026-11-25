package ingress

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/lease"
	"github.com/aetherix/aetherixcloud/internal/store"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Controller keeps the local Caddy in sync with the store.
type Controller struct {
	Store  store.Store
	Leases *lease.Manager
	Self   string // machine ID
	// Options is the base option set; Storage/Dev/DNS01 etc. come from here.
	Options Options
	Admin   *AdminClient
	// Storage (optional) lets the controller report certificate state.
	Storage  *Storage
	Logger   *slog.Logger
	Interval time.Duration // default 2s
	TTL      time.Duration // lease TTL, default 15s
	// Extra supplies additional routes (dashboard, MCP).
	Extra func(ctx context.Context) []ExtraRoute
	Now   func() time.Time

	mu          sync.Mutex
	lastHash    string
	lastApplied int64
	lastRoutes  []Route
}

func (c *Controller) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Controller) log() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

// Invalidate forces the next Reconcile to push (Caddy restarted).
func (c *Controller) Invalidate() {
	c.mu.Lock()
	c.lastHash = ""
	c.mu.Unlock()
}

// Routes returns the routes of the last pushed config.
func (c *Controller) Routes() []Route {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Route(nil), c.lastRoutes...)
}

// Snapshot reads everything Generate needs from the store.
func (c *Controller) Snapshot(ctx context.Context) (Input, error) {
	in := Input{Options: c.Options, Services: map[string]*v1.Service{}, Deployments: map[string]*v1.Deployment{},
		Machines: map[string]*v1.Machine{}, Draining: map[string]bool{}}
	var err error
	if in.Domains, err = store.ListDomains(ctx, c.Store, ""); err != nil {
		return in, err
	}
	svcs, err := store.ListServices(ctx, c.Store, nil)
	if err != nil {
		return in, err
	}
	for _, s := range svcs {
		in.Services[s.Id] = s
	}
	ms, err := store.ListMachines(ctx, c.Store)
	if err != nil {
		return in, err
	}
	for _, m := range ms {
		in.Machines[m.Id] = m
	}
	if in.Replicas, err = store.ListReplicas(ctx, c.Store, nil); err != nil {
		return in, err
	}
	for _, r := range in.Replicas {
		if _, ok := in.Deployments[r.DeploymentId]; !ok {
			if d, err := store.GetDeployment(ctx, c.Store, r.DeploymentId); err == nil {
				in.Deployments[r.DeploymentId] = d
			}
		}
	}
	drains, err := Drains(ctx, c.Store)
	if err != nil {
		return in, err
	}
	for id := range drains {
		in.Draining[id] = true
	}
	in.Extra = registeredExtras(ctx)
	if c.Extra != nil {
		in.Extra = append(in.Extra, c.Extra(ctx)...)
	}
	return in, nil
}

// Reconcile generates the config and pushes it if it changed. It reports
// whether a push happened.
func (c *Controller) Reconcile(ctx context.Context) (bool, error) {
	self, err := store.GetMachine(ctx, c.Store, c.Self)
	if err == nil && !IsIngress(self) {
		return false, nil
	}
	snapshotAt := c.now()
	in, err := c.Snapshot(ctx)
	if err != nil {
		return false, err
	}
	cfg, routes := Generate(in)
	raw, err := json.Marshal(cfg)
	if err != nil {
		return false, err
	}
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])

	c.mu.Lock()
	same := hash == c.lastHash
	c.mu.Unlock()
	if same {
		// keep drain acknowledgements flowing without churning the store
		c.mu.Lock()
		last := c.lastApplied
		c.mu.Unlock()
		if len(drainMarksNewerThan(ctx, c.Store, last)) > 0 {
			if err := recordApplied(ctx, c.Store, c.Self, snapshotAt); err == nil {
				c.mu.Lock()
				c.lastApplied = snapshotAt.UnixNano()
				c.mu.Unlock()
			}
		}
		return false, nil
	}
	if err := c.Admin.Load(ctx, cfg); err != nil {
		return false, err
	}
	c.mu.Lock()
	c.lastHash, c.lastRoutes, c.lastApplied = hash, routes, snapshotAt.UnixNano()
	c.mu.Unlock()
	if err := recordApplied(ctx, c.Store, c.Self, snapshotAt); err != nil {
		c.log().Warn("record ingress applied", "err", err)
	}
	c.log().Info("caddy config pushed", "routes", len(routes))
	return true, nil
}

func drainMarksNewerThan(ctx context.Context, st store.Store, at int64) []DrainMark {
	ds, err := Drains(ctx, st)
	if err != nil {
		return nil
	}
	var out []DrainMark
	for _, d := range ds {
		if d.At > at {
			out = append(out, d)
		}
	}
	return out
}

// HostOptions derives hostname options from the cluster row and machines.
func (c *Controller) hostOptions(ctx context.Context) (HostOptions, error) {
	o := HostOptions{Dev: c.Options.Dev}
	if cl, err := store.GetCluster(ctx, c.Store); err == nil {
		o.ClusterDomain = cl.Domain
	}
	ms, err := store.ListMachines(ctx, c.Store)
	if err != nil {
		return o, err
	}
	if ips := IngressIPs(ms); len(ips) > 0 {
		o.PublicIP = ips[0]
	}
	return o, nil
}

// MaintainDomains creates default domains and refreshes certificate state.
// Run under the "ingress-domains" lease.
func (c *Controller) MaintainDomains(ctx context.Context) error {
	ho, err := c.hostOptions(ctx)
	if err != nil {
		return err
	}
	if _, err := EnsureDomains(ctx, c.Store, ho); err != nil {
		return err
	}
	if c.Storage == nil {
		return nil
	}
	domains, err := store.ListDomains(ctx, c.Store, "")
	if err != nil {
		return err
	}
	for _, d := range domains {
		exp, ok := c.Storage.CertExpiry(ctx, d.Hostname)
		want, wantExp := "pending", (*timestamppb.Timestamp)(nil)
		if ok && exp.After(c.now()) {
			want, wantExp = "active", timestamppb.New(exp)
		}
		if d.TlsState == want && proto.Equal(d.TlsExpiresAt, wantExp) {
			continue
		}
		n := proto.Clone(d).(*v1.Domain)
		n.TlsState, n.TlsExpiresAt = want, wantExp
		if err := store.PutDomain(ctx, c.Store, n); err != nil {
			return err
		}
	}
	return nil
}

// Run blocks until ctx is done: the config pusher runs under the lease
// "ingress:<machine>", domain maintenance under "ingress-domains".
func (c *Controller) Run(ctx context.Context) {
	ttl := c.TTL
	if ttl == 0 {
		ttl = 15 * time.Second
	}
	iv := c.Interval
	if iv == 0 {
		iv = 2 * time.Second
	}
	go c.Leases.Run(ctx, "ingress-domains", ttl, func(ctx context.Context, _ *lease.Lease) {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			if err := c.MaintainDomains(ctx); err != nil && ctx.Err() == nil {
				c.log().Warn("maintain domains", "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	})
	c.Leases.Run(ctx, "ingress:"+c.Self, ttl, func(ctx context.Context, _ *lease.Lease) {
		trigger := make(chan struct{}, 1)
		for _, tbl := range []string{store.TableDomains, store.TableReplicas, store.TableServices, store.TableMachines, store.TableSettings} {
			ch, err := c.Store.Watch(ctx, tbl)
			if err != nil {
				continue
			}
			go func(tbl string) {
				for ch2 := range ch {
					if tbl == store.TableSettings && !strings.HasPrefix(ch2.ID, prefixDrain) {
						continue
					}
					select {
					case trigger <- struct{}{}:
					default:
					}
				}
			}(tbl)
		}
		t := time.NewTicker(iv)
		defer t.Stop()
		for {
			if _, err := c.Reconcile(ctx); err != nil && ctx.Err() == nil {
				c.log().Debug("ingress reconcile", "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			case <-trigger:
				time.Sleep(150 * time.Millisecond) // coalesce bursts
			}
		}
	})
}
