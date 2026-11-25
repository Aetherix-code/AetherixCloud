// Package cron runs scheduled cluster jobs (restart/deploy a service) and
// exposes a small HTTP API for CRUD. Expressions are classic 5-field cron.
package cron

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/deploy"
	"github.com/aetherix/aetherixcloud/internal/events"
	"github.com/aetherix/aetherixcloud/internal/ids"
	"github.com/aetherix/aetherixcloud/internal/store"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	jobPrefix = "cron/job/"
	Path      = "/api/cron"
)

// Action kinds.
const (
	ActionRestart = "restart"
	ActionDeploy  = "deploy"
)

// Job is a scheduled action stored in settings.
type Job struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Schedule  string    `json:"schedule"` // 5-field cron
	ServiceID string    `json:"service_id"`
	Action    string    `json:"action"` // restart|deploy
	Enabled   bool      `json:"enabled"`
	LastRun   time.Time `json:"last_run,omitempty"`
	LastError string    `json:"last_error,omitempty"`
}

// Runner claims and executes due jobs. Only the lowest machine ID runs jobs
// (deterministic single-actor without a lease for simplicity; lease can be
// layered later).
type Runner struct {
	Store    store.Store
	Self     string
	Logger   *slog.Logger
	Interval time.Duration
	// Deploy creates a new deployment for ActionDeploy (optional).
	Deploy func(ctx context.Context, serviceID string) error
	// Restart flips DesiredRunning off then on for ActionRestart.
	Restart func(ctx context.Context, serviceID string) error
}

// NewRunner builds a Runner with default deploy/restart helpers.
func NewRunner(st store.Store, self string, log *slog.Logger) *Runner {
	r := &Runner{Store: st, Self: self, Logger: log, Interval: time.Minute}
	if log == nil {
		r.Logger = slog.Default()
	}
	r.Deploy = func(ctx context.Context, serviceID string) error {
		svc, err := store.GetService(ctx, st, serviceID)
		if err != nil {
			return err
		}
		_, err = deploy.Start(ctx, st, svc, deploy.StartOptions{Author: "cron"})
		return err
	}
	r.Restart = func(ctx context.Context, serviceID string) error {
		reps, err := store.ListReplicas(ctx, st, map[string]string{"service_id": serviceID})
		if err != nil {
			return err
		}
		now := timestamppb.Now()
		for _, rep := range reps {
			n := proto.Clone(rep).(*v1.Replica)
			n.DesiredRunning = false
			n.UpdatedAt = now
			_ = store.PutReplica(ctx, st, n)
		}
		time.Sleep(500 * time.Millisecond)
		for _, rep := range reps {
			n := proto.Clone(rep).(*v1.Replica)
			n.DesiredRunning = true
			n.UpdatedAt = timestamppb.Now()
			_ = store.PutReplica(ctx, st, n)
		}
		_, _ = events.Emit(ctx, st, "cron.restart", serviceID, "scheduled restart", nil)
		return nil
	}
	return r
}

// Run ticks until ctx is done.
func (r *Runner) Run(ctx context.Context) {
	t := time.NewTicker(r.Interval)
	defer t.Stop()
	var lastSlot string
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if !r.isLeader(ctx) {
				continue
			}
			slot := now.UTC().Format("2006-01-02-15-04")
			if slot == lastSlot {
				continue
			}
			lastSlot = slot
			r.tick(ctx, now.UTC())
		}
	}
}

func (r *Runner) isLeader(ctx context.Context) bool {
	ms, err := store.ListMachines(ctx, r.Store)
	if err != nil || len(ms) == 0 {
		return true
	}
	lowest := r.Self
	for _, m := range ms {
		if m.State == v1.MachineState_MACHINE_STATE_DOWN {
			continue
		}
		if m.Id < lowest {
			lowest = m.Id
		}
	}
	return lowest == r.Self
}

func (r *Runner) tick(ctx context.Context, now time.Time) {
	jobs, err := ListJobs(ctx, r.Store)
	if err != nil {
		return
	}
	for _, j := range jobs {
		if !j.Enabled {
			continue
		}
		ok, err := Match(j.Schedule, now)
		if err != nil || !ok {
			continue
		}
		// skip if already ran in this minute
		if !j.LastRun.IsZero() && j.LastRun.UTC().Truncate(time.Minute).Equal(now.Truncate(time.Minute)) {
			continue
		}
		j.LastError = ""
		var runErr error
		switch j.Action {
		case ActionDeploy:
			runErr = r.Deploy(ctx, j.ServiceID)
		case ActionRestart:
			runErr = r.Restart(ctx, j.ServiceID)
		default:
			runErr = fmt.Errorf("unknown action %q", j.Action)
		}
		j.LastRun = now
		if runErr != nil {
			j.LastError = runErr.Error()
			r.Logger.Warn("cron job failed", "job", j.ID, "err", runErr)
			_, _ = events.Emit(ctx, r.Store, "cron.error", j.ID, runErr.Error(),
				map[string]string{"service_id": j.ServiceID, "action": j.Action})
		} else {
			_, _ = events.Emit(ctx, r.Store, "cron.ok", j.ID, fmt.Sprintf("%s %s", j.Action, j.ServiceID),
				map[string]string{"service_id": j.ServiceID, "action": j.Action})
		}
		_ = PutJob(ctx, r.Store, j)
	}
}

// ListJobs returns all cron jobs.
func ListJobs(ctx context.Context, st store.Store) ([]Job, error) {
	recs, err := st.List(ctx, store.TableSettings, store.Query{})
	if err != nil {
		return nil, err
	}
	var out []Job
	for _, r := range recs {
		if !strings.HasPrefix(r.ID, jobPrefix) {
			continue
		}
		var j Job
		if json.Unmarshal(r.Data, &j) != nil {
			continue
		}
		out = append(out, j)
	}
	return out, nil
}

// PutJob upserts a job.
func PutJob(ctx context.Context, st store.Store, j Job) error {
	if j.ID == "" {
		j.ID = ids.New()
	}
	if j.Action == "" {
		j.Action = ActionRestart
	}
	return store.PutJSON(ctx, st, store.TableSettings, jobPrefix+j.ID, nil, j)
}

// DeleteJob removes a job.
func DeleteJob(ctx context.Context, st store.Store, id string) error {
	return st.Delete(ctx, store.TableSettings, jobPrefix+id)
}

// API serves /api/cron.
type API struct {
	Store        store.Store
	Authenticate func(*http.Request) error
}

func (a *API) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.Authenticate != nil {
			if err := a.Authenticate(r); err != nil {
				http.Error(w, err.Error(), http.StatusUnauthorized)
				return
			}
		}
		path := strings.TrimPrefix(r.URL.Path, Path)
		path = strings.Trim(path, "/")
		switch {
		case path == "" && r.Method == http.MethodGet:
			jobs, err := ListJobs(r.Context(), a.Store)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			writeJSON(w, map[string]any{"jobs": jobs})
		case path == "" && r.Method == http.MethodPost:
			var j Job
			if err := json.NewDecoder(r.Body).Decode(&j); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			if j.Schedule == "" || j.ServiceID == "" {
				http.Error(w, "schedule and service_id required", 400)
				return
			}
			if _, err := Match(j.Schedule, time.Now()); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			j.Enabled = true
			if err := PutJob(r.Context(), a.Store, j); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			writeJSON(w, j)
		case path != "" && r.Method == http.MethodDelete:
			if err := DeleteJob(r.Context(), a.Store, path); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			writeJSON(w, map[string]any{"ok": true})
		case path != "" && r.Method == http.MethodPut:
			var j Job
			if err := json.NewDecoder(r.Body).Decode(&j); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			j.ID = path
			if err := PutJob(r.Context(), a.Store, j); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			writeJSON(w, j)
		default:
			http.Error(w, "method not allowed", 405)
		}
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
