package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/auth"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type ref struct {
	Service     string `json:"service" jsonschema:"service ID, or service name (then also give project)"`
	Project     string `json:"project,omitempty" jsonschema:"project name or ID (needed when service is a name)"`
	Environment string `json:"environment,omitempty" jsonschema:"environment name or ID; default production"`
}

func (c *call) service(ctx context.Context, r ref) (*v1.Service, error) {
	if r.Service == "" {
		return nil, errors.New("service is required")
	}
	resp, err := c.api.Service.GetService(ctx, connect.NewRequest(&v1.GetServiceRequest{Service: r.Service, Project: r.Project, Environment: r.Environment}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Service, nil
}

// environmentID resolves (project, environment) to an environment ID.
func (c *call) environmentID(ctx context.Context, project, env string) (string, error) {
	if project == "" {
		return "", errors.New("project is required")
	}
	resp, err := c.api.Project.GetProject(ctx, connect.NewRequest(&v1.GetProjectRequest{Project: project}))
	if err != nil {
		return "", err
	}
	if env == "" {
		env = "production"
	}
	for _, e := range resp.Msg.Environments {
		if e.Id == env || e.Name == env {
			return e.Id, nil
		}
	}
	return "", fmt.Errorf("environment %q not found in project %q", env, project)
}

type empty struct{}

func (s *Server) registerReadTools() {
	register(s, "list_projects", "List all projects.", auth.LevelRead, ro("List projects"),
		func(ctx context.Context, c *call, _ empty) (any, error) {
			r, err := c.api.Project.ListProjects(ctx, connect.NewRequest(&v1.ListProjectsRequest{}))
			if err != nil {
				return nil, err
			}
			return map[string]any{"projects": listAny(r.Msg.Projects)}, nil
		})

	type listServicesIn struct {
		Project     string `json:"project,omitempty" jsonschema:"filter by project name or ID"`
		Environment string `json:"environment,omitempty" jsonschema:"filter by environment name or ID"`
	}
	register(s, "list_services", "List services, optionally filtered by project/environment. Environment variables of the spec are redacted.", auth.LevelRead, ro("List services"),
		func(ctx context.Context, c *call, in listServicesIn) (any, error) {
			r, err := c.api.Service.ListServices(ctx, connect.NewRequest(&v1.ListServicesRequest{Project: in.Project, Environment: in.Environment}))
			if err != nil {
				return nil, err
			}
			out := make([]any, 0, len(r.Msg.Services))
			for _, sv := range r.Msg.Services {
				out = append(out, toAny(redactedService(sv)))
			}
			return map[string]any{"services": out}, nil
		})

	register(s, "get_service", "Get one service with its replicas and domains.", auth.LevelRead, ro("Get service"),
		func(ctx context.Context, c *call, in ref) (any, error) {
			r, err := c.api.Service.GetService(ctx, connect.NewRequest(&v1.GetServiceRequest{Service: in.Service, Project: in.Project, Environment: in.Environment}))
			if err != nil {
				return nil, err
			}
			return map[string]any{"service": toAny(redactedService(r.Msg.Service)), "replicas": listAny(r.Msg.Replicas), "domains": listAny(r.Msg.Domains)}, nil
		})

	type listDeploymentsIn struct {
		ref
		Limit int `json:"limit,omitempty" jsonschema:"maximum number of deployments (default 20)"`
	}
	register(s, "list_deployments", "List the deployment history of a service, newest first.", auth.LevelRead, ro("List deployments"),
		func(ctx context.Context, c *call, in listDeploymentsIn) (any, error) {
			svc, err := c.service(ctx, in.ref)
			if err != nil {
				return nil, err
			}
			r, err := c.api.Deploy.ListDeployments(ctx, connect.NewRequest(&v1.ListDeploymentsRequest{ServiceId: svc.Id}))
			if err != nil {
				return nil, err
			}
			limit := in.Limit
			if limit <= 0 {
				limit = 20
			}
			out := []any{}
			for _, d := range r.Msg.Deployments {
				if len(out) == limit {
					break
				}
				out = append(out, toAny(redactedDeployment(d)))
			}
			return map[string]any{"service_id": svc.Id, "deployments": out}, nil
		})

	type deploymentIn struct {
		DeploymentID string `json:"deployment_id" jsonschema:"deployment ID"`
	}
	register(s, "get_deployment", "Get a deployment and its replicas.", auth.LevelRead, ro("Get deployment"),
		func(ctx context.Context, c *call, in deploymentIn) (any, error) {
			r, err := c.api.Deploy.GetDeployment(ctx, connect.NewRequest(&v1.GetDeploymentRequest{DeploymentId: in.DeploymentID}))
			if err != nil {
				return nil, err
			}
			return map[string]any{"deployment": toAny(redactedDeployment(r.Msg.Deployment)), "replicas": listAny(r.Msg.Replicas)}, nil
		})

	type logsIn struct {
		ref
		ReplicaID    string `json:"replica_id,omitempty" jsonschema:"only this replica"`
		Tail         int    `json:"tail,omitempty" jsonschema:"number of most recent lines (default 100, max 1000)"`
		SinceSeconds int    `json:"since_seconds,omitempty" jsonschema:"only lines newer than this many seconds"`
	}
	register(s, "get_logs", "Fetch recent container logs of a service (all replicas merged). Credentials in log lines are redacted.", auth.LevelRead, ro("Get logs"),
		func(ctx context.Context, c *call, in logsIn) (any, error) {
			svc, err := c.service(ctx, in.ref)
			if err != nil {
				return nil, err
			}
			return c.logs(ctx, svc.Id, in.ReplicaID, in.Tail, in.SinceSeconds)
		})

	type buildLogsIn struct {
		BuildID string `json:"build_id" jsonschema:"build ID (see get_deployment.build_id)"`
		Tail    int    `json:"tail,omitempty" jsonschema:"number of most recent lines (default 200, max 2000)"`
	}
	register(s, "get_build_logs", "Fetch the output of a build. Credentials are redacted.", auth.LevelRead, ro("Get build logs"),
		func(ctx context.Context, c *call, in buildLogsIn) (any, error) {
			st, err := c.api.Log.StreamBuildLogs(ctx, connect.NewRequest(&v1.StreamBuildLogsRequest{BuildId: in.BuildID}))
			if err != nil {
				return nil, err
			}
			defer st.Close()
			tail := clamp(in.Tail, 200, 2000)
			var lines []string
			for st.Receive() {
				lines = append(lines, RedactText(st.Msg().Line))
				if len(lines) > tail {
					lines = lines[1:]
				}
			}
			if err := st.Err(); err != nil {
				return nil, err
			}
			return map[string]any{"build_id": in.BuildID, "lines": lines}, nil
		})

	type metricsIn struct {
		Service     string `json:"service,omitempty" jsonschema:"service ID or name; empty = whole cluster"`
		Project     string `json:"project,omitempty"`
		Environment string `json:"environment,omitempty"`
	}
	register(s, "get_metrics", "Resource view of a service or the cluster: replica states and machine capacity/usage.", auth.LevelRead, ro("Get metrics"),
		func(ctx context.Context, c *call, in metricsIn) (any, error) {
			svcID := ""
			if in.Service != "" {
				svc, err := c.service(ctx, ref{in.Service, in.Project, in.Environment})
				if err != nil {
					return nil, err
				}
				svcID = svc.Id
			}
			rr, err := c.api.Service.ListReplicas(ctx, connect.NewRequest(&v1.ListReplicasRequest{ServiceId: svcID}))
			if err != nil {
				return nil, err
			}
			mm, err := c.api.Machine.ListMachines(ctx, connect.NewRequest(&v1.ListMachinesRequest{}))
			if err != nil {
				return nil, err
			}
			states := map[string]int{}
			for _, r := range rr.Msg.Replicas {
				states[r.State.String()]++
			}
			machines := []any{}
			for _, m := range mm.Msg.Machines {
				machines = append(machines, map[string]any{"id": m.Id, "name": m.Name, "state": m.State.String(),
					"capacity": toAny(m.Capacity), "used": toAny(m.Used)})
			}
			return map[string]any{"replica_states": states, "replicas": listAny(rr.Msg.Replicas), "machines": machines,
				"note": "time series live at GET /api/metrics?service_id=… on each node; this tool returns the replica/machine resource view"}, nil
		})

	register(s, "list_machines", "List cluster machines with state, roles and capacity.", auth.LevelRead, ro("List machines"),
		func(ctx context.Context, c *call, _ empty) (any, error) {
			r, err := c.api.Machine.ListMachines(ctx, connect.NewRequest(&v1.ListMachinesRequest{}))
			if err != nil {
				return nil, err
			}
			return map[string]any{"machines": listAny(r.Msg.Machines)}, nil
		})

	register(s, "get_cluster_status", "Cluster health summary: machines up/down, services, replicas.", auth.LevelRead, ro("Cluster status"),
		func(ctx context.Context, c *call, _ empty) (any, error) {
			r, err := c.api.Cluster.GetStatus(ctx, connect.NewRequest(&v1.GetStatusRequest{}))
			if err != nil {
				return nil, err
			}
			cl := toAny(r.Msg).(map[string]any)
			if cluster, ok := cl["cluster"].(map[string]any); ok {
				delete(cluster, "ca_pem")
			}
			return cl, nil
		})

	type eventsIn struct {
		TypePrefix string `json:"type_prefix,omitempty" jsonschema:"e.g. deployment. or machine."`
		Limit      int    `json:"limit,omitempty" jsonschema:"default 50"`
	}
	register(s, "list_events", "Recent cluster events (deployments, machine state changes, ...), newest first.", auth.LevelRead, ro("List events"),
		func(ctx context.Context, c *call, in eventsIn) (any, error) {
			r, err := c.api.Cluster.ListEvents(ctx, connect.NewRequest(&v1.ListEventsRequest{TypePrefix: in.TypePrefix, Limit: int32(clamp(in.Limit, 50, 500))}))
			if err != nil {
				return nil, err
			}
			return map[string]any{"events": listAny(r.Msg.Events)}, nil
		})

	register(s, "list_domains", "List the domains routed to a service.", auth.LevelRead, ro("List domains"),
		func(ctx context.Context, c *call, in ref) (any, error) {
			svc, err := c.service(ctx, in)
			if err != nil {
				return nil, err
			}
			r, err := c.api.Service.ListDomains(ctx, connect.NewRequest(&v1.ListDomainsRequest{ServiceId: svc.Id}))
			if err != nil {
				return nil, err
			}
			return map[string]any{"service_id": svc.Id, "domains": listAny(r.Msg.Domains)}, nil
		})

	type listVarsIn struct {
		Project     string `json:"project" jsonschema:"project name or ID"`
		Environment string `json:"environment,omitempty" jsonschema:"default production"`
		Service     string `json:"service,omitempty" jsonschema:"service name or ID; empty = shared variables only, * = all"`
	}
	register(s, "list_variables", "List environment variables. Secret values are always redacted.", auth.LevelRead, ro("List variables"),
		func(ctx context.Context, c *call, in listVarsIn) (any, error) {
			envID, err := c.environmentID(ctx, in.Project, in.Environment)
			if err != nil {
				return nil, err
			}
			sid := in.Service
			if sid != "" && sid != "*" {
				svc, err := c.service(ctx, ref{in.Service, in.Project, in.Environment})
				if err != nil {
					return nil, err
				}
				sid = svc.Id
			}
			r, err := c.api.Project.ListVariables(ctx, connect.NewRequest(&v1.ListVariablesRequest{EnvironmentId: envID, ServiceId: sid}))
			if err != nil {
				return nil, err
			}
			out := make([]any, 0, len(r.Msg.Variables))
			for _, v := range r.Msg.Variables {
				out = append(out, toAny(redactedVariable(v)))
			}
			return map[string]any{"environment_id": envID, "variables": out}, nil
		})
}

func clamp(v, def, max int) int {
	if v <= 0 {
		return def
	}
	return min(v, max)
}

// logs collects non-following logs.
func (c *call) logs(ctx context.Context, serviceID, replicaID string, tail, sinceSec int) (map[string]any, error) {
	req := &v1.StreamLogsRequest{ServiceId: serviceID, ReplicaId: replicaID, Tail: int32(clamp(tail, 100, 1000)), Timestamps: true}
	if sinceSec > 0 {
		req.Since = timestamppb.New(time.Now().Add(-time.Duration(sinceSec) * time.Second))
	}
	st, err := c.api.Log.StreamLogs(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	defer st.Close()
	limit := int(req.Tail)
	var lines []string
	for st.Receive() {
		e := st.Msg()
		lines = append(lines, RedactText(strings.TrimRight(e.Line, "\r\n")))
		if len(lines) > limit {
			lines = lines[1:]
		}
	}
	if err := st.Err(); err != nil {
		return nil, err
	}
	return map[string]any{"service_id": serviceID, "lines": lines}, nil
}
