package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aetherix/aetherixcloud/internal/ids"
	"github.com/aetherix/aetherixcloud/internal/store"
)

const (
	targetPrefix       = "backup/s3-target/"
	clusterSettingID   = "backup/cluster"
	legacySettingID    = "backup/s3"
	TargetsPath        = "/api/s3-targets"
	DefaultVolumeCron  = "0 3 * * *"
	DefaultClusterCron = "0 4 * * *"
	DefaultRetain      = 7
	DefaultClusterRetain = 14
	secretPlaceholder  = "••••••••"
)

// Target is a named S3-compatible destination shared by volume and cluster backups.
type Target struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Endpoint        string    `json:"endpoint"`
	Region          string    `json:"region"`
	Bucket          string    `json:"bucket"`
	AccessKeyID     string    `json:"access_key_id"`
	SecretAccessKey string    `json:"secret_access_key"`
	Prefix          string    `json:"prefix"`
	PathStyle       bool      `json:"path_style"`
	CreatedAt       time.Time `json:"created_at"`
}

// S3 returns the wire credentials for this target.
func (t Target) S3() S3Config {
	return S3Config{
		Endpoint: t.Endpoint, Region: t.Region, Bucket: t.Bucket,
		AccessKeyID: t.AccessKeyID, SecretAccessKey: t.SecretAccessKey,
		Prefix: t.Prefix, PathStyle: t.PathStyle,
	}
}

// Redacted returns a copy safe to send to clients.
func (t Target) Redacted() Target {
	if t.SecretAccessKey != "" {
		t.SecretAccessKey = secretPlaceholder
	}
	return t
}

// ClusterSchedule controls scheduled cluster .snapshot uploads to an S3 target.
type ClusterSchedule struct {
	Enabled  bool   `json:"enabled"`
	TargetID string `json:"target_id"`
	Cron     string `json:"cron"`
	Retain   int    `json:"retain"`
}

// ListTargets returns all S3 targets, migrating legacy backup/s3 config if needed.
func ListTargets(ctx context.Context, st store.Store) ([]Target, error) {
	if err := migrateLegacy(ctx, st); err != nil {
		return nil, err
	}
	recs, err := st.List(ctx, store.TableSettings, store.Query{})
	if err != nil {
		return nil, err
	}
	var out []Target
	for _, r := range recs {
		if !strings.HasPrefix(r.ID, targetPrefix) {
			continue
		}
		var t Target
		if json.Unmarshal(r.Data, &t) != nil {
			continue
		}
		if t.ID == "" {
			t.ID = strings.TrimPrefix(r.ID, targetPrefix)
		}
		out = append(out, t)
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].Name < out[i].Name {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out, nil
}

// GetTarget loads one target by id.
func GetTarget(ctx context.Context, st store.Store, id string) (Target, error) {
	if id == "" {
		return Target{}, fmt.Errorf("target id required")
	}
	if err := migrateLegacy(ctx, st); err != nil {
		return Target{}, err
	}
	t, err := store.GetJSON[Target](ctx, st, store.TableSettings, targetPrefix+id)
	if err != nil {
		return Target{}, err
	}
	if t.ID == "" {
		t.ID = id
	}
	return *t, nil
}

// PutTarget upserts a target. Empty secret (or placeholder) keeps the previous secret.
func PutTarget(ctx context.Context, st store.Store, t Target) (Target, error) {
	if t.Name == "" {
		return Target{}, fmt.Errorf("name is required")
	}
	if t.Endpoint == "" || t.Bucket == "" || t.AccessKeyID == "" {
		return Target{}, fmt.Errorf("endpoint, bucket and access_key_id are required")
	}
	if t.ID == "" {
		t.ID = ids.New()
		t.CreatedAt = time.Now().UTC()
	} else if t.CreatedAt.IsZero() {
		if prev, err := GetTarget(ctx, st, t.ID); err == nil {
			t.CreatedAt = prev.CreatedAt
		} else {
			t.CreatedAt = time.Now().UTC()
		}
	}
	if t.Region == "" {
		t.Region = "us-east-1"
	}
	if t.Prefix == "" {
		t.Prefix = "aetherix/"
	}
	if t.SecretAccessKey == "" || t.SecretAccessKey == secretPlaceholder {
		if prev, err := GetTarget(ctx, st, t.ID); err == nil {
			t.SecretAccessKey = prev.SecretAccessKey
		}
	}
	if t.SecretAccessKey == "" || t.SecretAccessKey == secretPlaceholder {
		return Target{}, fmt.Errorf("secret_access_key is required")
	}
	if err := store.PutJSON(ctx, st, store.TableSettings, targetPrefix+t.ID, nil, t); err != nil {
		return Target{}, err
	}
	return t, nil
}

// DeleteTarget removes a target by id.
func DeleteTarget(ctx context.Context, st store.Store, id string) error {
	if id == "" {
		return fmt.Errorf("target id required")
	}
	return st.Delete(ctx, store.TableSettings, targetPrefix+id)
}

// ResolveTarget returns the named target, or the default/first when id is empty.
func ResolveTarget(ctx context.Context, st store.Store, id string) (Target, error) {
	if id != "" {
		return GetTarget(ctx, st, id)
	}
	list, err := ListTargets(ctx, st)
	if err != nil {
		return Target{}, err
	}
	if len(list) == 0 {
		return Target{}, fmt.Errorf("no S3 targets configured")
	}
	for _, t := range list {
		if strings.EqualFold(t.Name, "default") {
			return t, nil
		}
	}
	return list[0], nil
}

// GetClusterSchedule loads scheduled cluster snapshot settings.
func GetClusterSchedule(ctx context.Context, st store.Store) (ClusterSchedule, error) {
	c, err := store.GetJSON[ClusterSchedule](ctx, st, store.TableSettings, clusterSettingID)
	if err != nil {
		if store.IsNotFound(err) {
			return ClusterSchedule{Cron: DefaultClusterCron, Retain: DefaultClusterRetain}, nil
		}
		return ClusterSchedule{}, err
	}
	if c.Cron == "" {
		c.Cron = DefaultClusterCron
	}
	if c.Retain <= 0 {
		c.Retain = DefaultClusterRetain
	}
	return *c, nil
}

// PutClusterSchedule saves scheduled cluster snapshot settings.
func PutClusterSchedule(ctx context.Context, st store.Store, c ClusterSchedule) error {
	if c.Cron == "" {
		c.Cron = DefaultClusterCron
	}
	if c.Retain <= 0 {
		c.Retain = DefaultClusterRetain
	}
	return store.PutJSON(ctx, st, store.TableSettings, clusterSettingID, nil, c)
}

func migrateLegacy(ctx context.Context, st store.Store) error {
	recs, err := st.List(ctx, store.TableSettings, store.Query{})
	if err != nil {
		return err
	}
	for _, r := range recs {
		if strings.HasPrefix(r.ID, targetPrefix) {
			return nil // already migrated / has targets
		}
	}
	legacy, err := store.GetJSON[Config](ctx, st, store.TableSettings, legacySettingID)
	if err != nil {
		if store.IsNotFound(err) {
			return nil
		}
		return err
	}
	if !legacy.S3().Valid() {
		return nil
	}
	t := Target{
		ID:              "default",
		Name:            "Default",
		Endpoint:        legacy.Endpoint,
		Region:          legacy.Region,
		Bucket:          legacy.Bucket,
		AccessKeyID:     legacy.AccessKeyID,
		SecretAccessKey: legacy.SecretAccessKey,
		Prefix:          legacy.Prefix,
		PathStyle:       legacy.PathStyle,
		CreatedAt:       time.Now().UTC(),
	}
	if t.Region == "" {
		t.Region = "us-east-1"
	}
	if t.Prefix == "" {
		t.Prefix = "aetherix/"
	}
	return store.PutJSON(ctx, st, store.TableSettings, targetPrefix+t.ID, nil, t)
}

// TargetsAPI serves /api/s3-targets.
type TargetsAPI struct {
	Store        store.Store
	Authenticate func(*http.Request) error
	RequireAdmin func(*http.Request) error
}

func (a *TargetsAPI) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.Authenticate != nil {
			if err := a.Authenticate(r); err != nil {
				http.Error(w, err.Error(), http.StatusUnauthorized)
				return
			}
		}
		path := strings.TrimPrefix(r.URL.Path, TargetsPath)
		path = strings.Trim(path, "/")
		switch {
		case path == "" && r.Method == http.MethodGet:
			list, err := ListTargets(r.Context(), a.Store)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			out := make([]Target, len(list))
			for i, t := range list {
				out[i] = t.Redacted()
			}
			writeJSON(w, map[string]any{"targets": out})
		case path == "" && r.Method == http.MethodPost:
			if err := a.requireAdmin(r); err != nil {
				http.Error(w, err.Error(), http.StatusForbidden)
				return
			}
			var t Target
			if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			t.ID = "" // always allocate
			saved, err := PutTarget(r.Context(), a.Store, t)
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			writeJSON(w, saved.Redacted())
		case path != "" && r.Method == http.MethodGet:
			t, err := GetTarget(r.Context(), a.Store, path)
			if err != nil {
				if store.IsNotFound(err) {
					http.Error(w, "not found", 404)
					return
				}
				http.Error(w, err.Error(), 500)
				return
			}
			writeJSON(w, t.Redacted())
		case path != "" && r.Method == http.MethodPut:
			if err := a.requireAdmin(r); err != nil {
				http.Error(w, err.Error(), http.StatusForbidden)
				return
			}
			var t Target
			if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			t.ID = path
			saved, err := PutTarget(r.Context(), a.Store, t)
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			writeJSON(w, saved.Redacted())
		case path != "" && r.Method == http.MethodDelete:
			if err := a.requireAdmin(r); err != nil {
				http.Error(w, err.Error(), http.StatusForbidden)
				return
			}
			if err := DeleteTarget(r.Context(), a.Store, path); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			writeJSON(w, map[string]any{"ok": true})
		default:
			http.Error(w, "method not allowed", 405)
		}
	})
}

func (a *TargetsAPI) requireAdmin(r *http.Request) error {
	if a.RequireAdmin != nil {
		return a.RequireAdmin(r)
	}
	return nil
}
