package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/build"
	"github.com/aetherix/aetherixcloud/internal/store"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Labels set on preview services.
const (
	LabelPreviewOf = "aetherix.preview-of"
	LabelPreviewPR = "aetherix.preview-pr"
)

func isPreview(svc *v1.Service) bool { return svc.GetSpec().GetLabels()[LabelPreviewOf] != "" }

// detID derives a stable lowercase ID from parts (so nodes handling the same
// or consecutive webhook deliveries converge on one row).
func detID(prefix string, parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return prefix + hex.EncodeToString(h[:12])
}

// PreviewHostname is pr-<n>-<service>.<domain>.
func PreviewHostname(pr int, service, domain string) string {
	return fmt.Sprintf("pr-%d-%s.%s", pr, service, domain)
}

func (m *Module) previewUpsert(ctx context.Context, delivery string, ev *prEvent) (*EventResult, error) {
	repo := ev.Repository.FullName
	if ev.PullRequest.Head.Repo.FullName != "" && !strings.EqualFold(ev.PullRequest.Head.Repo.FullName, repo) {
		return &EventResult{Status: "ignored", Note: "fork pull request: previews run only for same-repo branches"}, nil
	}
	svcs, err := store.ListServices(ctx, m.Store, nil)
	if err != nil {
		return nil, err
	}
	res := &EventResult{Status: "ok"}
	for _, parent := range svcs {
		gh := parent.Spec.GetSource().GetGithub()
		if gh == nil || !gh.PrPreviews || !strings.EqualFold(gh.Repo, repo) || isPreview(parent) {
			continue
		}
		pv, created, err := m.ensurePreview(ctx, parent, ev.Number, ev.PullRequest.Head.Ref)
		if err != nil {
			return nil, err
		}
		inst := firstNonZero(ev.Installation.ID, gh.InstallationId)
		b, _, err := build.Enqueue(ctx, m.Store, build.JobMeta{
			BuildID:   build.DeterministicBuildID(delivery, pv.Id),
			ServiceID: pv.Id, Kind: "github", Repo: repo, InstallationID: inst,
			Ref: fmt.Sprintf("refs/pull/%d/head", ev.Number), SHA: ev.PullRequest.Head.SHA, PRNumber: ev.Number,
			Message: ev.PullRequest.Title, Author: ev.PullRequest.User.Login, Delivery: delivery,
		})
		if err != nil {
			return nil, err
		}
		res.Builds = append(res.Builds, b.Id)
		if created {
			m.commentPreview(ctx, inst, repo, ev.Number, pv)
		}
	}
	if len(res.Builds) == 0 {
		res.Status, res.Note = "ignored", "no service with PR previews enabled for "+repo
	}
	return res, nil
}

func (m *Module) commentPreview(ctx context.Context, inst int64, repo string, pr int, pv *v1.Service) {
	body := fmt.Sprintf("Preview environment `pr-%d` for **%s** is being built.", pr, pv.Name)
	for _, p := range pv.Spec.Ports {
		if p.Hostname != "" {
			body += fmt.Sprintf(" It will be available at https://%s once the deployment is healthy.", p.Hostname)
			break
		}
	}
	c, err := m.Client(ctx)
	if err != nil {
		return
	}
	if inst == 0 {
		if inst, err = m.resolveInstallation(ctx, 0, repo); err != nil {
			return
		}
	}
	if err := c.CommentPR(ctx, inst, repo, pr, body); err != nil {
		m.logger().Debug("preview comment failed", "repo", repo, "pr", pr, "err", err)
	}
}

// ensurePreview returns the preview service of parent for PR n, creating the
// ephemeral environment, the service and copies of the variables if needed.
func (m *Module) ensurePreview(ctx context.Context, parent *v1.Service, n int, headRef string) (*v1.Service, bool, error) {
	envName := fmt.Sprintf("pr-%d", n)
	envID := detID("pe", parent.ProjectId, envName)
	if _, err := store.GetProto[v1.Environment](ctx, m.Store, store.TableEnvironments, envID); err != nil {
		if !store.IsNotFound(err) {
			return nil, false, err
		}
		if err := store.PutEnvironment(ctx, m.Store, &v1.Environment{Id: envID, ProjectId: parent.ProjectId, Name: envName,
			Ephemeral: true, CreatedAt: timestamppb.Now()}); err != nil {
			return nil, false, err
		}
	}
	svcID := detID("ps", parent.Id, envName)
	if ex, err := store.GetService(ctx, m.Store, svcID); err == nil {
		// Follow the PR branch if it was renamed.
		if g := ex.Spec.GetSource().GetGithub(); g != nil && headRef != "" && g.Branch != headRef {
			g.Branch = headRef
			_ = store.PutService(ctx, m.Store, ex)
		}
		return ex, false, nil
	} else if !store.IsNotFound(err) {
		return nil, false, err
	}
	spec := proto.Clone(parent.Spec).(*v1.ServiceSpec)
	spec.Image = ""
	spec.Source.Image = ""
	spec.Source.Github.PrPreviews = false
	spec.Source.Github.Branch = headRef
	if spec.Replicas > 1 {
		spec.Replicas = 1
	}
	if spec.Labels == nil {
		spec.Labels = map[string]string{}
	}
	spec.Labels[LabelPreviewOf] = parent.Id
	spec.Labels[LabelPreviewPR] = strconv.Itoa(n)
	if cl, err := store.GetCluster(ctx, m.Store); err == nil && cl.Domain != "" {
		for _, p := range spec.Ports {
			if p.Mode == "http" || p.Mode == "https" {
				p.Hostname = PreviewHostname(n, parent.Name, cl.Domain)
			}
		}
	} else {
		for _, p := range spec.Ports {
			if p.Mode == "http" || p.Mode == "https" {
				p.Hostname = "" // ingress generates a default hostname
			}
		}
	}
	pv := &v1.Service{Id: svcID, ProjectId: parent.ProjectId, EnvironmentId: envID, Name: parent.Name, Spec: spec,
		CreatedAt: timestamppb.Now(), UpdatedAt: timestamppb.Now()}
	if err := store.PutService(ctx, m.Store, pv); err != nil {
		return nil, false, err
	}
	if err := m.copyVariables(ctx, parent, pv); err != nil {
		return nil, false, err
	}
	return pv, true, nil
}

// copyVariables copies shared and service variables of the parent's
// environment into the preview environment (secret values stay sealed with
// the same cluster key).
func (m *Module) copyVariables(ctx context.Context, parent, pv *v1.Service) error {
	vars, err := store.ListProto[v1.Variable](ctx, m.Store, store.TableVariables,
		store.Query{Where: map[string]string{"environment_id": parent.EnvironmentId}})
	if err != nil {
		return err
	}
	for _, v := range vars {
		if v.ServiceId != "" && v.ServiceId != parent.Id {
			continue
		}
		nv := proto.Clone(v).(*v1.Variable)
		nv.EnvironmentId = pv.EnvironmentId
		if v.ServiceId != "" {
			nv.ServiceId = pv.Id
		}
		nv.Id = detID("pv", pv.EnvironmentId, nv.ServiceId, v.Key)
		if err := store.PutProto(ctx, m.Store, store.TableVariables, nv.Id,
			map[string]string{"environment_id": nv.EnvironmentId, "service_id": nv.ServiceId, "key": nv.Key}, nv); err != nil {
			return err
		}
	}
	return nil
}

// previewTeardown removes every preview service (and then-empty ephemeral
// environments) of PR n of repo, stopping their replicas.
func (m *Module) previewTeardown(ctx context.Context, repo string, n int) (int, error) {
	svcs, err := store.ListServices(ctx, m.Store, nil)
	if err != nil {
		return 0, err
	}
	removed := 0
	envs := map[string]bool{}
	for _, svc := range svcs {
		if !isPreview(svc) || svc.Spec.Labels[LabelPreviewPR] != strconv.Itoa(n) {
			continue
		}
		if g := svc.Spec.GetSource().GetGithub(); g == nil || !strings.EqualFold(g.Repo, repo) {
			continue
		}
		if err := m.deleteService(ctx, svc); err != nil {
			return removed, err
		}
		envs[svc.EnvironmentId] = true
		removed++
	}
	for envID := range envs {
		left, _ := store.ListServices(ctx, m.Store, map[string]string{"environment_id": envID})
		if len(left) == 0 {
			vars, _ := m.Store.List(ctx, store.TableVariables, store.Query{Where: map[string]string{"environment_id": envID}})
			for _, v := range vars {
				_ = m.Store.Delete(ctx, store.TableVariables, v.ID)
			}
			_ = m.Store.Delete(ctx, store.TableEnvironments, envID)
		}
	}
	if removed > 0 && m.Trigger != nil {
		m.Trigger()
	}
	return removed, nil
}

// deleteService mirrors ServiceService.DeleteService: replicas are marked
// stopping (the owning node's reconciler removes the containers), then the
// service's domains, variables and row are deleted.
func (m *Module) deleteService(ctx context.Context, svc *v1.Service) error {
	reps, _ := store.ListReplicas(ctx, m.Store, map[string]string{"service_id": svc.Id})
	for _, r := range reps {
		r.DesiredRunning = false
		r.State = v1.ReplicaState_REPLICA_STATE_STOPPING
		_ = store.PutReplica(ctx, m.Store, r)
	}
	doms, _ := store.ListDomains(ctx, m.Store, svc.Id)
	for _, d := range doms {
		_ = m.Store.Delete(ctx, store.TableDomains, d.Id)
	}
	vars, _ := m.Store.List(ctx, store.TableVariables, store.Query{Where: map[string]string{"service_id": svc.Id}})
	for _, v := range vars {
		_ = m.Store.Delete(ctx, store.TableVariables, v.ID)
	}
	return m.Store.Delete(ctx, store.TableServices, svc.Id)
}
