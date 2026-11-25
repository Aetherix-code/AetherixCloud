package github

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/aetherix/aetherixcloud/internal/build"
)

// Status contexts.
const (
	ContextBuild  = "aetherix/build"
	ContextDeploy = "aetherix/deploy"
)

type statusTracker struct {
	mu     sync.Mutex
	checks map[string]int64 // buildID/stage -> check run id
}

// Reporter posts commit statuses and check runs. It implements
// build.Reporter; calls are asynchronous and best-effort.
type Reporter struct {
	Module *Module
	// Sync makes Report block (tests).
	Sync bool
}

// Report implements build.Reporter.
func (r *Reporter) Report(ctx context.Context, job *build.JobMeta, ev build.Event) {
	if job == nil || job.Kind != "github" || job.Repo == "" || job.SHA == "" {
		return
	}
	do := func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		if err := r.Module.report(cctx, job, ev); err != nil {
			r.Module.logger().Warn("github status report failed", "repo", job.Repo, "sha", short(job.SHA), "err", err)
		}
	}
	if r.Sync {
		do()
		return
	}
	go do()
}

func (m *Module) report(ctx context.Context, job *build.JobMeta, ev build.Event) error {
	c, err := m.Client(ctx)
	if err != nil {
		return err
	}
	inst, err := m.resolveInstallation(ctx, job.InstallationID, job.Repo)
	if err != nil {
		return err
	}
	stage, sctx := ev.Stage, ContextBuild
	if stage == "deploy" {
		sctx = ContextDeploy
	}
	state := ev.State
	if state != "pending" && state != "success" && state != "failure" {
		state = "error"
	}
	target := ""
	if d := strings.TrimRight(m.dashboard(ctx), "/"); d != "" {
		target = d + "/builds/" + ev.BuildID
	}
	err = c.CreateStatus(ctx, inst, job.Repo, job.SHA, Status{State: state, TargetURL: target, Description: ev.Description, Context: sctx})

	// Check run (needs checks:write; failures are not fatal).
	if m.statuses == nil {
		m.mu.Lock()
		if m.statuses == nil {
			m.statuses = &statusTracker{checks: map[string]int64{}}
		}
		m.mu.Unlock()
	}
	key := ev.BuildID + "/" + stage
	name := "Aetherix " + stage
	cr := CheckRun{DetailsURL: target}
	cr.Output = &struct {
		Title   string `json:"title"`
		Summary string `json:"summary"`
	}{Title: name, Summary: ev.Description}
	m.statuses.mu.Lock()
	id, ok := m.statuses.checks[key]
	m.statuses.mu.Unlock()
	switch {
	case !ok && state == "pending":
		cr.Name, cr.HeadSHA, cr.Status = name, job.SHA, "in_progress"
		if cid, cerr := c.CreateCheckRun(ctx, inst, job.Repo, cr); cerr == nil {
			m.statuses.mu.Lock()
			m.statuses.checks[key] = cid
			m.statuses.mu.Unlock()
		}
	case ok && state != "pending":
		cr.Status = "completed"
		cr.Conclusion = map[string]string{"success": "success", "failure": "failure"}[state]
		if cr.Conclusion == "" {
			cr.Conclusion = "failure"
		}
		if cerr := c.UpdateCheckRun(ctx, inst, job.Repo, id, cr); cerr == nil {
			m.statuses.mu.Lock()
			delete(m.statuses.checks, key)
			m.statuses.mu.Unlock()
		}
	case !ok && state != "pending":
		cr.Name, cr.HeadSHA, cr.Status = name, job.SHA, "completed"
		cr.Conclusion = map[string]string{"success": "success"}[state]
		if cr.Conclusion == "" {
			cr.Conclusion = "failure"
		}
		_, _ = c.CreateCheckRun(ctx, inst, job.Repo, cr)
	}
	return err
}
