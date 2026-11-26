// Package deploy creates deployments from service specs.
//
// OWNERSHIP: the rolling-deploy worker owns this package. Phase A provides
// only the minimal primitives the API and CLI need:
//
//   - Start: snapshot a service spec into an immutable Deployment and make it
//     the service's active deployment. The placement Controller
//     (internal/reconciler) then starts replicas of the new deployment first
//     and retires the old ones once enough new replicas are RUNNING. There is
//     NO health-gated rollback, surge control, ingress draining or
//     stop-first strategy yet: those belong to the rolling-deploy worker,
//     which should replace the "retire old deployment" logic in
//     reconciler.Controller.placeService with a DeployController driven by
//     Deployment.status.
//   - Rollback: Start with the spec of an earlier deployment.
//   - ResolveEnv: basic environment resolution (spec env + variables).
package deploy

import (
	"context"
	"errors"
	"fmt"
	"strings"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/ids"
	"github.com/aetherix/aetherixcloud/internal/secrets"
	"github.com/aetherix/aetherixcloud/internal/store"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// StartOptions tune Start.
type StartOptions struct {
	// SpecOverride replaces the service spec for this deployment only.
	SpecOverride *v1.ServiceSpec
	CommitSHA    string
	CommitMsg    string
	Author       string
	BuildID      string
	ImageDigest  string
	// PreviousID overrides the "previous deployment" link (rollbacks).
	PreviousID string
}

// ErrNoImage is returned when a service has no image to run yet.
var ErrNoImage = errors.New("service has no image: set spec.image or trigger a GitHub build")

// Start creates a deployment of svc and activates it.
func Start(ctx context.Context, st store.Store, svc *v1.Service, o StartOptions) (*v1.Deployment, error) {
	spec := svc.Spec
	if o.SpecOverride != nil {
		spec = o.SpecOverride
	}
	if spec == nil || spec.Image == "" {
		if spec != nil && spec.GetSource().GetGithub() != nil {
			return nil, errors.New("service has no image yet: trigger a build (Simulate push or push to the tracked branch)")
		}
		return nil, ErrNoImage
	}
	spec = proto.Clone(spec).(*v1.ServiceSpec)
	dep := &v1.Deployment{
		Id: ids.New(), ServiceId: svc.Id, Spec: spec, ImageDigest: o.ImageDigest,
		Status: v1.DeploymentStatus_DEPLOYMENT_STATUS_QUEUED, CommitSha: o.CommitSHA, CommitMessage: o.CommitMsg,
		Author: o.Author, BuildId: o.BuildID, PreviousDeploymentId: svc.ActiveDeploymentId, CreatedAt: timestamppb.Now(),
	}
	if o.PreviousID != "" {
		dep.PreviousDeploymentId = o.PreviousID
	}
	if err := store.PutDeployment(ctx, st, dep); err != nil {
		return nil, err
	}
	svc.ActiveDeploymentId = dep.Id
	svc.UpdatedAt = timestamppb.Now()
	if err := store.PutService(ctx, st, svc); err != nil {
		return nil, err
	}
	_ = store.PutEvent(ctx, st, &v1.Event{Id: ids.New(), Type: "deployment.created", SubjectId: dep.Id,
		Message: fmt.Sprintf("deployment of %s started (%s)", svc.Name, spec.Image), Time: timestamppb.Now(),
		Attributes: map[string]string{"service_id": svc.Id}})
	return dep, nil
}

// Rollback redeploys the spec of target (default: the newest earlier
// deployment that became active/superseded).
func Rollback(ctx context.Context, st store.Store, svc *v1.Service, targetID string) (*v1.Deployment, error) {
	var target *v1.Deployment
	if targetID != "" {
		d, err := store.GetDeployment(ctx, st, targetID)
		if err != nil || d.ServiceId != svc.Id {
			return nil, fmt.Errorf("deployment %q not found for service %s", targetID, svc.Name)
		}
		target = d
	} else {
		deps, err := store.ListDeployments(ctx, st, svc.Id, 50)
		if err != nil {
			return nil, err
		}
		for _, d := range deps {
			if d.Id == svc.ActiveDeploymentId {
				continue
			}
			if d.Status == v1.DeploymentStatus_DEPLOYMENT_STATUS_SUPERSEDED || d.Status == v1.DeploymentStatus_DEPLOYMENT_STATUS_ACTIVE {
				target = d
				break
			}
		}
		if target == nil {
			return nil, errors.New("no earlier successful deployment to roll back to")
		}
	}
	svc.Spec = proto.Clone(target.Spec).(*v1.ServiceSpec)
	return Start(ctx, st, svc, StartOptions{ImageDigest: target.ImageDigest, PreviousID: svc.ActiveDeploymentId})
}

// ResolveEnv merges spec.env with the service's stored variables (shared
// environment variables first, service variables override); secret values are
// opened with key. Reference syntax (${{svc.VAR}}) is NOT resolved here.
func ResolveEnv(st store.Store, key secrets.Key) func(ctx context.Context, svc *v1.Service, spec *v1.ServiceSpec) (map[string]string, error) {
	return func(ctx context.Context, svc *v1.Service, spec *v1.ServiceSpec) (map[string]string, error) {
		env := map[string]string{}
		for k, v := range spec.Env {
			env[k] = v
		}
		vars, err := store.ListProto[v1.Variable](ctx, st, store.TableVariables, store.Query{Where: map[string]string{"environment_id": svc.EnvironmentId}})
		if err != nil {
			return nil, err
		}
		apply := func(serviceID string) error {
			for _, v := range vars {
				if v.ServiceId != serviceID {
					continue
				}
				val := v.Value
				if v.Secret {
					b, err := key.Open(val)
					if err != nil {
						return fmt.Errorf("variable %s: %w", v.Key, err)
					}
					val = string(b)
				}
				env[v.Key] = val
			}
			return nil
		}
		if err := apply(""); err != nil {
			return nil, err
		}
		if err := apply(svc.Id); err != nil {
			return nil, err
		}
		// Injected service metadata.
		env["AX_SERVICE"] = svc.Name
		env["AX_SERVICE_ID"] = svc.Id
		env["AX_PROJECT_ID"] = svc.ProjectId
		for k := range env {
			if strings.TrimSpace(k) == "" {
				delete(env, k)
			}
		}
		return env, nil
	}
}
