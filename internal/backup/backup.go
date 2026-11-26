// Package backup schedules Docker volume snapshots and cluster settings
// snapshots to S3-compatible storage, and restores volumes when reschedule
// emits volume.restore_requested.
package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aetherix/aetherixcloud/internal/events"
	"github.com/aetherix/aetherixcloud/internal/ids"
	"github.com/aetherix/aetherixcloud/internal/lease"
	"github.com/aetherix/aetherixcloud/internal/runtime"
	"github.com/aetherix/aetherixcloud/internal/snapshot"
	"github.com/aetherix/aetherixcloud/internal/store"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

const (
	historyPrefix = "backup/run/"
	Path          = "/api/backups"
)

// Config is the legacy cluster-wide volume backup settings (backup/s3).
// Kept for one-shot migration into Target records — prefer Target + per-volume fields.
type Config struct {
	Enabled         bool     `json:"enabled"`
	Endpoint        string   `json:"endpoint"`
	Region          string   `json:"region"`
	Bucket          string   `json:"bucket"`
	AccessKeyID     string   `json:"access_key_id"`
	SecretAccessKey string   `json:"secret_access_key"`
	Prefix          string   `json:"prefix"`
	PathStyle       bool     `json:"path_style"`
	Cron            string   `json:"cron"`
	ServiceIDs      []string `json:"service_ids,omitempty"`
}

func (c Config) S3() S3Config {
	return S3Config{
		Endpoint: c.Endpoint, Region: c.Region, Bucket: c.Bucket,
		AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey,
		Prefix: c.Prefix, PathStyle: c.PathStyle,
	}
}

// RunRecord is one completed (or failed) backup attempt.
type RunRecord struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"` // volume|cluster
	ServiceID string    `json:"service_id,omitempty"`
	Volume    string    `json:"volume,omitempty"`
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
	// Leases coordinates scheduled backups across nodes (nil = no lease).
	Leases *lease.Manager
	// Identity supplies cluster secrets for scheduled .snapshot uploads.
	Identity func() *snapshot.Identity

	docker *client.Client

	// test hooks (nil in production)
	testBackupVolume  func(ctx context.Context, serviceID, volume string) error
	testBackupCluster func(ctx context.Context) error
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
		if rr.Kind == "" {
			rr.Kind = "volume"
		}
		out = append(out, rr)
	}
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
	lastFire := map[string]string{}
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			m.tick(ctx, now.UTC(), lastFire)
		}
	}
}

func (m *Manager) tick(ctx context.Context, now time.Time, lastFire map[string]string) {
	m.backupVolumesDue(ctx, now, lastFire)
	m.backupClusterDue(ctx, now, lastFire)
}

func (m *Manager) backupVolumesDue(ctx context.Context, now time.Time, lastFire map[string]string) {
	svcs, err := store.ListServices(ctx, m.Store, nil)
	if err != nil {
		return
	}
	slot := now.Format("2006-01-02-15-04")
	for _, svc := range svcs {
		spec := svc.Spec
		if spec == nil {
			continue
		}
		for _, v := range spec.Volumes {
			if v.Persistence != "backup-restore" {
				continue
			}
			cronExpr := v.BackupCron
			if cronExpr == "" {
				cronExpr = DefaultVolumeCron
			}
			fireKey := "vol:" + svc.Id + ":" + v.Source
			if lastFire[fireKey] == slot {
				continue
			}
			ok, err := MatchCron(cronExpr, now)
			if err != nil || !ok {
				continue
			}
			lastFire[fireKey] = slot
			if !m.shouldRunVolumeOnSelf(ctx, svc.Id) {
				continue
			}
			_, err = m.runExclusive(ctx, volumeLeaseName(svc.Id, v.Source), fireKey, slot, func(ctx context.Context) error {
				return m.runVolumeBackup(ctx, svc.Id, v.Source)
			})
			if err != nil {
				m.Logger.Warn("volume backup failed", "service", svc.Id, "volume", v.Source, "err", err)
			}
		}
	}
}

func (m *Manager) backupClusterDue(ctx context.Context, now time.Time, lastFire map[string]string) {
	cs, err := GetClusterSchedule(ctx, m.Store)
	if err != nil || !cs.Enabled {
		return
	}
	slot := now.Format("2006-01-02-15-04")
	if lastFire["cluster"] == slot {
		return
	}
	ok, err := MatchCron(cs.Cron, now)
	if err != nil || !ok {
		return
	}
	lastFire["cluster"] = slot
	_, err = m.runExclusive(ctx, leaseCluster, "cluster", slot, func(ctx context.Context) error {
		return m.runClusterBackup(ctx)
	})
	if err != nil {
		m.Logger.Warn("cluster snapshot backup failed", "err", err)
	}
}

// BackupVolumeMount resolves the volume's S3 target / retain from the service
// spec and uploads a snapshot.
func (m *Manager) BackupVolumeMount(ctx context.Context, serviceID, volumeName string) error {
	s3, retain, err := m.resolveVolumeBackup(ctx, serviceID, volumeName)
	if err != nil {
		return err
	}
	return m.BackupVolume(ctx, s3, serviceID, volumeName, retain)
}

func (m *Manager) resolveVolumeBackup(ctx context.Context, serviceID, volumeName string) (S3Config, int, error) {
	targetID := ""
	retain := DefaultRetain
	if svc, err := store.GetService(ctx, m.Store, serviceID); err == nil && svc.Spec != nil {
		for _, v := range svc.Spec.Volumes {
			if v.Source != volumeName {
				continue
			}
			targetID = v.S3TargetId
			if v.BackupRetain > 0 {
				retain = int(v.BackupRetain)
			}
			break
		}
	}
	tgt, err := ResolveTarget(ctx, m.Store, targetID)
	if err != nil {
		return S3Config{}, 0, err
	}
	if !tgt.S3().Valid() {
		return S3Config{}, 0, fmt.Errorf("S3 target %q is incomplete", tgt.Name)
	}
	return tgt.S3(), retain, nil
}

// BackupAllVolumesNow snapshots every backup-restore volume once (manual run).
func (m *Manager) BackupAllVolumesNow(ctx context.Context) {
	svcs, err := store.ListServices(ctx, m.Store, nil)
	if err != nil {
		return
	}
	for _, svc := range svcs {
		if svc.Spec == nil {
			continue
		}
		for _, v := range svc.Spec.Volumes {
			if v.Persistence != "backup-restore" {
				continue
			}
			if err := m.BackupVolumeMount(ctx, svc.Id, v.Source); err != nil {
				m.Logger.Warn("volume backup failed", "service", svc.Id, "volume", v.Source, "err", err)
			}
		}
	}
}

// BackupVolume tars a named Docker volume and uploads it to S3.
func (m *Manager) BackupVolume(ctx context.Context, s3 S3Config, serviceID, volumeName string, retain int) error {
	rec := RunRecord{ID: ids.New(), Kind: "volume", ServiceID: serviceID, Volume: volumeName, Status: "ok", At: time.Now().UTC()}
	defer func() {
		_ = store.PutJSON(ctx, m.Store, store.TableSettings, historyPrefix+rec.ID, nil, rec)
		_, _ = events.Emit(ctx, m.Store, "backup."+rec.Status, serviceID,
			fmt.Sprintf("volume %s backup %s", volumeName, rec.Status),
			map[string]string{"volume": volumeName, "key": rec.Key, "error": rec.Error})
	}()
	if m.docker == nil {
		rec.Status, rec.Error = "error", "no docker runtime on this node"
		return fmt.Errorf("%s", rec.Error)
	}
	if !s3.Valid() {
		rec.Status, rec.Error = "error", "S3 target not configured"
		return fmt.Errorf("%s", rec.Error)
	}
	key := s3.key("volumes", serviceID, volumeName, time.Now().UTC().Format("20060102T150405Z")+".tar.gz")
	rec.Key = key

	tmp, err := os.CreateTemp("", "ax-vol-*.tar.gz")
	if err != nil {
		rec.Status, rec.Error = "error", err.Error()
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if err := m.tarVolume(ctx, volumeName, tmp); err != nil {
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
	cli := NewClient(s3)
	if err := cli.PutObject(ctx, key, f, fi.Size(), "application/gzip"); err != nil {
		rec.Status, rec.Error = "error", err.Error()
		return err
	}
	prefix := s3.key("volumes", serviceID, volumeName) + "/"
	m.prune(ctx, s3, prefix, retain)
	return nil
}

// BackupCluster encodes a full store snapshot and uploads it to the configured S3 target.
func (m *Manager) BackupCluster(ctx context.Context) error {
	cs, err := GetClusterSchedule(ctx, m.Store)
	if err != nil {
		return err
	}
	tgt, err := ResolveTarget(ctx, m.Store, cs.TargetID)
	if err != nil {
		return err
	}
	s3 := tgt.S3()
	if !s3.Valid() {
		return fmt.Errorf("S3 target incomplete")
	}
	var ident *snapshot.Identity
	if m.Identity != nil {
		ident = m.Identity()
	}
	data, err := snapshot.Encode(ctx, m.Store, ident)
	if err != nil {
		return err
	}
	rec := RunRecord{ID: ids.New(), Kind: "cluster", Status: "ok", At: time.Now().UTC(), Bytes: int64(len(data))}
	defer func() {
		_ = store.PutJSON(ctx, m.Store, store.TableSettings, historyPrefix+rec.ID, nil, rec)
		_, _ = events.Emit(ctx, m.Store, "backup."+rec.Status, "",
			fmt.Sprintf("cluster snapshot backup %s", rec.Status),
			map[string]string{"key": rec.Key, "error": rec.Error, "kind": "cluster"})
	}()
	key := s3.key("cluster", time.Now().UTC().Format("20060102T150405Z")+".snapshot")
	rec.Key = key
	cli := NewClient(s3)
	if err := cli.PutObject(ctx, key, bytes.NewReader(data), int64(len(data)), "application/json"); err != nil {
		rec.Status, rec.Error = "error", err.Error()
		return err
	}
	m.prune(ctx, s3, s3.key("cluster")+"/", cs.Retain)
	return nil
}

func (m *Manager) prune(ctx context.Context, s3 S3Config, prefix string, retain int) {
	if retain <= 0 {
		retain = DefaultRetain
	}
	cli := NewClient(s3)
	keys, err := cli.ListObjects(ctx, prefix)
	if err != nil {
		m.Logger.Warn("backup prune list failed", "prefix", prefix, "err", err)
		return
	}
	sort.Strings(keys)
	if len(keys) <= retain {
		return
	}
	for _, k := range keys[:len(keys)-retain] {
		if err := cli.DeleteObject(ctx, k); err != nil {
			m.Logger.Warn("backup prune delete failed", "key", k, "err", err)
		}
	}
}

func (m *Manager) tarVolume(ctx context.Context, volumeName string, w io.Writer) error {
	name := "ax-backup-" + ids.New()[:8]
	resp, err := m.docker.ContainerCreate(ctx, &container.Config{
		Image:      "alpine:3.20",
		Cmd:        []string{"tar", "czf", "-", "-C", "/data", "."},
		Entrypoint: []string{},
		Labels:     map[string]string{runtime.LabelManaged: "true", "aetherix.backup": "1"},
	}, &container.HostConfig{
		AutoRemove: true,
		Mounts:     []mount.Mount{{Type: mount.TypeVolume, Source: volumeName, Target: "/data", ReadOnly: true}},
	}, nil, nil, name)
	if err != nil {
		if pullErr := m.ensureAlpine(ctx); pullErr == nil {
			resp, err = m.docker.ContainerCreate(ctx, &container.Config{
				Image: "alpine:3.20", Cmd: []string{"tar", "czf", "-", "-C", "/data", "."}, Entrypoint: []string{},
				Labels: map[string]string{runtime.LabelManaged: "true"},
			}, &container.HostConfig{
				AutoRemove: true,
				Mounts:     []mount.Mount{{Type: mount.TypeVolume, Source: volumeName, Target: "/data", ReadOnly: true}},
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
		s3, _, err := m.resolveVolumeBackup(ctx, svc, vol)
		if err != nil || !s3.Valid() {
			m.Logger.Warn("volume restore: no S3 target", "service", svc, "volume", vol, "err", err)
			continue
		}
		key := e.Attributes["key"]
		var restoreErr error
		if key != "" {
			restoreErr = m.RestoreKey(ctx, s3, vol, key)
		} else {
			restoreErr = m.RestoreLatest(ctx, s3, svc, vol)
		}
		if restoreErr != nil {
			m.Logger.Warn("volume restore failed", "service", svc, "volume", vol, "err", restoreErr)
			_, _ = events.Emit(ctx, m.Store, "backup.restore_error", svc, restoreErr.Error(), map[string]string{"volume": vol})
		} else {
			_, _ = events.Emit(ctx, m.Store, "backup.restored", svc, fmt.Sprintf("restored volume %s", vol), map[string]string{"volume": vol})
		}
		// Manual restores pause the service and pass prev_replicas; reschedule
		// events leave it empty (replica already gone on the failed node).
		if prev := e.Attributes["prev_replicas"]; prev != "" && prev != "0" {
			var n int32
			if _, scanErr := fmt.Sscanf(prev, "%d", &n); scanErr == nil && n > 0 {
				if err := resumeService(ctx, m.Store, svc, n); err != nil {
					m.Logger.Warn("volume restore: resume service failed", "service", svc, "err", err)
				}
			}
		}
	}
}

// RestoreLatest finds the newest object for service/volume and extracts it.
func (m *Manager) RestoreLatest(ctx context.Context, s3 S3Config, serviceID, volumeName string) error {
	if m.docker == nil {
		return fmt.Errorf("no docker runtime")
	}
	cli := NewClient(s3)
	prefix := s3.key("volumes", serviceID, volumeName) + "/"
	keys, err := cli.ListObjects(ctx, prefix)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return fmt.Errorf("no backups for %s/%s", serviceID, volumeName)
	}
	latest := keys[0]
	for _, k := range keys[1:] {
		if k > latest {
			latest = k
		}
	}
	return m.RestoreKey(ctx, s3, volumeName, latest)
}

// RestoreKey downloads a specific S3 object and extracts it into the named volume.
func (m *Manager) RestoreKey(ctx context.Context, s3 S3Config, volumeName, key string) error {
	if m.docker == nil {
		return fmt.Errorf("no docker runtime")
	}
	if key == "" {
		return fmt.Errorf("backup key is required")
	}
	if err := m.EnsureVolumeExists(ctx, volumeName); err != nil {
		return err
	}
	cli := NewClient(s3)
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
	return m.untarVolume(ctx, volumeName, tmp.Name())
}

func (m *Manager) untarVolume(ctx context.Context, volumeName, archivePath string) error {
	name := "ax-restore-" + ids.New()[:8]
	abs, _ := filepath.Abs(archivePath)
	resp, err := m.docker.ContainerCreate(ctx, &container.Config{
		Image:  "alpine:3.20",
		Cmd:    []string{"sh", "-c", "tar xzf /archive.tar.gz -C /data"},
		Labels: map[string]string{runtime.LabelManaged: "true"},
	}, &container.HostConfig{
		AutoRemove: true,
		Mounts: []mount.Mount{
			{Type: mount.TypeVolume, Source: volumeName, Target: "/data"},
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

// API serves backup cluster schedule and history.
type API struct {
	Store        store.Store
	Manager      *Manager
	Authenticate func(*http.Request) error
	RequireAdmin func(*http.Request) error
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(Path, a.handleRoot)
	mux.HandleFunc(Path+"/run", a.handleRun)
	mux.HandleFunc(Path+"/cluster/run", a.handleClusterRun)
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
		cs, err := GetClusterSchedule(r.Context(), a.Store)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		runs, _ := ListRuns(r.Context(), a.Store, 30)
		targets, _ := ListTargets(r.Context(), a.Store)
		redacted := make([]Target, len(targets))
		for i, t := range targets {
			redacted[i] = t.Redacted()
		}
		writeJSON(w, map[string]any{"cluster": cs, "runs": runs, "targets": redacted})
	case http.MethodPut:
		if a.RequireAdmin != nil {
			if err := a.RequireAdmin(r); err != nil {
				http.Error(w, err.Error(), http.StatusForbidden)
				return
			}
		}
		var body struct {
			Cluster *ClusterSchedule `json:"cluster"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if body.Cluster == nil {
			http.Error(w, "cluster required", 400)
			return
		}
		if _, err := MatchCron(body.Cluster.Cron, time.Now().UTC()); body.Cluster.Cron != "" && err != nil {
			http.Error(w, "invalid cron: "+err.Error(), 400)
			return
		}
		if err := PutClusterSchedule(r.Context(), a.Store, *body.Cluster); err != nil {
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
	if a.Manager == nil {
		http.Error(w, "backup manager unavailable", 503)
		return
	}
	if body.ServiceID == "" || body.Volume == "" {
		go a.Manager.BackupAllVolumesNowExclusive(context.Background())
		writeJSON(w, map[string]any{"started": true})
		return
	}
	if err := a.Manager.BackupVolumeMountExclusive(r.Context(), body.ServiceID, body.Volume); err != nil {
		if strings.Contains(err.Error(), "already in progress") {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (a *API) handleClusterRun(w http.ResponseWriter, r *http.Request) {
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
	if a.Manager == nil {
		http.Error(w, "backup manager unavailable", 503)
		return
	}
	if err := a.Manager.BackupClusterExclusive(r.Context()); err != nil {
		if strings.Contains(err.Error(), "already in progress") {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
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
	if a.Manager == nil {
		http.Error(w, "backup manager unavailable", 503)
		return
	}
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
	s3, _, err := a.Manager.resolveVolumeBackup(r.Context(), body.ServiceID, body.Volume)
	if err != nil || !s3.Valid() {
		http.Error(w, "backup not configured: "+errString(err), 400)
		return
	}
	// Stop the app first so nothing is writing to the volume during untar.
	prev, err := pauseService(r.Context(), a.Store, body.ServiceID)
	if err != nil {
		http.Error(w, "could not stop service before restore: "+err.Error(), 500)
		return
	}
	if a.Manager.docker != nil {
		var restoreErr error
		if body.Key != "" {
			restoreErr = a.Manager.RestoreKey(r.Context(), s3, body.Volume, body.Key)
		} else {
			restoreErr = a.Manager.RestoreLatest(r.Context(), s3, body.ServiceID, body.Volume)
		}
		resumeErr := resumeService(r.Context(), a.Store, body.ServiceID, prev)
		if restoreErr != nil {
			http.Error(w, restoreErr.Error(), 500)
			return
		}
		if resumeErr != nil {
			http.Error(w, "restored but failed to restart service: "+resumeErr.Error(), 500)
			return
		}
		_, _ = events.Emit(r.Context(), a.Store, "backup.restored", body.ServiceID,
			fmt.Sprintf("restored volume %s", body.Volume),
			map[string]string{"volume": body.Volume, "key": body.Key})
		writeJSON(w, map[string]any{"ok": true, "restored": true, "paused_replicas": prev})
		return
	}
	_, _ = events.Emit(r.Context(), a.Store, "volume.restore_requested", body.ServiceID,
		fmt.Sprintf("manual restore of volume %q", body.Volume),
		map[string]string{
			"volume": body.Volume, "key": body.Key,
			"prev_replicas": fmt.Sprintf("%d", prev),
		})
	writeJSON(w, map[string]any{"ok": true, "queued": true, "paused_replicas": prev})
}

func errString(err error) string {
	if err == nil {
		return "no S3 target"
	}
	return err.Error()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
