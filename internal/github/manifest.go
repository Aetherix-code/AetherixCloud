package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aetherix/aetherixcloud/internal/store"
)

// Paths served by the module.
const (
	WebhookPath          = "/api/github/webhook"
	ManifestCallbackPath = "/api/github/manifest/callback"
)

type manifestState struct {
	Expires int64  `json:"expires"`
	BaseURL string `json:"base_url"`
	// Completed is set after a successful code exchange so a retried GitHub
	// callback (or a second CompleteManifestSetup) is idempotent.
	Completed bool `json:"completed,omitempty"`
}

// How long CompleteManifest waits for a pending state row to appear. Begin and
// callback may land on different nodes; Corrosion gossip is usually sub-second
// but a short poll covers brief lag without failing the whole setup.
const manifestStateWait = 3 * time.Second

// BuildManifest returns the GitHub App manifest for base (the public
// dashboard URL) and the URL it must be POSTed to.
func BuildManifest(webBase, clusterName, base, org, state string) (manifestJSON, postURL string) {
	name := "AetherixCloud-" + strings.NewReplacer(" ", "-", "_", "-").Replace(clusterName)
	if len(name) > 28 {
		name = name[:28]
	}
	name += "-" + randHex(2)
	manifest := map[string]any{
		"name":         name,
		"url":          base,
		"redirect_url": base + ManifestCallbackPath,
		"setup_url":    base + "/settings/github",
		"hook_attributes": map[string]any{
			"url":    base + WebhookPath,
			"active": true,
		},
		"public": false,
		"default_permissions": map[string]string{
			"contents":      "read",
			"metadata":      "read",
			"pull_requests": "write",
			"statuses":      "write",
			"checks":        "write",
		},
		"default_events": []string{"push", "pull_request"},
	}
	b, _ := json.Marshal(manifest)
	webBase = strings.TrimRight(webBase, "/")
	if org != "" {
		postURL = fmt.Sprintf("%s/organizations/%s/settings/apps/new?state=%s", webBase, url.PathEscape(org), url.QueryEscape(state))
	} else {
		postURL = fmt.Sprintf("%s/settings/apps/new?state=%s", webBase, url.QueryEscape(state))
	}
	return string(b), postURL
}

// BeginManifest creates a one-hour state token and the manifest.
func (m *Module) BeginManifest(ctx context.Context, baseURL, org string) (manifestJSON, postURL, state string, err error) {
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" {
		baseURL = strings.TrimRight(m.dashboard(ctx), "/")
	}
	if baseURL == "" {
		return "", "", "", errors.New("base_url is required (the public dashboard URL)")
	}
	state = randHex(16)
	if err := store.PutJSON(ctx, m.Store, store.TableSettings, manifestPrefix+state, nil,
		manifestState{Expires: m.now().Add(manifestValidFor).Unix(), BaseURL: baseURL}); err != nil {
		return "", "", "", err
	}
	cl := "cluster"
	if c, err := store.GetCluster(ctx, m.Store); err == nil && c.Name != "" {
		cl = c.Name
	}
	manifestJSON, postURL = BuildManifest(m.webBase(), cl, baseURL, org, state)
	return manifestJSON, postURL, state, nil
}

// CompleteManifest validates state, exchanges code and stores the App.
// Pending state lives in the replicated settings table (not process memory) so
// any node can finish the callback. The row is marked completed (not deleted)
// so GitHub retries and double submits are idempotent.
func (m *Module) CompleteManifest(ctx context.Context, code, state string) (*AppConfig, string, error) {
	if code == "" || state == "" {
		return nil, "", errors.New("code and state are required")
	}
	key := manifestPrefix + state
	st, err := m.loadManifestState(ctx, key)
	if err != nil {
		return nil, "", errors.New("unknown or already used state")
	}
	if m.now().Unix() > st.Expires {
		_ = m.Store.Delete(ctx, store.TableSettings, key)
		return nil, "", errors.New("manifest state expired: start the setup again")
	}
	if st.Completed {
		cfg, err := m.LoadApp(ctx)
		if err != nil {
			return nil, "", fmt.Errorf("manifest already used but app missing: %w", err)
		}
		return cfg, st.BaseURL, nil
	}
	c := &Client{APIBase: m.APIBase, HTTP: m.HTTP}
	mc, err := c.ConvertManifest(ctx, code)
	if err != nil {
		return nil, "", fmt.Errorf("exchange manifest code: %w", err)
	}
	cfg, err := m.storeConversion(ctx, mc, st.BaseURL+WebhookPath)
	if err != nil {
		return nil, "", err
	}
	st.Completed = true
	if err := store.PutJSON(ctx, m.Store, store.TableSettings, key, nil, st); err != nil {
		m.logger().Debug("mark manifest state completed", "err", err)
	}
	if err := m.SyncInstallations(ctx); err != nil {
		m.logger().Debug("sync installations after setup", "err", err)
	}
	return cfg, st.BaseURL, nil
}

// loadManifestState reads the pending/completed row, briefly retrying NotFound
// so a callback handled on another node can see a Begin that just replicated.
// Wall-clock time is used for the wait (not m.Now) so tests can freeze Now
// without hanging; Module.ManifestStateWait overrides the duration.
func (m *Module) loadManifestState(ctx context.Context, key string) (*manifestState, error) {
	wait := manifestStateWait
	if m.ManifestStateWait != 0 {
		wait = m.ManifestStateWait
	}
	if wait < 0 {
		wait = 0
	}
	deadline := time.Now().Add(wait)
	var last error
	for {
		st, err := store.GetJSON[manifestState](ctx, m.Store, store.TableSettings, key)
		if err == nil {
			return st, nil
		}
		last = err
		if !store.IsNotFound(err) {
			return nil, err
		}
		if wait == 0 || !time.Now().Before(deadline) {
			return nil, last
		}
		delay := 100 * time.Millisecond
		if rem := time.Until(deadline); rem < delay {
			delay = rem
		}
		if delay <= 0 {
			return nil, last
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
}

// manifestCallback handles GitHub's redirect after the user created the App.
func (m *Module) manifestCallback(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	cfg, base, err := m.CompleteManifest(ctx, r.URL.Query().Get("code"), r.URL.Query().Get("state"))
	if err != nil {
		http.Error(w, "GitHub App setup failed: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Send the user to the App installation page, then back to the dashboard
	// settings (installations are picked up from webhooks / on demand).
	target := cfg.HTMLURL
	if target == "" {
		target = base
	}
	if cfg.Slug != "" {
		target = strings.TrimRight(m.webBase(), "/") + "/apps/" + cfg.Slug + "/installations/new"
	}
	http.Redirect(w, r, target, http.StatusFound)
}
