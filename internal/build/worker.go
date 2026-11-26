package build

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/deploy"
	"github.com/aetherix/aetherixcloud/internal/ids"
	"github.com/aetherix/aetherixcloud/internal/lease"
	"github.com/aetherix/aetherixcloud/internal/secrets"
	"github.com/aetherix/aetherixcloud/internal/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// SourceInfo describes the fetched commit.
type SourceInfo struct {
	SHA, Message, Author string
}

// Source fetches the source tree of a queued build into dir (GitHub clone).
type Source interface {
	Fetch(ctx context.Context, job *JobMeta, dir string, log io.Writer) (SourceInfo, error)
}

// Event is a progress notification for the reporter (GitHub statuses).
type Event struct {
	// Stage is "build" or "deploy".
	Stage string
	// State is "pending" | "success" | "failure".
	State       string
	Description string
	BuildID     string
}

// Reporter receives build and deploy progress. Implementations must not block.
type Reporter interface {
	Report(ctx context.Context, job *JobMeta, ev Event)
}

// DeployFunc creates a deployment (deploy.Start by default).
type DeployFunc func(ctx context.Context, st store.Store, svc *v1.Service, o deploy.StartOptions) (*v1.Deployment, error)

// BuildSecretPrefix marks variables that are passed to builds as BuildKit
// secrets (prefix stripped). All other variables stay runtime-only.
const BuildSecretPrefix = "BUILD_SECRET_"

// Worker runs queued builds on a machine with the builder role.
type Worker struct {
	Store    store.Store
	Leases   *lease.Manager
	Self     string
	Pipeline *Pipeline
	Logs     *LogStore
	Source   Source
	Reporter Reporter
	Deploy   DeployFunc
	Key      secrets.Key
	Logger   *slog.Logger
	// WorkDir holds source checkouts.
	WorkDir string
	// Concurrency is the max parallel builds on this node (default 2).
	Concurrency int
	// Stagger is the per-rank delay before a less-loaded builder's competitors
	// try to claim a build (default 1.5s).
	Stagger  time.Duration
	LeaseTTL time.Duration
	// DeployWait bounds waiting for the deployment to become active.
	DeployWait time.Duration
	// Poll is the queue scan interval (default 5s).
	Poll time.Duration

	mu       sync.Mutex
	inflight map[string]bool
}

func (w *Worker) defaults() {
	if w.Concurrency <= 0 {
		w.Concurrency = 2
	}
	if w.Stagger == 0 {
		w.Stagger = 1500 * time.Millisecond
	}
	if w.LeaseTTL == 0 {
		w.LeaseTTL = 30 * time.Second
	}
	if w.DeployWait == 0 {
		w.DeployWait = 15 * time.Minute
	}
	if w.Poll == 0 {
		w.Poll = 5 * time.Second
	}
	if w.Logger == nil {
		w.Logger = slog.Default()
	}
	if w.Deploy == nil {
		w.Deploy = deploy.Start
	}
	if w.inflight == nil {
		w.inflight = map[string]bool{}
	}
}

// RankBuilders orders eligible builder machines least-loaded first (ties:
// lowest machine ID). Machines with the builder role that are not DOWN are
// eligible; if none exist every non-DOWN machine is (so a cluster without an
// explicit builder still builds).
func RankBuilders(machines []*v1.Machine, running map[string]int) []string {
	pick := func(requireRole bool) []*v1.Machine {
		var out []*v1.Machine
		for _, m := range machines {
			if m.State == v1.MachineState_MACHINE_STATE_DOWN || m.Draining {
				continue
			}
			if requireRole {
				ok := false
				for _, r := range m.Roles {
					if r == v1.MachineRole_MACHINE_ROLE_BUILDER {
						ok = true
					}
				}
				if !ok {
					continue
				}
			}
			out = append(out, m)
		}
		return out
	}
	c := pick(true)
	if len(c) == 0 {
		c = pick(false)
	}
	sort.Slice(c, func(i, j int) bool {
		li, lj := running[c[i].Id], running[c[j].Id]
		if li != lj {
			return li < lj
		}
		return c[i].Id < c[j].Id
	})
	order := make([]string, len(c))
	for i, m := range c {
		order[i] = m.Id
	}
	return order
}

// Load returns the running build count per builder machine.
func (w *Worker) load(ctx context.Context) map[string]int {
	out := map[string]int{}
	bs, _ := store.ListProto[v1.Build](ctx, w.Store, store.TableBuilds, store.Query{Where: map[string]string{"status": StatusRunning}})
	for _, b := range bs {
		out[b.BuilderMachineId]++
	}
	return out
}

// Rank returns this machine's position among builders (-1 if ineligible).
func (w *Worker) Rank(ctx context.Context) int {
	ms, err := store.ListMachines(ctx, w.Store)
	if err != nil {
		return -1
	}
	for i, id := range RankBuilders(ms, w.load(ctx)) {
		if id == w.Self {
			return i
		}
	}
	return -1
}

// Run processes the build queue until ctx is done.
func (w *Worker) Run(ctx context.Context) error {
	w.defaults()
	ch, err := w.Store.Watch(ctx, store.TableBuilds)
	if err != nil {
		return err
	}
	t := time.NewTicker(w.Poll)
	defer t.Stop()
	for {
		w.scan(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-ch:
		case <-t.C:
		}
	}
}

func (w *Worker) scan(ctx context.Context) {
	w.reapStale(ctx)
	// Refresh from cluster setting each scan so UI changes apply without restart.
	if n := EffectiveMaxParallel(ctx, w.Store); n > 0 {
		w.Concurrency = n
	}
	recs, err := w.Store.List(ctx, store.TableBuilds, store.Query{Where: map[string]string{"status": StatusQueued}, OrderBy: "updated_at"})
	if err != nil || len(recs) == 0 {
		return
	}
	for _, r := range recs {
		w.mu.Lock()
		busy := w.inflight[r.ID] || len(w.inflight) >= w.Concurrency
		if !busy {
			w.inflight[r.ID] = true
		}
		w.mu.Unlock()
		if busy {
			continue // leave queued — picked up on a later scan when a slot frees
		}
		go func(id string) {
			defer func() { w.mu.Lock(); delete(w.inflight, id); w.mu.Unlock() }()
			w.tryClaim(ctx, id)
		}(r.ID)
	}
}

func (w *Worker) tryClaim(ctx context.Context, id string) {
	rank := w.Rank(ctx)
	if rank < 0 {
		return
	}
	if rank > 0 {
		select {
		case <-time.After(time.Duration(rank) * w.Stagger):
		case <-ctx.Done():
			return
		}
	}
	b, err := GetBuild(ctx, w.Store, id)
	if err != nil || b.Status != StatusQueued {
		return
	}
	l, err := w.Leases.Acquire(ctx, "build:"+id, w.LeaseTTL)
	if err != nil {
		return
	}
	bctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	go w.renew(bctx, cancel, l, done)
	defer l.Release(context.WithoutCancel(ctx))
	// Re-read under the lease: another builder may have finished it already.
	b, err = GetBuild(bctx, w.Store, id)
	if err != nil || b.Status != StatusQueued {
		return
	}
	w.runQueued(bctx, b, l.Fence)
}

func (w *Worker) renew(ctx context.Context, cancel context.CancelFunc, l *lease.Lease, done <-chan struct{}) {
	t := time.NewTicker(w.LeaseTTL / 3)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			if err := l.Renew(ctx); err != nil && (errors.Is(err, lease.ErrLost) || !l.Valid()) {
				w.Logger.Warn("build lease lost", "lease", l.Name)
				cancel()
				return
			}
		}
	}
}

// reapStale fails running builds whose builder vanished (no lease holder).
func (w *Worker) reapStale(ctx context.Context) {
	recs, err := w.Store.List(ctx, store.TableBuilds, store.Query{Where: map[string]string{"status": StatusRunning}})
	if err != nil {
		return
	}
	for _, r := range recs {
		if time.Since(time.Unix(0, r.UpdatedAt)) < w.LeaseTTL*2 {
			continue
		}
		var b v1.Build
		if store.UnmarshalProto(r, &b) != nil {
			continue
		}
		if h, _ := w.Leases.Holder(ctx, "build:"+b.Id); h != nil {
			continue
		}
		b.Status, b.FinishedAt = StatusFailed, timestamppb.Now()
		_ = PutBuild(ctx, w.Store, &b)
		w.Logger.Warn("build builder lost; marked failed", "build", b.Id)
		if job, err := LoadJob(ctx, w.Store, b.Id); err == nil && w.Reporter != nil {
			w.Reporter.Report(ctx, job, Event{Stage: "build", State: "failure", Description: "builder node lost", BuildID: b.Id})
		}
	}
}

func (w *Worker) runQueued(ctx context.Context, b *v1.Build, fence int64) {
	job, err := LoadJob(ctx, w.Store, b.Id)
	if err != nil {
		w.fail(ctx, b, nil, nil, fmt.Errorf("build job metadata missing: %w", err))
		return
	}
	svc, err := store.GetService(ctx, w.Store, b.ServiceId)
	if err != nil {
		w.fail(ctx, b, job, nil, fmt.Errorf("service %s no longer exists", b.ServiceId))
		return
	}
	b.Status, b.BuilderMachineId = StatusRunning, w.Self
	b.LogPointer = "node:" + w.Self + "/" + b.Id
	_ = PutBuild(ctx, w.Store, b)
	lw, err := w.Logs.Writer(b.Id)
	if err != nil {
		w.fail(ctx, b, job, nil, err)
		return
	}
	defer lw.Close()
	lw.System(fmt.Sprintf("build %s on %s (lease fence %d)", b.Id, w.Self, fence))
	dir, err := os.MkdirTemp(w.WorkDir, "ax-src-")
	if err != nil {
		w.fail(ctx, b, job, lw, err)
		return
	}
	defer os.RemoveAll(dir)
	w.report(ctx, job, "build", "pending", "building", b.Id)
	info, err := w.Source.Fetch(ctx, job, dir, lw)
	if err != nil {
		w.fail(ctx, b, job, lw, fmt.Errorf("fetch source: %w", err))
		return
	}
	if info.SHA != "" {
		b.CommitSha = info.SHA
		job.SHA, job.Message, job.Author = info.SHA, firstNonEmpty(info.Message, job.Message), firstNonEmpty(info.Author, job.Author)
	}
	w.execute(ctx, b, svc, job, dir, lw)
}

// RunUpload builds an uploaded source tree synchronously on this node (`ax up`)
// and streams output to log. The build row is created in the running state.
func (w *Worker) RunUpload(ctx context.Context, svc *v1.Service, srcDir string, log io.Writer) (*v1.Build, error) {
	w.defaults()
	b := &v1.Build{Id: ids.New(), ServiceId: svc.Id, Status: StatusRunning, BuilderMachineId: w.Self,
		Provider: "", CreatedAt: timestamppb.Now()}
	b.LogPointer = "node:" + w.Self + "/" + b.Id
	job := &JobMeta{BuildID: b.Id, ServiceID: svc.Id, Kind: "upload"}
	if err := store.PutJSON(ctx, w.Store, store.TableSettings, jobPrefix+b.Id, nil, job); err != nil {
		return nil, err
	}
	if err := PutBuild(ctx, w.Store, b); err != nil {
		return nil, err
	}
	lw, err := w.Logs.Writer(b.Id)
	if err != nil {
		return nil, err
	}
	defer lw.Close()
	lw.System("upload build " + b.Id)
	w.execute(ctx, b, svc, job, srcDir, io.MultiWriter(lw, log))
	nb, err := GetBuild(ctx, w.Store, b.Id)
	if err != nil {
		return b, nil
	}
	if nb.Status == StatusFailed {
		return nb, errors.New("build failed")
	}
	return nb, nil
}

// execute runs the pipeline for b and deploys the result.
func (w *Worker) execute(ctx context.Context, b *v1.Build, svc *v1.Service, job *JobMeta, srcDir string, log io.Writer) {
	proj, _ := store.GetProto[v1.Project](ctx, w.Store, store.TableProjects, svc.ProjectId)
	projName := svc.ProjectId
	if proj != nil {
		projName = proj.Name
	}
	tag := shortTag(job.SHA, b.Id)
	sec := w.buildSecrets(ctx, svc)
	res, err := w.Pipeline.Run(ctx, Job{SrcDir: srcDir, Settings: SettingsFromSpec(svc.Spec.GetBuild(), svc.Name),
		Project: projName, Service: svc.Name, Tag: tag, Secrets: sec, Log: log})
	if err != nil {
		w.fail(ctx, b, job, log, err)
		return
	}
	b.Provider = string(res.Provider)
	w.finish(ctx, b, svc, job, res.Image, res.Digest, log)
}

// finish records a successful build of image and rolls it out.
func (w *Worker) finish(ctx context.Context, b *v1.Build, svc *v1.Service, job *JobMeta, image, digest string, log io.Writer) {
	b.Image, b.Status, b.FinishedAt = image, StatusSucceeded, timestamppb.Now()
	_ = PutBuild(ctx, w.Store, b)
	fmt.Fprintf(log, "==> build succeeded: %s\n", image)
	w.report(ctx, job, "build", "success", "build succeeded", b.Id)
	w.event(ctx, "build.succeeded", b, "build succeeded: "+image)

	// Deploy: re-read the service (it may have been edited during the build).
	cur, err := store.GetService(ctx, w.Store, svc.Id)
	if err != nil {
		fmt.Fprintf(log, "==> service %s vanished, not deploying\n", svc.Id)
		return
	}
	// Optional S3 archive (BuildSpec.image_retain > 0 + Settings → S3 Targets).
	w.archiveImage(ctx, cur, image, digest, log)

	cur.Spec.Image = image
	cur.UpdatedAt = timestamppb.Now()
	_ = store.PutService(ctx, w.Store, cur) // keep the built image on the service even if the rollout fails
	dep, err := w.Deploy(ctx, w.Store, cur, deploy.StartOptions{CommitSHA: job.SHA, CommitMsg: job.Message,
		Author: job.Author, BuildID: b.Id, ImageDigest: digest})
	if err != nil {
		fmt.Fprintf(log, "==> deploy failed: %v\n", err)
		w.report(ctx, job, "deploy", "failure", "deploy failed: "+err.Error(), b.Id)
		return
	}
	fmt.Fprintf(log, "==> deployment %s created\n", dep.Id)
	w.report(ctx, job, "deploy", "pending", "deploying", b.Id)
	go w.watchDeployment(context.WithoutCancel(ctx), job, b.Id, dep.Id)
}

func (w *Worker) watchDeployment(ctx context.Context, job *JobMeta, buildID, depID string) {
	deadline := time.Now().Add(w.DeployWait)
	poll := 2 * time.Second
	for time.Now().Before(deadline) {
		d, err := store.GetDeployment(ctx, w.Store, depID)
		if err == nil {
			switch d.Status {
			case v1.DeploymentStatus_DEPLOYMENT_STATUS_ACTIVE:
				w.report(ctx, job, "deploy", "success", "deployed", buildID)
				return
			case v1.DeploymentStatus_DEPLOYMENT_STATUS_FAILED, v1.DeploymentStatus_DEPLOYMENT_STATUS_ROLLED_BACK:
				w.report(ctx, job, "deploy", "failure", "deployment "+strings.ToLower(strings.TrimPrefix(d.Status.String(), "DEPLOYMENT_STATUS_"))+": "+d.StatusMessage, buildID)
				return
			case v1.DeploymentStatus_DEPLOYMENT_STATUS_SUPERSEDED:
				return
			}
		}
		time.Sleep(poll)
	}
	w.report(ctx, job, "deploy", "failure", "timed out waiting for the deployment", buildID)
}

func (w *Worker) fail(ctx context.Context, b *v1.Build, job *JobMeta, log io.Writer, err error) {
	w.Logger.Warn("build failed", "build", b.Id, "err", err)
	if log != nil {
		fmt.Fprintf(log, "==> build failed: %v\n", err)
	}
	b.Status, b.FinishedAt = StatusFailed, timestamppb.Now()
	_ = PutBuild(context.WithoutCancel(ctx), w.Store, b)
	w.event(ctx, "build.failed", b, "build failed: "+err.Error())
	if job != nil {
		w.report(context.WithoutCancel(ctx), job, "build", "failure", truncate("build failed: "+err.Error(), 140), b.Id)
	}
}

func (w *Worker) report(ctx context.Context, job *JobMeta, stage, state, desc, buildID string) {
	if w.Reporter != nil && job != nil {
		w.Reporter.Report(ctx, job, Event{Stage: stage, State: state, Description: desc, BuildID: buildID})
	}
}

func (w *Worker) event(ctx context.Context, typ string, b *v1.Build, msg string) {
	_ = store.PutEvent(ctx, w.Store, &v1.Event{Id: ids.New(), Type: typ, SubjectId: b.Id, Message: msg,
		Time: timestamppb.Now(), Attributes: map[string]string{"service_id": b.ServiceId}})
}

// buildSecrets collects BUILD_SECRET_* variables of the service's environment.
func (w *Worker) buildSecrets(ctx context.Context, svc *v1.Service) map[string]string {
	vars, err := store.ListProto[v1.Variable](ctx, w.Store, store.TableVariables, store.Query{Where: map[string]string{"environment_id": svc.EnvironmentId}})
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, pass := range []string{"", svc.Id} { // shared first, service overrides
		for _, v := range vars {
			if v.ServiceId != pass || !strings.HasPrefix(v.Key, BuildSecretPrefix) {
				continue
			}
			val := v.Value
			if v.Secret {
				bs, err := w.Key.Open(val)
				if err != nil {
					continue
				}
				val = string(bs)
			}
			out[strings.TrimPrefix(v.Key, BuildSecretPrefix)] = val
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func shortTag(sha, buildID string) string {
	suffix := buildID
	if len(suffix) > 8 {
		suffix = suffix[len(suffix)-8:]
	}
	if len(sha) >= 7 {
		return sha[:7] + "-" + suffix
	}
	return "up-" + suffix
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

var (
	loadedRe = regexp.MustCompile(`Loaded image(?: ID)?: (\S+)`)
	digestRe = regexp.MustCompile(`digest: (sha256:[0-9a-f]{64})`)
)

// RunImageUpload loads a `docker save` stream on this node, tags it into the
// embedded registry, pushes it and deploys it (`ax build --local` /
// `ax image push`). Requires the docker CLI on the builder node.
func (w *Worker) RunImageUpload(ctx context.Context, svc *v1.Service, image io.Reader, log io.Writer) (*v1.Build, error) {
	w.defaults()
	b := &v1.Build{Id: ids.New(), ServiceId: svc.Id, Status: StatusRunning, BuilderMachineId: w.Self,
		Provider: string(ProviderImage), CreatedAt: timestamppb.Now()}
	b.LogPointer = "node:" + w.Self + "/" + b.Id
	job := &JobMeta{BuildID: b.Id, ServiceID: svc.Id, Kind: "upload"}
	if err := store.PutJSON(ctx, w.Store, store.TableSettings, jobPrefix+b.Id, nil, job); err != nil {
		return nil, err
	}
	if err := PutBuild(ctx, w.Store, b); err != nil {
		return nil, err
	}
	lw, err := w.Logs.Writer(b.Id)
	if err != nil {
		return nil, err
	}
	defer lw.Close()
	out := io.MultiWriter(lw, log)
	ref, digest, err := w.loadAndPush(ctx, svc, b, image, out)
	if err != nil {
		w.fail(ctx, b, job, out, err)
		return b, err
	}
	w.finish(ctx, b, svc, job, ref, digest, out)
	return b, nil
}

func (w *Worker) loadAndPush(ctx context.Context, svc *v1.Service, b *v1.Build, image io.Reader, out io.Writer) (ref, digest string, err error) {
	dir, err := os.MkdirTemp(w.WorkDir, "ax-img-")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(dir)
	f := filepath.Join(dir, "image.tar")
	fh, err := os.Create(f)
	if err != nil {
		return "", "", err
	}
	_, cerr := io.Copy(fh, image)
	if e := fh.Close(); cerr == nil {
		cerr = e
	}
	if cerr != nil {
		return "", "", fmt.Errorf("receive image: %w", cerr)
	}
	run := w.Pipeline.Runner
	var loaded captureWriter
	loaded.w = out
	fmt.Fprintln(out, "==> docker load")
	if err := run.Run(ctx, Cmd{Name: "docker", Args: []string{"load", "-i", f}}, &loaded); err != nil {
		return "", "", err
	}
	m := loadedRe.FindStringSubmatch(string(loaded.buf))
	if m == nil {
		return "", "", errors.New("docker load did not report a loaded image")
	}
	proj, _ := store.GetProto[v1.Project](ctx, w.Store, store.TableProjects, svc.ProjectId)
	pn := svc.ProjectId
	if proj != nil {
		pn = proj.Name
	}
	ref = ImageRef(pn, svc.Name, shortTag("", b.Id))
	if err := run.Run(ctx, Cmd{Name: "docker", Args: []string{"tag", m[1], ref}}, out); err != nil {
		return "", "", err
	}
	var pushed captureWriter
	pushed.w = out
	fmt.Fprintln(out, "==> docker push "+ref)
	if err := run.Run(ctx, Cmd{Name: "docker", Args: []string{"push", ref}}, &pushed); err != nil {
		return "", "", err
	}
	if d := digestRe.FindStringSubmatch(string(pushed.buf)); d != nil {
		digest = d[1]
	}
	return ref, digest, nil
}
