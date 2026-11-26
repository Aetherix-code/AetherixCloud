package api

import (
	"context"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/compose"
	"github.com/aetherix/aetherixcloud/internal/deploy"
	"github.com/aetherix/aetherixcloud/internal/store"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type deploySvc struct{ *server }

func (s *deploySvc) start(ctx context.Context, svc *v1.Service, o *deploy.StartOptions) (*v1.Deployment, error) {
	opts := deploy.StartOptions{}
	if o != nil {
		opts = *o
	}
	d, err := deploy.Start(ctx, s.Store, svc, opts)
	if err != nil {
		if err == deploy.ErrNoImage {
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		}
		return nil, internalErr(err)
	}
	s.changed()
	return d, nil
}

func (s *deploySvc) Deploy(ctx context.Context, req *connect.Request[v1.DeployRequest]) (*connect.Response[v1.DeployResponse], error) {
	svc, err := store.GetService(ctx, s.Store, req.Msg.ServiceId)
	if err != nil {
		return nil, errNotFound("service", req.Msg.ServiceId)
	}
	o := &deploy.StartOptions{CommitSHA: req.Msg.CommitSha}
	if req.Msg.SpecOverride != nil {
		o.SpecOverride = NormalizeSpec(req.Msg.SpecOverride)
	}
	// Redeploy with an existing image reuses it. A fresh Deploy (or first
	// deploy with no image) for a GitHub-sourced service goes through build.
	overrideImg := ""
	if o.SpecOverride != nil {
		overrideImg = o.SpecOverride.Image
	}
	gh := svc.Spec.GetSource().GetGithub()
	githubSrc := gh != nil && gh.Repo != ""
	if overrideImg == "" && githubSrc && (svc.Spec.Image == "" || !req.Msg.Redeploy) {
		ref := req.Msg.Ref
		if ref == "" {
			ref = req.Msg.CommitSha
		}
		if _, err := s.BuildTrigger.Enqueue(ctx, svc.Id, ref); err != nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		}
		return connect.NewResponse(&v1.DeployResponse{}), nil
	}
	d, err := s.start(ctx, svc, o)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.DeployResponse{Deployment: d}), nil
}

func (s *deploySvc) DeployCompose(ctx context.Context, req *connect.Request[v1.DeployComposeRequest]) (*connect.Response[v1.DeployComposeResponse], error) {
	proj, err := compose.Parse([]byte(req.Msg.ComposeYaml), req.Msg.Vars)
	if err != nil {
		return nil, errInvalid("%v", err)
	}
	resp := &v1.DeployComposeResponse{Warnings: proj.Warnings}
	want := map[string]bool{}
	for _, n := range req.Msg.Services {
		want[n] = true
	}
	// Build pipeline is owned by the autobuild worker.
	for _, cs := range proj.Services {
		if len(want) > 0 && !want[cs.Name] {
			continue
		}
		if cs.Spec.Image == "" {
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				errorf("service %q has no image (build: sections need the build pipeline, not available yet)", cs.Name))
		}
	}
	pname := req.Msg.Project
	if pname == "" {
		pname = proj.Name
	}
	if pname == "" {
		return nil, errInvalid("project is required (set it in the request or `name:` in the compose file)")
	}
	p, err := s.project(ctx, pname)
	if err != nil {
		if req.Msg.DryRun {
			p = &v1.Project{Id: "(new)", Name: pname}
		} else {
			cr, cerr := (&projectSvc{s.server}).CreateProject(ctx, connect.NewRequest(&v1.CreateProjectRequest{Name: pname}))
			if cerr != nil {
				return nil, cerr
			}
			p = cr.Msg.Project
		}
	}
	envName := req.Msg.Environment
	var env *v1.Environment
	if p.Id == "(new)" {
		env = &v1.Environment{Id: "(new)", Name: "default"}
	} else if env, err = s.environment(ctx, p.Id, envName); err != nil {
		return nil, err
	}

	existing, _ := store.ListServices(ctx, s.Store, map[string]string{"project_id": p.Id, "environment_id": env.Id})
	byName := map[string]*v1.Service{}
	for _, e := range existing {
		byName[e.Name] = e
	}
	seen := map[string]bool{}
	for _, cs := range proj.Services {
		if len(want) > 0 && !want[cs.Name] {
			continue
		}
		seen[cs.Name] = true
		spec := NormalizeSpec(cs.Spec)
		svc := byName[cs.Name]
		exceptID := ""
		if svc != nil {
			exceptID = svc.Id
			// Preserve already-assigned TCP proxy ports across redeploys.
			preserveTCPPublished(svc.Spec, spec)
		}
		if err := AssignTCPPorts(ctx, s.Store, spec, exceptID); err != nil {
			return nil, internalErr(err)
		}
		changed := true
		if svc == nil {
			svc = &v1.Service{Id: newID(), ProjectId: p.Id, EnvironmentId: env.Id, Name: cs.Name, CreatedAt: timestamppb.Now()}
		} else if svc.Spec != nil && proto.Equal(svc.Spec, spec) && svc.ActiveDeploymentId != "" {
			changed = false
		}
		svc.Spec = spec
		svc.UpdatedAt = timestamppb.Now()
		resp.Services = append(resp.Services, svc)
		if req.Msg.DryRun {
			continue
		}
		if err := store.PutService(ctx, s.Store, svc); err != nil {
			return nil, internalErr(err)
		}
		if !changed {
			continue
		}
		d, err := s.start(ctx, svc, nil)
		if err != nil {
			return nil, err
		}
		resp.Deployments = append(resp.Deployments, d)
	}
	if req.Msg.Prune {
		for _, e := range existing {
			if !seen[e.Name] {
				resp.Removed = append(resp.Removed, e.Name)
				if !req.Msg.DryRun {
					if err := (&serviceSvc{s.server}).deleteService(ctx, e); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	return connect.NewResponse(resp), nil
}

func (s *deploySvc) GetDeployment(ctx context.Context, req *connect.Request[v1.GetDeploymentRequest]) (*connect.Response[v1.GetDeploymentResponse], error) {
	d, err := store.GetDeployment(ctx, s.Store, req.Msg.DeploymentId)
	if err != nil {
		return nil, errNotFound("deployment", req.Msg.DeploymentId)
	}
	reps, _ := store.ListReplicas(ctx, s.Store, map[string]string{"deployment_id": d.Id})
	return connect.NewResponse(&v1.GetDeploymentResponse{Deployment: d, Replicas: visibleReplicas(reps)}), nil
}

func (s *deploySvc) ListDeployments(ctx context.Context, req *connect.Request[v1.ListDeploymentsRequest]) (*connect.Response[v1.ListDeploymentsResponse], error) {
	ds, err := store.ListDeployments(ctx, s.Store, req.Msg.ServiceId, int(req.Msg.Limit))
	if err != nil {
		return nil, internalErr(err)
	}
	return connect.NewResponse(&v1.ListDeploymentsResponse{Deployments: ds}), nil
}

func (s *deploySvc) Rollback(ctx context.Context, req *connect.Request[v1.RollbackRequest]) (*connect.Response[v1.RollbackResponse], error) {
	svc, err := store.GetService(ctx, s.Store, req.Msg.ServiceId)
	if err != nil {
		return nil, errNotFound("service", req.Msg.ServiceId)
	}
	d, err := deploy.Rollback(ctx, s.Store, svc, req.Msg.DeploymentId)
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	s.changed()
	return connect.NewResponse(&v1.RollbackResponse{Deployment: d}), nil
}

func (s *deploySvc) CancelDeployment(ctx context.Context, req *connect.Request[v1.CancelDeploymentRequest]) (*connect.Response[v1.CancelDeploymentResponse], error) {
	dep, err := store.GetDeployment(ctx, s.Store, req.Msg.DeploymentId)
	if err != nil {
		return nil, errNotFound("deployment", req.Msg.DeploymentId)
	}
	svc, err := store.GetService(ctx, s.Store, dep.ServiceId)
	if err != nil {
		return nil, errNotFound("service", dep.ServiceId)
	}
	if err := deploy.Cancel(ctx, s.Store, svc, dep.Id); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	s.changed()
	return connect.NewResponse(&v1.CancelDeploymentResponse{}), nil
}

func (s *deploySvc) WatchDeployment(ctx context.Context, req *connect.Request[v1.WatchDeploymentRequest], stream *connect.ServerStream[v1.Deployment]) error {
	var last v1.DeploymentStatus = -1
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		d, err := store.GetDeployment(ctx, s.Store, req.Msg.DeploymentId)
		if err != nil {
			return errNotFound("deployment", req.Msg.DeploymentId)
		}
		if d.Status != last {
			last = d.Status
			if err := stream.Send(d); err != nil {
				return err
			}
		}
		switch d.Status {
		case v1.DeploymentStatus_DEPLOYMENT_STATUS_ACTIVE, v1.DeploymentStatus_DEPLOYMENT_STATUS_FAILED,
			v1.DeploymentStatus_DEPLOYMENT_STATUS_ROLLED_BACK, v1.DeploymentStatus_DEPLOYMENT_STATUS_SUPERSEDED:
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}
