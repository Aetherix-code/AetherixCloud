package ingress

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Supervisor runs the caddy binary and restarts it when it exits.
type Supervisor struct {
	Binary    string
	Dir       string // caddy data/config home (local fallback storage)
	AdminAddr string
	Logger    *slog.Logger
	// OnStart is called after each (re)start; the Controller uses it to
	// re-push its config.
	OnStart func()
}

// HasStorageModule reports whether binary was built with the aetherix
// storage module (see deploy/caddy).
func HasStorageModule(ctx context.Context, binary string) bool {
	out, err := exec.CommandContext(ctx, binary, "list-modules").Output()
	return err == nil && strings.Contains(string(out), "caddy.storage.aetherix")
}

// Run blocks until ctx is done.
func (s *Supervisor) Run(ctx context.Context) error {
	log := s.Logger
	if log == nil {
		log = slog.Default()
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	cfgPath := filepath.Join(s.Dir, "bootstrap.json")
	boot, _ := json.Marshal(map[string]any{"admin": map[string]any{"listen": Options{AdminAddr: s.AdminAddr}.admin()}})
	if err := os.WriteFile(cfgPath, boot, 0o600); err != nil {
		return err
	}
	backoff := time.Second
	for ctx.Err() == nil {
		cmd := exec.CommandContext(ctx, s.Binary, "run", "--config", cfgPath)
		cmd.Env = append(os.Environ(), "XDG_DATA_HOME="+filepath.Join(s.Dir, "data"), "XDG_CONFIG_HOME="+filepath.Join(s.Dir, "config"))
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		started := time.Now()
		if err := cmd.Start(); err != nil {
			log.Error("start caddy", "err", err)
		} else {
			if s.OnStart != nil {
				go func() {
					// wait for the admin API, then notify
					c := NewAdminClient(Options{AdminAddr: s.AdminAddr}.admin())
					for i := 0; i < 50 && ctx.Err() == nil; i++ {
						if c.Ready(ctx) {
							s.OnStart()
							return
						}
						time.Sleep(200 * time.Millisecond)
					}
				}()
			}
			err := cmd.Wait()
			if ctx.Err() != nil {
				return nil
			}
			log.Warn("caddy exited", "err", err)
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
	return nil
}
