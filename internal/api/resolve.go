package api

import (
	"context"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/store"
)

func (s *server) project(ctx context.Context, ref string) (*v1.Project, error) {
	if p, err := store.GetProto[v1.Project](ctx, s.Store, store.TableProjects, ref); err == nil {
		return p, nil
	}
	ps, err := store.ListProto[v1.Project](ctx, s.Store, store.TableProjects, store.Query{Where: map[string]string{"name": ref}})
	if err != nil {
		return nil, internalErr(err)
	}
	if len(ps) == 0 {
		return nil, errNotFound("project", ref)
	}
	return ps[0], nil
}

func (s *server) environment(ctx context.Context, projectID, ref string) (*v1.Environment, error) {
	if ref != "" {
		if e, err := store.GetProto[v1.Environment](ctx, s.Store, store.TableEnvironments, ref); err == nil && e.ProjectId == projectID {
			return e, nil
		}
		es, err := store.ListProto[v1.Environment](ctx, s.Store, store.TableEnvironments,
			store.Query{Where: map[string]string{"project_id": projectID, "name": ref}})
		if err != nil {
			return nil, internalErr(err)
		}
		if len(es) == 0 {
			return nil, errNotFound("environment", ref)
		}
		return es[0], nil
	}
	// Empty ref: prefer "default", then legacy "production", else any non-ephemeral.
	es, err := store.ListProto[v1.Environment](ctx, s.Store, store.TableEnvironments,
		store.Query{Where: map[string]string{"project_id": projectID}})
	if err != nil {
		return nil, internalErr(err)
	}
	var fallback *v1.Environment
	for _, e := range es {
		if e.Ephemeral {
			continue
		}
		if e.Name == "default" || e.Name == "production" {
			return e, nil
		}
		if fallback == nil {
			fallback = e
		}
	}
	if fallback != nil {
		return fallback, nil
	}
	return nil, errNotFound("environment", projectID)
}

// service resolves an ID, or a name within (project, environment).
func (s *server) service(ctx context.Context, ref, project, env string) (*v1.Service, error) {
	if svc, err := store.GetService(ctx, s.Store, ref); err == nil {
		return svc, nil
	}
	where := map[string]string{"name": ref}
	if project != "" {
		p, err := s.project(ctx, project)
		if err != nil {
			return nil, err
		}
		where["project_id"] = p.Id
		e, err := s.environment(ctx, p.Id, env)
		if err != nil {
			return nil, err
		}
		where["environment_id"] = e.Id
	}
	svcs, err := store.ListServices(ctx, s.Store, where)
	if err != nil {
		return nil, internalErr(err)
	}
	switch len(svcs) {
	case 0:
		return nil, errNotFound("service", ref)
	case 1:
		return svcs[0], nil
	}
	return nil, errInvalid("service name %q is ambiguous; specify project and environment or use the ID", ref)
}
