// Package backup schedules Docker volume snapshots to S3-compatible storage
// and restores them when reschedule emits volume.restore_requested.
package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aetherix/aetherixcloud/internal/events"
	"github.com/aetherix/aetherixcloud/internal/ids"
	"github.com/aetherix/aetherixcloud/internal/runtime"
	"github.com/aetherix/aetherixcloud/internal/store"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

const (
	settingID     = "backup/s3"
	historyPrefix = "backup/run/"
	Path          = "/api/backups"
)

// Config is the cluster-wide volume backup settings (stored encrypted-ish in settings).
type Config struct {
	Enabled         bool   `json:"enabled"`
	Endpoint        string `json:"endpoint"`
	Region          string `json:"region"`
	Bucket          string `json:"bucket"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	Prefix          string `json:"prefix"`
	PathStyle       bool   `json:"path_style"`
	// Cron is a 5-field cron (default "0 3 * * *" = 03:00 daily).
	Cron string `json:"cron"`
	// ServiceIDs limits backups; empty = every service with backup-restore volumes.
	ServiceIDs []string `json:"service_ids,omitempty"`
}

// RunRecord is one completed (or failed) backup attempt.
type RunRecord struct {
	ID        string    `json:"id"`
	ServiceID string    `json:"service_id"`
	Volume    string    `json:"volume"`
	Key       string    `json:"key"`
	Bytes     int64     `json:"bytes"`
	Status    string    `json:"status"` // ok|error
	Error     string    `json:"error,omitempty"`
	At        time.Time `json:"at"`
}

// Manager owns backup scheduling and restore.
type Manager struct {
	Store    store.Store
	Runtime  runtime.Runtime
	Self     string
	Logger   *slog.Logger
	Interval time.Duration // how often we re-check cron (default 1m)

	docker *client.Client
}

// NewManager wires a Manager. Runtime may be nil (no-docker mode).
func NewManager(st store.Store, rt runtime.Runtime, self string, log *slog.Logger) *Manager {
	m := &Manager{Store: st, Runtime: rt, Self: self, Logger: log, Interval: time.Minute}
	if log == nil {
		m.Logger = slog.Default()
	}
	if d, ok := rt.(interface{ Client() *client.Client }); ok {
		m.docker = d.Client()
	}
	return m
}

// GetConfig loads backup settings.
func GetConfig(ctx context.Context, st store.Store) (Config, error) {
	c, err := store.GetJSON[Config](ctx, st, store.TableSettings, settingID)
	if err != nil {
		if store.IsNotFound(err) {
			return Config{Cron: "0 3 * * *", PathStyle: true}, nil
		}
		return Config{}, err
	}
	return *c, nil
}

// PutConfig saves backup settings.
func PutConfig(ctx context.Context, st store.Store, c Config) error {
	if c.Cron == "" {
		c.Cron = "0 3 * * *"
	}
	return store.PutJSON(ctx, st, store.TableSettings, settingID, nil, c)
}

// ListRuns returns recent backup runs, newest first.
func ListRuns(ctx context.Context, st store.Store, limit int) ([]RunRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	recs, err := st.List(ctx, store.TableSettings, store.Query{})
	if err != nil {
		return nil, err
	}
	var out []RunRecord
	for _, r := range recs {
		if !strings.HasPrefix(r.ID, historyPrefix) {
			continue
		}
		var rr RunRecord
		if json.Unmarshal(r.Data, &rr) != nil {
			continue
		}
		out = append(out, rr)
	}
	// newest first (ULID / time in At)
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].At.After(out[i].At) {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Run schedules backups and watches restore events until ctx is done.
func (m *Manager) Run(ctx context.Context) {
	go m.watchRestores(ctx)
	t := time.NewTicker(m.Interval)
	defer t.Stop()
	var lastFire string // "YYYY-MM-DD-HH-MM" of last cron match
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			cfg, err := GetConfig(ctx, m.Store)
			if err != nil || !cfg.Enabled || !cfg.S3().Valid() {
				continue
			}
			slot := now.UTC().Format("2006-01-02-15-04")
			if slot == lastFire {
				continue
			}
			ok, err := MatchCron(cfg.Cron, now.UTC())
			if err != nil || !ok {
				continue
			}
			lastFire = slot
			m.backupDue(ctx, cfg)
		}
	}
}

func (c Config) S3() S3Config {
	return S3Config{
		Endpoint: c.Endpoint, Region: c.Region, Bucket: c.Bucket,
		AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey,
		Prefix: c.Prefix, PathStyle: c.PathStyle,
	}
}

func (m *Manager) backupDue(ctx context.Context, cfg Config) {
	svcs, err := store.ListServices(ctx, m.Store, nil)
	if err != nil {
		return
	}
	allow := map[string]bool{}
	for _, id := range cfg.ServiceIDs {
		allow[id] = true
	}
	for _, svc := range svcs {
		if len(allow) > 0 && !allow[svc.Id] {
			continue
		}
		spec := svc.Spec
		if spec == nil {
			continue
		}
		for _, v := range spec.Volumes {
			if v.Persistence != "backup-restore" {
				continue
			}
			if err := m.BackupVolume(ctx, cfg, svc.Id, v.Source); err != nil {
				m.Logger.Warn("volume backup failed", "service", svc.Id, "volume", v.Source, "err", err)
			}
		}
	}
}

// BackupVolume tars a named Docker volume and uploads it to S3. Runs on the
// node that currently hosts a replica using that volume when possible; if
// docker is unavailable, records an error run.
func (m *Manager) BackupVolume(ctx context.Context, cfg Config, serviceID, volume string) error {
	rec := RunRecord{ID: ids.New(), ServiceID: serviceID, Volume: volume, Status: "ok", At: time.Now().UTC()}
	defer func() {
		_ = store.PutJSON(ctx, m.Store, store.TableSettings, historyPrefix+rec.ID, nil, rec)
		_, _ = events.Emit(ctx, m.Store, "backup."+rec.Status, serviceID,
			fmt.Sprintf("volume %s backup %s", volume, rec.Status),
			map[string]string{"volume": volume, "key": rec.Key, "error": rec.Error})
	}()
	if m.docker == nil {
		rec.Status, rec.Error = "error", "no docker runtime on this node"
		return fmt.Errorf("%s", rec.Error)
	}
	key := cfg.S3().key("volumes", serviceID, volume, time.Now().UTC().Format("20060102T150405Z")+".tar.gz")
	rec.Key = key

	tmp, err := os.CreateTemp("", "ax-vol-*.tar.gz")
	if err != nil {
		rec.Status, rec.Error = "error", err.Error()
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if err := m.tarVolume(ctx, volume, tmp); err != nil {
		tmp.Close()
		rec.Status, rec.Error = "error", err.Error()
		return err
	}
	if err := tmp.Close(); err != nil {
		rec.Status, rec.Error = "error", err.Error()
		return err
	}
	fi, err := os.Stat(tmpPath)
	if err != nil {
		rec.Status, rec.Error = "error", err.Error()
		return err
	}
	rec.Bytes = fi.Size()
	f, err := os.Open(tmpPath)
	if err != nil {
		rec.Status, rec.Error = "error", err.Error()
		return err
	}
	defer f.Close()
	cli := NewClient(cfg.S3())
	if err := cli.PutObject(ctx, key, f, fi.Size(), "application/gzip"); err != nil {
		rec.Status, rec.Error = "error", err.Error()
		return err
	}
	return nil
}

func (m *Manager) tarVolume(ctx context.Context, volume string, w io.Writer) error {
	name := "ax-backup-" + ids.New()[:8]
	resp, err := m.docker.ContainerCreate(ctx, &container.Config{
		Image:      "alpine:3.20",
		Cmd:        []string{"tar", "czf", "-", "-C", "/data", "."},
		Entrypoint: []string{},
		Labels:     map[string]string{runtime.LabelManaged: "true", "aetherix.backup": "1"},
	}, &container.HostConfig{
		AutoRemove: true,
		Mounts:     []mount.Mount{{Type: mount.TypeVolume, Source: volume, Target: "/data", ReadOnly: true}},
	}, nil, nil, name)
	if err != nil {
		if pullErr := m.ensureAlpine(ctx); pullErr == nil {
			resp, err = m.docker.ContainerCreate(ctx, &container.Config{
				Image: "alpine:3.20", Cmd: []string{"tar", "czf", "-", "-C", "/data", "."}, Entrypoint: []string{},
				Labels: map[string]string{runtime.LabelManaged: "true"},
			}, &container.HostConfig{
				AutoRemove: true,
				Mounts:     []mount.Mount{{Type: mount.TypeVolume, Source: volume, Target: "/data", ReadOnly: true}},
			}, nil, nil, name)
		}
		if err != nil {
			return err
		}
	}
	hijack, err := m.docker.ContainerAttach(ctx, resp.ID, container.AttachOptions{Stream: true, Stdout: true, Stderr: true})
	if err != nil {
		_ = m.docker.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return err
	}
	defer hijack.Close()
	if err := m.docker.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return err
	}
	_, err = stdcopy.StdCopy(w, io.Discard, hijack.Reader)
	return err
}

func (m *Manager) ensureAlpine(ctx context.Context) error {
	rc, err := m.docker.ImagePull(ctx, "alpine:3.20", image.PullOptions{})
	if err != nil {
		return err
	}
	defer rc.Close()
	_, _ = io.Copy(io.Discard, rc)
	return nil
}

func (m *Manager) watchRestores(ctx context.Context) {
	ch, err := events.Watch(ctx, m.Store, "volume.restore_requested", 0)
	if err != nil {
		m.Logger.Warn("backup restore watch failed", "err", err)
		return
	}
	for e := range ch {
		vol := e.Attributes["volume"]
		svc := e.SubjectId
		if vol == "" {
			continue
		}
		cfg, err := GetConfig(ctx, m.Store)
		if err != nil || !cfg.S3().Valid() {
			continue
		}
		if err := m.RestoreLatest(ctx, cfg, svc, vol); err != nil {
			m.Logger.Warn("volume restore failed", "service", svc, "volume", vol, "err", err)
			_, _ = events.Emit(ctx, m.Store, "backup.restore_error", svc, err.Error(), map[string]string{"volume": vol})
		} else {
			_, _ = events.Emit(ctx, m.Store, "backup.restored", svc, fmt.Sprintf("restored volume %s", vol), map[string]string{"volume": vol})
		}
	}
}

// RestoreLatest finds the newest object for service/volume and extracts it.
func (m *Manager) RestoreLatest(ctx context.Context, cfg Config, serviceID, volume string) error {
	if m.docker == nil {
		return fmt.Errorf("no docker runtime")
	}
	cli := NewClient(cfg.S3())
	prefix := cfg.S3().key("volumes", serviceID, volume) + "/"
	keys, err := cli.ListObjects(ctx, prefix)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return fmt.Errorf("no backups for %s/%s", serviceID, volume)
	}
	latest := keys[0]
	for _, k := range keys[1:] {
		if k > latest {
			latest = k
		}
	}
	return m.RestoreKey(ctx, cfg, serviceID, volume, latest)
}

// RestoreKey downloads a specific S3 object and extracts it into the named volume.
func (m *Manager) RestoreKey(ctx context.Context, cfg Config, serviceID, volume, key string) error {
	if m.docker == nil {
		return fmt.Errorf("no docker runtime")
	}
	if key == "" {
		return fmt.Errorf("backup key is required")
	}
	if err := m.EnsureVolumeExists(ctx, volume); err != nil {
		return err
	}
	cli := NewClient(cfg.S3())
	rc, err := cli.GetObject(ctx, key)
	if err != nil {
		return err
	}
	defer rc.Close()
	tmp, err := os.CreateTemp("", "ax-restore-*.tar.gz")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, rc); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return m.untarVolume(ctx, volume, tmp.Name())
}

func (m *Manager) untarVolume(ctx context.Context, volume, archivePath string) error {
	name := "ax-restore-" + ids.New()[:8]
	abs, _ := filepath.Abs(archivePath)
	resp, err := m.docker.ContainerCreate(ctx, &container.Config{
		Image:  "alpine:3.20",
		Cmd:    []string{"sh", "-c", "tar xzf /archive.tar.gz -C /data"},
		Labels: map[string]string{runtime.LabelManaged: "true"},
	}, &container.HostConfig{
		AutoRemove: true,
		Mounts: []mount.Mount{
			{Type: mount.TypeVolume, Source: volume, Target: "/data"},
			{Type: mount.TypeBind, Source: abs, Target: "/archive.tar.gz", ReadOnly: true},
		},
	}, nil, nil, name)
	if err != nil {
		return err
	}
	if err := m.docker.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return err
	}
	statusCh, errCh := m.docker.ContainerWait(ctx, resp.ID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		return err
	case st := <-statusCh:
		if st.StatusCode != 0 {
			return fmt.Errorf("restore container exited %d", st.StatusCode)
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// EnsureVolumeExists creates a named volume if missing (used before restore).
func (m *Manager) EnsureVolumeExists(ctx context.Context, name string) error {
	if m.docker == nil {
		return fmt.Errorf("no docker")
	}
	vols, err := m.docker.VolumeList(ctx, volume.ListOptions{})
	if err != nil {
		return err
	}
	for _, v := range vols.Volumes {
		if v.Name == name {
			return nil
		}
	}
	_, err = m.docker.VolumeCreate(ctx, volume.CreateOptions{Name: name, Labels: map[string]string{runtime.LabelManaged: "true"}})
	return err
}

// --- HTTP API ---

// API serves backup config and history.
type API struct {
	Store        store.Store
	Manager      *Manager
	Authenticate func(*http.Request) error
	RequireAdmin func(*http.Request) error // optional stricter check
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(Path, a.handleRoot)
	mux.HandleFunc(Path+"/run", a.handleRun)
	mux.HandleFunc(Path+"/restore", a.handleRestore)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.Authenticate != nil {
			if err := a.Authenticate(r); err != nil {
				http.Error(w, err.Error(), http.StatusUnauthorized)
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

func (a *API) handleRoot(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg, err := GetConfig(r.Context(), a.Store)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		redacted := cfg
		if redacted.SecretAccessKey != "" {
			redacted.SecretAccessKey = "••••••••"
		}
		runs, _ := ListRuns(r.Context(), a.Store, 30)
		writeJSON(w, map[string]any{"config": redacted, "runs": runs})
	case http.MethodPut:
		if a.RequireAdmin != nil {
			if err := a.RequireAdmin(r); err != nil {
				http.Error(w, err.Error(), http.StatusForbidden)
				return
			}
		}
		var cfg Config
		if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		// Preserve secret if the client sent the redacted placeholder.
		if cfg.SecretAccessKey == "••••••••" || cfg.SecretAccessKey == "" {
			if prev, err := GetConfig(r.Context(), a.Store); err == nil {
				cfg.SecretAccessKey = prev.SecretAccessKey
			}
		}
		if err := PutConfig(r.Context(), a.Store, cfg); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	default:
		http.Error(w, "method not allowed", 405)
	}
}

func (a *API) handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	if a.RequireAdmin != nil {
		if err := a.RequireAdmin(r); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
	}
	var body struct {
		ServiceID string `json:"service_id"`
		Volume    string `json:"volume"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	cfg, err := GetConfig(r.Context(), a.Store)
	if err != nil || !cfg.S3().Valid() {
		http.Error(w, "backup not configured", 400)
		return
	}
	if a.Manager == nil {
		http.Error(w, "backup manager unavailable", 503)
		return
	}
	if body.ServiceID == "" || body.Volume == "" {
		go a.Manager.backupDue(context.Background(), cfg)
		writeJSON(w, map[string]any{"started": true})
		return
	}
	if err := a.Manager.BackupVolume(r.Context(), cfg, body.ServiceID, body.Volume); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (a *API) handleRestore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	if a.RequireAdmin != nil {
		if err := a.RequireAdmin(r); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
	}
	var body struct {
		ServiceID string `json:"service_id"`
		Volume    string `json:"volume"`
		Key       string `json:"key"`
		RunID     string `json:"run_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	cfg, err := GetConfig(r.Context(), a.Store)
	if err != nil || !cfg.S3().Valid() {
		http.Error(w, "backup not configured", 400)
		return
	}
	if a.Manager == nil {
		http.Error(w, "backup manager unavailable", 503)
		return
	}
	// Resolve service/volume from a past run when only run_id is given.
	if body.RunID != "" && (body.ServiceID == "" || body.Volume == "") {
		runs, _ := ListRuns(r.Context(), a.Store, 100)
		for _, rr := range runs {
			if rr.ID == body.RunID {
				body.ServiceID, body.Volume = rr.ServiceID, rr.Volume
				if body.Key == "" {
					body.Key = rr.Key
				}
				break
			}
		}
	}
	if body.ServiceID == "" || body.Volume == "" {
		http.Error(w, "service_id and volume are required (or run_id)", 400)
		return
	}
	// Local docker: restore now. Otherwise emit the same event reschedule uses
	// so a node that hosts the volume can pick it up.
	if a.Manager.docker != nil {
		var restoreErr error
		if body.Key != "" {
			restoreErr = a.Manager.RestoreKey(r.Context(), cfg, body.ServiceID, body.Volume, body.Key)
		} else {
			restoreErr = a.Manager.RestoreLatest(r.Context(), cfg, body.ServiceID, body.Volume)
		}
		if restoreErr != nil {
			http.Error(w, restoreErr.Error(), 500)
			return
		}
		_, _ = events.Emit(r.Context(), a.Store, "backup.restored", body.ServiceID,
			fmt.Sprintf("restored volume %s", body.Volume),
			map[string]string{"volume": body.Volume, "key": body.Key})
		writeJSON(w, map[string]any{"ok": true, "restored": true})
		return
	}
	_, _ = events.Emit(r.Context(), a.Store, "volume.restore_requested", body.ServiceID,
		fmt.Sprintf("manual restore of volume %q", body.Volume),
		map[string]string{"volume": body.Volume, "key": body.Key})
	writeJSON(w, map[string]any{"ok": true, "queued": true})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
