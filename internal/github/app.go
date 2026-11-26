package github

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/build"
	"github.com/aetherix/aetherixcloud/internal/secrets"
	"github.com/aetherix/aetherixcloud/internal/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Settings-table row IDs.
const (
	appSettingID     = "github_app"
	manifestPrefix   = "ghmanifest/"
	deliveryPrefix   = "ghdelivery/"
	deliveryRetain   = 7 * 24 * time.Hour
	manifestValidFor = time.Hour
)

// AppConfig is the persisted App registration. The private key, webhook
// secret and client secret are sealed with the cluster key (NaCl secretbox).
type AppConfig struct {
	AppID               int64  `json:"app_id"`
	Slug                string `json:"slug"`
	HTMLURL             string `json:"html_url"`
	ClientID            string `json:"client_id"`
	WebhookURL          string `json:"webhook_url"`
	SealedPEM           string `json:"sealed_pem"`
	SealedWebhookSecret string `json:"sealed_webhook_secret"`
	SealedClientSecret  string `json:"sealed_client_secret"`
}

// Module is the GitHub integration of one node.
type Module struct {
	Store store.Store
	Key   secrets.Key
	Self  string
	// APIBase / WebBase default to github.com (override for GHES and tests).
	APIBase string
	WebBase string
	HTTP    *http.Client
	Runner  build.Runner
	// DashboardURL returns the public dashboard URL ("" if unknown); used for
	// the webhook URL, manifest redirect and status target links.
	DashboardURL func(ctx context.Context) string
	// Trigger asks the local reconciler to act now (optional).
	Trigger func()
	Logger  *slog.Logger
	Now     func() time.Time
	// ManifestStateWait overrides how long CompleteManifest polls for a state
	// row after Begin (Corrosion lag). Zero uses the default; negative disables
	// the wait (tests).
	ManifestStateWait time.Duration

	mu       sync.Mutex
	cli      *Client
	cliKey   string
	statuses *statusTracker
}

func (m *Module) logger() *slog.Logger {
	if m.Logger != nil {
		return m.Logger
	}
	return slog.Default()
}

func (m *Module) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Module) webBase() string {
	if m.WebBase != "" {
		return m.WebBase
	}
	return DefaultWebBase
}

func (m *Module) dashboard(ctx context.Context) string {
	if m.DashboardURL != nil {
		return m.DashboardURL(ctx)
	}
	return ""
}

// LoadApp returns the App config or store.ErrNotFound.
func (m *Module) LoadApp(ctx context.Context) (*AppConfig, error) {
	return store.GetJSON[AppConfig](ctx, m.Store, store.TableSettings, appSettingID)
}

// SaveApp stores cfg (secrets already sealed).
func (m *Module) SaveApp(ctx context.Context, cfg *AppConfig) error {
	return store.PutJSON(ctx, m.Store, store.TableSettings, appSettingID, nil, cfg)
}

// ClearApp removes stored App credentials, cached API client, and installation
// rows. The GitHub App registration itself is left on GitHub.
func (m *Module) ClearApp(ctx context.Context) error {
	if err := m.Store.Delete(ctx, store.TableSettings, appSettingID); err != nil && !store.IsNotFound(err) {
		return err
	}
	m.mu.Lock()
	m.cli, m.cliKey = nil, ""
	m.mu.Unlock()
	rows, err := m.Store.List(ctx, store.TableGitHubInstallations, store.Query{})
	if err != nil {
		return err
	}
	for _, r := range rows {
		_ = m.Store.Delete(ctx, store.TableGitHubInstallations, r.ID)
	}
	return nil
}

// storeConversion seals and saves the credentials from a manifest conversion.
func (m *Module) storeConversion(ctx context.Context, mc *ManifestConversion, webhookURL string) (*AppConfig, error) {
	seal := func(s string) (string, error) { return m.Key.Seal([]byte(s)) }
	pem, err := seal(mc.PEM)
	if err != nil {
		return nil, err
	}
	ws, err := seal(mc.WebhookSecret)
	if err != nil {
		return nil, err
	}
	cs, err := seal(mc.ClientSecret)
	if err != nil {
		return nil, err
	}
	cfg := &AppConfig{AppID: mc.ID, Slug: mc.Slug, HTMLURL: mc.HTMLURL, ClientID: mc.ClientID, WebhookURL: webhookURL,
		SealedPEM: pem, SealedWebhookSecret: ws, SealedClientSecret: cs}
	return cfg, m.SaveApp(ctx, cfg)
}

// WebhookSecret returns the decrypted webhook secret.
func (m *Module) WebhookSecret(ctx context.Context) ([]byte, error) {
	cfg, err := m.LoadApp(ctx)
	if err != nil {
		return nil, err
	}
	return m.Key.Open(cfg.SealedWebhookSecret)
}

// Client returns an API client for the configured App. The client (and its
// installation-token cache) is reused until the stored key changes.
func (m *Module) Client(ctx context.Context) (*Client, error) {
	cfg, err := m.LoadApp(ctx)
	if err != nil {
		if store.IsNotFound(err) {
			return nil, errNotConfigured
		}
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := fmt.Sprintf("%d/%s", cfg.AppID, cfg.SealedPEM)
	if m.cli != nil && m.cliKey == key {
		return m.cli, nil
	}
	pemBytes, err := m.Key.Open(cfg.SealedPEM)
	if err != nil {
		return nil, fmt.Errorf("decrypt app key: %w", err)
	}
	rk, err := ParsePrivateKey(pemBytes)
	if err != nil {
		return nil, err
	}
	m.cli = &Client{APIBase: m.APIBase, HTTP: m.HTTP, AppID: cfg.AppID, Key: rk, Now: m.Now}
	m.cliKey = key
	return m.cli, nil
}

type notConfiguredError struct{}

func (notConfiguredError) Error() string { return "GitHub App is not configured" }

var errNotConfigured error = notConfiguredError{}

// ---- installations

func putInstallation(ctx context.Context, st store.Store, in *v1.Installation) error {
	return store.PutProto(ctx, st, store.TableGitHubInstallations, strconv.FormatInt(in.Id, 10), nil, in)
}

func listInstallations(ctx context.Context, st store.Store) ([]*v1.Installation, error) {
	return store.ListProto[v1.Installation](ctx, st, store.TableGitHubInstallations, store.Query{})
}

// SyncInstallations refreshes the installations table from the GitHub API.
func (m *Module) SyncInstallations(ctx context.Context) error {
	c, err := m.Client(ctx)
	if err != nil {
		return err
	}
	list, err := c.ListInstallations(ctx)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, in := range list {
		seen[strconv.FormatInt(in.ID, 10)] = true
		if err := putInstallation(ctx, m.Store, &v1.Installation{Id: in.ID, Account: in.Account.Login,
			AccountType: in.Account.Type, CreatedAt: timestamppb.New(in.CreatedAt)}); err != nil {
			return err
		}
	}
	old, _ := m.Store.List(ctx, store.TableGitHubInstallations, store.Query{})
	for _, r := range old {
		if !seen[r.ID] {
			_ = m.Store.Delete(ctx, store.TableGitHubInstallations, r.ID)
		}
	}
	return nil
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
