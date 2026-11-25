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

	mu         sync.Mutex
	running    bool
	pendingFor string // desired version already installed; waiting for process restart
	interval   time.Duration
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
		w.mu.Lock()
		w.pendingFor = ""
		w.mu.Unlock()
		w.clearStatusIfIdle(ctx)
		return
	}
	if Equal(w.Version, cfg.DesiredVersion) {
		w.mu.Lock()
		w.pendingFor = ""
		w.mu.Unlock()
		w.clearStatusOnTarget(ctx)
		return
	}
	// Binaries already installed for this target (AX_DEV / manual restart): do not
	// re-download every poll — that caused a pending_restart ↔ downloading loop.
	if w.waitingForRestart(ctx, cfg.DesiredVersion) {
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

// waitingForRestart is true when this node has finished installing DesiredVersion
// and is waiting for aetherixd to come back on the new binary.
func (w *Watcher) waitingForRestart(ctx context.Context, desired string) bool {
	w.mu.Lock()
	pending := w.pendingFor
	w.mu.Unlock()
	if pending != "" && Equal(pending, desired) {
		_ = w.setStatus(ctx, "pending_restart", "binaries installed — restart this node to finish")
		return true
	}
	m, err := store.GetMachine(ctx, w.Store, w.Self)
	if err != nil {
		return false
	}
	switch m.UpgradeStatus {
	case "restarting":
		return true
	case "pending_restart":
		// Adopt store state after process-local pendingFor was lost (or peer view).
		w.mu.Lock()
		if w.pendingFor == "" || Equal(w.pendingFor, desired) {
			w.pendingFor = Normalize(desired)
			w.mu.Unlock()
			return true
		}
		w.mu.Unlock()
		// Desired version changed since the pending install — re-apply.
		return false
	default:
		return false
	}
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
		// Restart was scheduled (systemd / exit). Keep "restarting" until Version matches.
		return
	}
	if errors.Is(err, ErrPendingRestart) {
		w.mu.Lock()
		w.pendingFor = Normalize(cfg.DesiredVersion)
		w.mu.Unlock()
		_ = w.setStatus(ctx, "pending_restart", "binaries installed — restart this node to finish")
		return
	}
	w.mu.Lock()
	w.pendingFor = ""
	w.mu.Unlock()
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
