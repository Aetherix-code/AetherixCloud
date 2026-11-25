package store

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// CorrosionRuntime describes the files and addresses of a Corrosion agent.
type CorrosionRuntime struct {
	Binary     string   // path to the corrosion binary
	Dir        string   // data dir (db, schema, config live here)
	GossipAddr string   // "meshIP:8787"
	API        string   // "127.0.0.1:8081"
	Bootstrap  []string // peers' gossip addresses
}

// ConfigTOML renders the Corrosion agent configuration.
func (c CorrosionRuntime) ConfigTOML() string {
	var bs []string
	for _, b := range c.Bootstrap {
		bs = append(bs, fmt.Sprintf("%q", b))
	}
	return fmt.Sprintf(`[db]
path = %q
schema_paths = [%q]

[gossip]
addr = %q
bootstrap = [%s]
plaintext = true # traffic only flows inside the WireGuard mesh

[api]
addr = %q

[admin]
path = %q
`, filepath.Join(c.Dir, "corrosion.db"), filepath.Join(c.Dir, "schema"), c.GossipAddr,
		strings.Join(bs, ", "), c.API, filepath.Join(c.Dir, "admin.sock"))
}

// Prepare writes the schema and config files and returns the config path.
func (c CorrosionRuntime) Prepare() (string, error) {
	if err := WriteCorrosionSchema(filepath.Join(c.Dir, "schema")); err != nil {
		return "", err
	}
	p := filepath.Join(c.Dir, "config.toml")
	return p, os.WriteFile(p, []byte(c.ConfigTOML()), 0o644)
}

// Supervise runs `corrosion agent` and restarts it when it exits, until ctx
// is done.
func (c CorrosionRuntime) Supervise(ctx context.Context, log *slog.Logger) error {
	cfg, err := c.Prepare()
	if err != nil {
		return err
	}
	for ctx.Err() == nil {
		cmd := exec.CommandContext(ctx, c.Binary, "-c", cfg, "agent")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		log.Info("starting corrosion", "bin", c.Binary)
		err := cmd.Run()
		if ctx.Err() != nil {
			return nil
		}
		log.Warn("corrosion exited; restarting", "err", err)
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
		}
	}
	return nil
}

// WaitReady blocks until the Corrosion HTTP API answers or timeout elapses.
func WaitCorrosionReady(ctx context.Context, api string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+api+"/v1/health", nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
			return nil
		}
		select {
		case <-time.After(300 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return fmt.Errorf("corrosion API %s not ready after %s", api, timeout)
}
