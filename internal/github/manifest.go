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
}

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
func (m *Module) CompleteManifest(ctx context.Context, code, state string) (*AppConfig, string, error) {
	if code == "" || state == "" {
		return nil, "", errors.New("code and state are required")
	}
	st, err := store.GetJSON[manifestState](ctx, m.Store, store.TableSettings, manifestPrefix+state)
	if err != nil {
		return nil, "", errors.New("unknown or already used state")
	}
	_ = m.Store.Delete(ctx, store.TableSettings, manifestPrefix+state) // single use
	if m.now().Unix() > st.Expires {
		return nil, "", errors.New("manifest state expired: start the setup again")
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
	if err := m.SyncInstallations(ctx); err != nil {
		m.logger().Debug("sync installations after setup", "err", err)
	}
	return cfg, st.BaseURL, nil
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
