package store

import (
	"context"
	"sort"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
)

// Typed accessors for the core API resources. They keep index columns
// consistent with the Tables definition so callers cannot get it wrong.

func PutMachine(ctx context.Context, s Store, m *v1.Machine) error {
	return PutProto(ctx, s, TableMachines, m.Id, map[string]string{"name": m.Name}, m)
}
func GetMachine(ctx context.Context, s Store, id string) (*v1.Machine, error) {
	return GetProto[v1.Machine](ctx, s, TableMachines, id)
}
func ListMachines(ctx context.Context, s Store) ([]*v1.Machine, error) {
	return ListProto[v1.Machine](ctx, s, TableMachines, Query{})
}

func PutProject(ctx context.Context, s Store, p *v1.Project) error {
	return PutProto(ctx, s, TableProjects, p.Id, map[string]string{"name": p.Name}, p)
}
func PutEnvironment(ctx context.Context, s Store, e *v1.Environment) error {
	return PutProto(ctx, s, TableEnvironments, e.Id, map[string]string{"project_id": e.ProjectId, "name": e.Name}, e)
}

func PutService(ctx context.Context, s Store, svc *v1.Service) error {
	return PutProto(ctx, s, TableServices, svc.Id, map[string]string{
		"project_id": svc.ProjectId, "environment_id": svc.EnvironmentId, "name": svc.Name}, svc)
}
func GetService(ctx context.Context, s Store, id string) (*v1.Service, error) {
	return GetProto[v1.Service](ctx, s, TableServices, id)
}
func ListServices(ctx context.Context, s Store, where map[string]string) ([]*v1.Service, error) {
	return ListProto[v1.Service](ctx, s, TableServices, Query{Where: where})
}

func PutDeployment(ctx context.Context, s Store, d *v1.Deployment) error {
	return PutProto(ctx, s, TableDeployments, d.Id, map[string]string{"service_id": d.ServiceId}, d)
}
func GetDeployment(ctx context.Context, s Store, id string) (*v1.Deployment, error) {
	return GetProto[v1.Deployment](ctx, s, TableDeployments, id)
}
func ListDeployments(ctx context.Context, s Store, serviceID string, limit int) ([]*v1.Deployment, error) {
	// Prefer id DESC at the SQL layer: deployment IDs are ULIDs, so this is
	// roughly creation order and keeps LIMIT meaningful. Do not order by the
	// row's updated_at — marking a prior deployment SUPERSEDED (or any later
	// status write) bumps updated_at and would float old rows above newer ones.
	q := Query{OrderBy: "id", Desc: true, Limit: limit}
	if serviceID != "" {
		q.Where = map[string]string{"service_id": serviceID}
	}
	deps, err := ListProto[v1.Deployment](ctx, s, TableDeployments, q)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(deps, func(i, j int) bool {
		ti, tj := deploymentCreated(deps[i]), deploymentCreated(deps[j])
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return deps[i].Id > deps[j].Id
	})
	return deps, nil
}

func deploymentCreated(d *v1.Deployment) time.Time {
	if d != nil && d.CreatedAt != nil {
		return d.CreatedAt.AsTime()
	}
	return time.Time{}
}

func PutReplica(ctx context.Context, s Store, r *v1.Replica) error {
	return PutProto(ctx, s, TableReplicas, r.Id, map[string]string{
		"service_id": r.ServiceId, "deployment_id": r.DeploymentId, "machine_id": r.MachineId}, r)
}
func GetReplica(ctx context.Context, s Store, id string) (*v1.Replica, error) {
	return GetProto[v1.Replica](ctx, s, TableReplicas, id)
}
func ListReplicas(ctx context.Context, s Store, where map[string]string) ([]*v1.Replica, error) {
	return ListProto[v1.Replica](ctx, s, TableReplicas, Query{Where: where})
}

func PutDomain(ctx context.Context, s Store, d *v1.Domain) error {
	return PutProto(ctx, s, TableDomains, d.Id, map[string]string{"service_id": d.ServiceId, "hostname": d.Hostname}, d)
}
func ListDomains(ctx context.Context, s Store, serviceID string) ([]*v1.Domain, error) {
	q := Query{}
	if serviceID != "" {
		q.Where = map[string]string{"service_id": serviceID}
	}
	return ListProto[v1.Domain](ctx, s, TableDomains, q)
}

func PutCluster(ctx context.Context, s Store, c *v1.Cluster) error {
	return PutProto(ctx, s, TableCluster, "cluster", nil, c)
}
func GetCluster(ctx context.Context, s Store) (*v1.Cluster, error) {
	return GetProto[v1.Cluster](ctx, s, TableCluster, "cluster")
}

func PutEvent(ctx context.Context, s Store, e *v1.Event) error {
	return PutProto(ctx, s, TableEvents, e.Id, map[string]string{"type": e.Type}, e)
}
