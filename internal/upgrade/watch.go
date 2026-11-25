package upgrade

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/store"
)

// Watcher applies cluster desired upgrades on this node.
type Watcher struct {
	Store   store.Store
	Self    string
	Version string
	Arch    string
	BinDir  string
	Logger  *slog.Logger
	Apply   func(ctx context.Context, opt ApplyOptions) error // override in tests

	mu       sync.Mutex
	running  bool
	interval time.Duration
}

func (w *Watcher) log() *slog.Logger {
	if w.Logger != nil {
		return w.Logger
	}
	return slog.Default()
}

// Run polls upgrade settings until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) error {
	if w.interval <= 0 {
		w.interval = 20 * time.Second
	}
	w.reconcileLocal(ctx)

	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		w.tick(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

func (w *Watcher) tick(ctx context.Context) {
	cfg, err := GetConfig(ctx, w.Store)
	if err != nil {
		w.log().Warn("upgrade: load config", "err", err)
		return
	}
	if !cfg.Targets(w.Self) {
		w.clearStatusIfIdle(ctx)
		return
	}
	if Equal(w.Version, cfg.DesiredVersion) {
		w.clearStatusOnTarget(ctx)
		return
	}
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		return
	}
	w.running = true
	w.mu.Unlock()

	go func() {
		defer func() {
			w.mu.Lock()
			w.running = false
			w.mu.Unlock()
		}()
		w.applyOnce(ctx, cfg)
	}()
}

func (w *Watcher) applyOnce(ctx context.Context, cfg Config) {
	log := w.log()
	_ = w.setStatus(ctx, "downloading", "fetching "+Normalize(cfg.DesiredVersion))
	apply := w.Apply
	if apply == nil {
		apply = Apply
	}
	err := apply(ctx, ApplyOptions{
		Version:      cfg.DesiredVersion,
		Arch:         w.Arch,
		DownloadBase: cfg.DownloadBase,
		Repo:         cfg.Repo,
		BinDir:       w.BinDir,
		Logger:       log,
		Restart: func(l *slog.Logger) error {
			_ = w.setStatus(ctx, "restarting", "restarting aetherixd")
			return DefaultRestart(l)
		},
	})
	if err == nil {
		return
	}
	if errors.Is(err, ErrPendingRestart) {
		_ = w.setStatus(ctx, "pending_restart", "binaries installed — restart this node to finish")
		return
	}
	log.Error("upgrade failed", "err", err)
	_ = w.setStatus(ctx, "failed", err.Error())
}

func (w *Watcher) reconcileLocal(ctx context.Context) {
	cfg, _ := GetConfig(ctx, w.Store)
	if cfg.DesiredVersion != "" && Equal(w.Version, cfg.DesiredVersion) && cfg.Targets(w.Self) {
		w.clearStatusOnTarget(ctx)
	}
}

func (w *Watcher) clearStatusOnTarget(ctx context.Context) {
	m, err := store.GetMachine(ctx, w.Store, w.Self)
	if err != nil {
		return
	}
	if m.UpgradeStatus == "" && m.AgentVersion == w.Version && m.Arch == w.Arch {
		return
	}
	m.AgentVersion, m.Arch = w.Version, w.Arch
	m.UpgradeStatus, m.UpgradeMessage = "", ""
	_ = store.PutMachine(ctx, w.Store, m)
}

func (w *Watcher) clearStatusIfIdle(ctx context.Context) {
	m, err := store.GetMachine(ctx, w.Store, w.Self)
	if err != nil {
		return
	}
	if m.UpgradeStatus == "" || m.UpgradeStatus == "failed" {
		return
	}
	m.UpgradeStatus, m.UpgradeMessage = "", ""
	_ = store.PutMachine(ctx, w.Store, m)
}

func (w *Watcher) setStatus(ctx context.Context, status, msg string) error {
	m, err := store.GetMachine(ctx, w.Store, w.Self)
	if err != nil {
		return err
	}
	m.UpgradeStatus, m.UpgradeMessage = status, msg
	m.AgentVersion, m.Arch = w.Version, w.Arch
	return store.PutMachine(ctx, w.Store, m)
}

// StatusPayload builds GetUpgrade machine rows from store state.
func StatusPayload(ctx context.Context, st store.Store, cfg Config) (machines []*v1.MachineUpgrade, onTarget, total int32, err error) {
	ms, err := store.ListMachines(ctx, st)
	if err != nil {
		return nil, 0, 0, err
	}
	want := map[string]bool{}
	for _, id := range cfg.MachineIDs {
		want[id] = true
	}
	for _, m := range ms {
		if len(want) > 0 && !want[m.Id] {
			continue
		}
		total++
		ok := cfg.DesiredVersion != "" && Equal(m.AgentVersion, cfg.DesiredVersion)
		if ok {
			onTarget++
		}
		machines = append(machines, &v1.MachineUpgrade{
			MachineId:      m.Id,
			MachineName:    m.Name,
			AgentVersion:   m.AgentVersion,
			Arch:           m.Arch,
			UpgradeStatus:  m.UpgradeStatus,
			UpgradeMessage: m.UpgradeMessage,
			OnTarget:       ok,
		})
	}
	return machines, onTarget, total, nil
}
