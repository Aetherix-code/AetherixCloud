package api

import (
	"context"

	"connectrpc.com/connect"
	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type projectSvc struct{ *server }

func (s *projectSvc) CreateProject(ctx context.Context, req *connect.Request[v1.CreateProjectRequest]) (*connect.Response[v1.CreateProjectResponse], error) {
	if req.Msg.Name == "" {
		return nil, errInvalid("name is required")
	}
	if ps, _ := store.ListProto[v1.Project](ctx, s.Store, store.TableProjects, store.Query{Where: map[string]string{"name": req.Msg.Name}}); len(ps) > 0 {
		return nil, connect.NewError(connect.CodeAlreadyExists, errorf("project %q exists", req.Msg.Name))
	}
	now := timestamppb.Now()
	p := &v1.Project{Id: newID(), Name: req.Msg.Name, Description: req.Msg.Description, CreatedAt: now}
	e := &v1.Environment{Id: newID(), ProjectId: p.Id, Name: "default", CreatedAt: now}
	if err := store.PutProject(ctx, s.Store, p); err != nil {
		return nil, internalErr(err)
	}
	if err := store.PutEnvironment(ctx, s.Store, e); err != nil {
		return nil, internalErr(err)
	}
	return connect.NewResponse(&v1.CreateProjectResponse{Project: p, Environment: e}), nil
}

func (s *projectSvc) GetProject(ctx context.Context, req *connect.Request[v1.GetProjectRequest]) (*connect.Response[v1.GetProjectResponse], error) {
	p, err := s.project(ctx, req.Msg.Project)
	if err != nil {
		return nil, err
	}
	envs, _ := store.ListProto[v1.Environment](ctx, s.Store, store.TableEnvironments, store.Query{Where: map[string]string{"project_id": p.Id}})
	svcs, _ := store.ListServices(ctx, s.Store, map[string]string{"project_id": p.Id})
	return connect.NewResponse(&v1.GetProjectResponse{Project: p, Environments: envs, Services: svcs}), nil
}

func (s *projectSvc) ListProjects(ctx context.Context, _ *connect.Request[v1.ListProjectsRequest]) (*connect.Response[v1.ListProjectsResponse], error) {
	ps, err := store.ListProto[v1.Project](ctx, s.Store, store.TableProjects, store.Query{})
	if err != nil {
		return nil, internalErr(err)
	}
	return connect.NewResponse(&v1.ListProjectsResponse{Projects: ps}), nil
}

func (s *projectSvc) DeleteProject(ctx context.Context, req *connect.Request[v1.DeleteProjectRequest]) (*connect.Response[v1.DeleteProjectResponse], error) {
	p, err := s.project(ctx, req.Msg.Project)
	if err != nil {
		return nil, err
	}
	svcs, _ := store.ListServices(ctx, s.Store, map[string]string{"project_id": p.Id})
	if len(svcs) > 0 && !req.Msg.Confirm {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errorf("project %s has %d services; set confirm to delete them", p.Name, len(svcs)))
	}
	ss := &serviceSvc{s.server}
	for _, svc := range svcs {
		if err := ss.deleteService(ctx, svc); err != nil {
			return nil, err
		}
	}
	envs, _ := store.ListProto[v1.Environment](ctx, s.Store, store.TableEnvironments, store.Query{Where: map[string]string{"project_id": p.Id}})
	for _, e := range envs {
		_ = s.deleteEnv(ctx, e.Id)
	}
	if err := s.Store.Delete(ctx, store.TableProjects, p.Id); err != nil {
		return nil, internalErr(err)
	}
	return connect.NewResponse(&v1.DeleteProjectResponse{}), nil
}

func (s *projectSvc) deleteEnv(ctx context.Context, id string) error {
	vars, _ := store.ListProto[v1.Variable](ctx, s.Store, store.TableVariables, store.Query{Where: map[string]string{"environment_id": id}})
	for _, v := range vars {
		_ = s.Store.Delete(ctx, store.TableVariables, v.Id)
	}
	return s.Store.Delete(ctx, store.TableEnvironments, id)
}

func (s *projectSvc) CreateEnvironment(ctx context.Context, req *connect.Request[v1.CreateEnvironmentRequest]) (*connect.Response[v1.CreateEnvironmentResponse], error) {
	p, err := s.project(ctx, req.Msg.Project)
	if err != nil {
		return nil, err
	}
	if req.Msg.Name == "" {
		return nil, errInvalid("name is required")
	}
	if es, _ := store.ListProto[v1.Environment](ctx, s.Store, store.TableEnvironments,
		store.Query{Where: map[string]string{"project_id": p.Id, "name": req.Msg.Name}}); len(es) > 0 {
		return nil, connect.NewError(connect.CodeAlreadyExists, errorf("environment %q exists", req.Msg.Name))
	}
	e := &v1.Environment{Id: newID(), ProjectId: p.Id, Name: req.Msg.Name, Ephemeral: req.Msg.Ephemeral, CreatedAt: timestamppb.Now()}
	if err := store.PutEnvironment(ctx, s.Store, e); err != nil {
		return nil, internalErr(err)
	}
	if req.Msg.CloneFrom != "" {
		src, err := store.GetProto[v1.Environment](ctx, s.Store, store.TableEnvironments, req.Msg.CloneFrom)
		if err != nil || src.ProjectId != p.Id {
			return nil, errNotFound("environment", req.Msg.CloneFrom)
		}
		vars, _ := store.ListProto[v1.Variable](ctx, s.Store, store.TableVariables, store.Query{Where: map[string]string{"environment_id": src.Id}})
		idMap := map[string]string{}
		svcs, _ := store.ListServices(ctx, s.Store, map[string]string{"environment_id": src.Id})
		for _, sv := range svcs {
			nid := newID()
			idMap[sv.Id] = nid
			if err := store.PutService(ctx, s.Store, &v1.Service{Id: nid, ProjectId: sv.ProjectId, EnvironmentId: e.Id,
				Name: sv.Name, Spec: sv.Spec, CreatedAt: timestamppb.Now()}); err != nil {
				return nil, internalErr(err)
			}
		}
		for _, v := range vars {
			nv := &v1.Variable{Id: newID(), EnvironmentId: e.Id, ServiceId: idMap[v.ServiceId], Key: v.Key, Value: v.Value, Secret: v.Secret}
			if err := s.putVar(ctx, nv); err != nil {
				return nil, internalErr(err)
			}
		}
	}
	return connect.NewResponse(&v1.CreateEnvironmentResponse{Environment: e}), nil
}

func (s *projectSvc) ListEnvironments(ctx context.Context, req *connect.Request[v1.ListEnvironmentsRequest]) (*connect.Response[v1.ListEnvironmentsResponse], error) {
	p, err := s.project(ctx, req.Msg.Project)
	if err != nil {
		return nil, err
	}
	es, err := store.ListProto[v1.Environment](ctx, s.Store, store.TableEnvironments, store.Query{Where: map[string]string{"project_id": p.Id}})
	if err != nil {
		return nil, internalErr(err)
	}
	return connect.NewResponse(&v1.ListEnvironmentsResponse{Environments: es}), nil
}

func (s *projectSvc) DeleteEnvironment(ctx context.Context, req *connect.Request[v1.DeleteEnvironmentRequest]) (*connect.Response[v1.DeleteEnvironmentResponse], error) {
	svcs, _ := store.ListServices(ctx, s.Store, map[string]string{"environment_id": req.Msg.EnvironmentId})
	ss := &serviceSvc{s.server}
	for _, svc := range svcs {
		if err := ss.deleteService(ctx, svc); err != nil {
			return nil, err
		}
	}
	if err := s.deleteEnv(ctx, req.Msg.EnvironmentId); err != nil {
		return nil, internalErr(err)
	}
	return connect.NewResponse(&v1.DeleteEnvironmentResponse{}), nil
}

func (s *projectSvc) putVar(ctx context.Context, v *v1.Variable) error {
	return store.PutProto(ctx, s.Store, store.TableVariables, v.Id,
		map[string]string{"environment_id": v.EnvironmentId, "service_id": v.ServiceId, "key": v.Key}, v)
}

func redact(v *v1.Variable) *v1.Variable {
	out := &v1.Variable{Id: v.Id, EnvironmentId: v.EnvironmentId, ServiceId: v.ServiceId, Key: v.Key, Secret: v.Secret, Value: v.Value}
	if v.Secret {
		out.Value, out.Redacted = "", true
	}
	return out
}

func (s *projectSvc) ListVariables(ctx context.Context, req *connect.Request[v1.ListVariablesRequest]) (*connect.Response[v1.ListVariablesResponse], error) {
	vars, err := store.ListProto[v1.Variable](ctx, s.Store, store.TableVariables, store.Query{Where: map[string]string{"environment_id": req.Msg.EnvironmentId}})
	if err != nil {
		return nil, internalErr(err)
	}
	var out []*v1.Variable
	for _, v := range vars {
		if req.Msg.ServiceId == "*" || v.ServiceId == req.Msg.ServiceId {
			out = append(out, redact(v))
		}
	}
	return connect.NewResponse(&v1.ListVariablesResponse{Variables: out}), nil
}

func (s *projectSvc) SetVariables(ctx context.Context, req *connect.Request[v1.SetVariablesRequest]) (*connect.Response[v1.SetVariablesResponse], error) {
	existing, _ := store.ListProto[v1.Variable](ctx, s.Store, store.TableVariables, store.Query{Where: map[string]string{"environment_id": req.Msg.EnvironmentId}})
	byKey := map[string]*v1.Variable{}
	for _, v := range existing {
		if v.ServiceId == req.Msg.ServiceId {
			byKey[v.Key] = v
		}
	}
	var out []*v1.Variable
	for _, in := range req.Msg.Variables {
		if in.Key == "" {
			return nil, errInvalid("variable key is required")
		}
		v := byKey[in.Key]
		if v == nil {
			v = &v1.Variable{Id: newID(), EnvironmentId: req.Msg.EnvironmentId, ServiceId: req.Msg.ServiceId, Key: in.Key}
		}
		v.Secret = in.Secret
		v.Value = in.Value
		if in.Secret {
			sealed, err := s.key.Seal([]byte(in.Value))
			if err != nil {
				return nil, internalErr(err)
			}
			v.Value = sealed
		}
		if err := s.putVar(ctx, v); err != nil {
			return nil, internalErr(err)
		}
		out = append(out, redact(v))
	}
	if req.Msg.Redeploy {
		svcs, _ := store.ListServices(ctx, s.Store, map[string]string{"environment_id": req.Msg.EnvironmentId})
		ds := &deploySvc{s.server}
		for _, svc := range svcs {
			if req.Msg.ServiceId == "" || svc.Id == req.Msg.ServiceId {
				if svc.ActiveDeploymentId != "" {
					if _, err := ds.start(ctx, svc, nil); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	return connect.NewResponse(&v1.SetVariablesResponse{Variables: out}), nil
}

func (s *projectSvc) DeleteVariable(ctx context.Context, req *connect.Request[v1.DeleteVariableRequest]) (*connect.Response[v1.DeleteVariableResponse], error) {
	vars, _ := store.ListProto[v1.Variable](ctx, s.Store, store.TableVariables, store.Query{Where: map[string]string{"environment_id": req.Msg.EnvironmentId, "key": req.Msg.Key}})
	for _, v := range vars {
		if v.ServiceId == req.Msg.ServiceId {
			_ = s.Store.Delete(ctx, store.TableVariables, v.Id)
		}
	}
	return connect.NewResponse(&v1.DeleteVariableResponse{}), nil
}
