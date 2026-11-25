package mcp

import (
	"context"
	"errors"
	"sort"

	"connectrpc.com/connect"
	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/auth"
	"github.com/aetherix/aetherixcloud/internal/templates"
)

func (s *Server) registerDeployTools() {
	type createServiceIn struct {
		Project      string            `json:"project" jsonschema:"project name or ID"`
		Environment  string            `json:"environment,omitempty" jsonschema:"environment name or ID; default production"`
		Name         string            `json:"name" jsonschema:"service name"`
		Image        string            `json:"image,omitempty" jsonschema:"container image, e.g. nginx:1.27"`
		GithubRepo   string            `json:"github_repo,omitempty" jsonschema:"GitHub repo owner/name to build and deploy on push"`
		GithubBranch string            `json:"github_branch,omitempty" jsonschema:"branch to track (default main)"`
		Template     string            `json:"template,omitempty" jsonschema:"one of postgres, mysql, redis, minio (instead of image/github_repo)"`
		Replicas     int               `json:"replicas,omitempty" jsonschema:"number of replicas (default 1)"`
		Port         int               `json:"port,omitempty" jsonschema:"container port to expose over HTTPS"`
		Env          map[string]string `json:"env,omitempty" jsonschema:"plain (non-secret) environment variables; use set_variables for secrets"`
		Deploy       *bool             `json:"deploy,omitempty" jsonschema:"deploy right away (default true)"`
	}
	register(s, "create_service", "Create a service from a Docker image, a GitHub repo or a template (postgres, mysql, redis, minio) and optionally deploy it.",
		auth.LevelDeploy, write("Create service", false),
		func(ctx context.Context, c *call, in createServiceIn) (any, error) {
			spec := &v1.ServiceSpec{Mode: v1.ServiceMode_SERVICE_MODE_REPLICATED, Replicas: int32(max(in.Replicas, 1)), Env: in.Env}
			var secretKeys []string
			switch {
			case in.Template != "":
				t, ok := templates.Get(in.Template)
				if !ok {
					return nil, templates.ErrUnknown{ID: in.Template}
				}
				spec = templates.Spec(t, in.Name)
				spec.Replicas = int32(max(in.Replicas, 1))
				for k, v := range in.Env {
					if spec.Env == nil {
						spec.Env = map[string]string{}
					}
					spec.Env[k] = v
				}
				secretKeys = t.Secrets
			case in.Image != "":
				spec.Source = &v1.ServiceSource{Image: in.Image}
				spec.Image = in.Image
			case in.GithubRepo != "":
				br := in.GithubBranch
				if br == "" {
					br = "main"
				}
				spec.Source = &v1.ServiceSource{Github: &v1.GitHubSource{Repo: in.GithubRepo, Branch: br}}
			default:
				return nil, errors.New("one of image, github_repo or template is required")
			}
			if in.Port > 0 && in.Template == "" {
				spec.Ports = []*v1.ServicePort{{ContainerPort: int32(in.Port), Protocol: v1.PortProtocol_PORT_PROTOCOL_TCP, Mode: "https"}}
			}
			doDeploy := in.Deploy == nil || *in.Deploy
			r, err := c.api.Service.CreateService(ctx, connect.NewRequest(&v1.CreateServiceRequest{
				Project: in.Project, Environment: in.Environment, Name: in.Name, Spec: spec, Deploy: doDeploy && len(secretKeys) == 0}))
			if err != nil {
				return nil, err
			}
			out := map[string]any{"service": toAny(redactedService(r.Msg.Service))}
			if len(secretKeys) > 0 {
				// generated credentials are stored as secret variables and never shown
				envID, err := c.environmentID(ctx, in.Project, in.Environment)
				if err != nil {
					return nil, err
				}
				var vars []*v1.Variable
				for _, k := range secretKeys {
					vars = append(vars, &v1.Variable{Key: k, Value: templates.RandomSecret(), Secret: true})
				}
				if _, err := c.api.Project.SetVariables(ctx, connect.NewRequest(&v1.SetVariablesRequest{EnvironmentId: envID, ServiceId: r.Msg.Service.Id, Variables: vars})); err != nil {
					return nil, err
				}
				out["generated_secret_variables"] = secretKeys
				if doDeploy {
					d, err := c.api.Deploy.Deploy(ctx, connect.NewRequest(&v1.DeployRequest{ServiceId: r.Msg.Service.Id}))
					if err != nil {
						return nil, err
					}
					out["deployment"] = toAny(redactedDeployment(d.Msg.Deployment))
				}
			} else if r.Msg.Deployment != nil {
				out["deployment"] = toAny(redactedDeployment(r.Msg.Deployment))
			}
			return out, nil
		})

	type deployIn struct {
		ref
		CommitSHA string `json:"commit_sha,omitempty" jsonschema:"commit to build (GitHub services)"`
		Ref       string `json:"ref,omitempty" jsonschema:"git ref/branch (GitHub services)"`
	}
	register(s, "deploy_service", "Deploy the service's current spec (builds first for GitHub sources).", auth.LevelDeploy, write("Deploy service", false),
		func(ctx context.Context, c *call, in deployIn) (any, error) {
			svc, err := c.service(ctx, in.ref)
			if err != nil {
				return nil, err
			}
			r, err := c.api.Deploy.Deploy(ctx, connect.NewRequest(&v1.DeployRequest{ServiceId: svc.Id, CommitSha: in.CommitSHA, Ref: in.Ref}))
			if err != nil {
				return nil, err
			}
			return map[string]any{"deployment": toAny(redactedDeployment(r.Msg.Deployment))}, nil
		})

	register(s, "redeploy", "Redeploy the current image without rebuilding (picks up changed variables).", auth.LevelDeploy, write("Redeploy", false),
		func(ctx context.Context, c *call, in ref) (any, error) {
			svc, err := c.service(ctx, in)
			if err != nil {
				return nil, err
			}
			r, err := c.api.Deploy.Deploy(ctx, connect.NewRequest(&v1.DeployRequest{ServiceId: svc.Id, Redeploy: true}))
			if err != nil {
				return nil, err
			}
			return map[string]any{"deployment": toAny(redactedDeployment(r.Msg.Deployment))}, nil
		})

	type rollbackIn struct {
		ref
		DeploymentID string `json:"deployment_id,omitempty" jsonschema:"deployment to roll back to; default the previous one"`
	}
	register(s, "rollback", "Roll a service back to a previous deployment (redeploys its spec and image digest).", auth.LevelDeploy, write("Rollback", false),
		func(ctx context.Context, c *call, in rollbackIn) (any, error) {
			svc, err := c.service(ctx, in.ref)
			if err != nil {
				return nil, err
			}
			r, err := c.api.Deploy.Rollback(ctx, connect.NewRequest(&v1.RollbackRequest{ServiceId: svc.Id, DeploymentId: in.DeploymentID}))
			if err != nil {
				return nil, err
			}
			return map[string]any{"deployment": toAny(redactedDeployment(r.Msg.Deployment))}, nil
		})

	type scaleIn struct {
		ref
		Replicas int `json:"replicas" jsonschema:"desired replica count; 0 stops the service and needs confirmation"`
		Confirmation
	}
	register(s, "scale_service", "Set the replica count of a service. Scaling to 0 is destructive and needs confirm=true.", auth.LevelDeploy, destructive("Scale service"),
		func(ctx context.Context, c *call, in scaleIn) (any, error) {
			svc, err := c.service(ctx, in.ref)
			if err != nil {
				return nil, err
			}
			if in.Replicas < 0 {
				return nil, errors.New("replicas must be >= 0")
			}
			if in.Replicas == 0 {
				if dry, err := c.gate("scale_service", svc.Id+"=0", in.Confirmation, func() (map[string]any, error) {
					rr, _ := c.api.Service.ListReplicas(ctx, connect.NewRequest(&v1.ListReplicasRequest{ServiceId: svc.Id}))
					n := 0
					if rr != nil {
						for _, r := range rr.Msg.Replicas {
							if r.DesiredRunning {
								n++
							}
						}
					}
					return map[string]any{"action": "scale to zero", "service": svc.Name, "replicas_to_stop": n,
						"effect": "the service stops serving traffic until it is scaled up again"}, nil
				}); err != nil || dry != nil {
					return dry, err
				}
			}
			r, err := c.api.Service.ScaleService(ctx, connect.NewRequest(&v1.ScaleServiceRequest{ServiceId: svc.Id, Replicas: int32(in.Replicas)}))
			if err != nil {
				return nil, err
			}
			return map[string]any{"executed": true, "service": toAny(redactedService(r.Msg.Service))}, nil
		})

	type setVarsIn struct {
		Project     string            `json:"project" jsonschema:"project name or ID"`
		Environment string            `json:"environment,omitempty" jsonschema:"default production"`
		Service     string            `json:"service,omitempty" jsonschema:"service name or ID; empty = shared (environment-wide) variables"`
		Variables   map[string]string `json:"variables,omitempty" jsonschema:"plain variables KEY=value"`
		Secrets     map[string]string `json:"secrets,omitempty" jsonschema:"secret variables KEY=value; stored encrypted and never shown again"`
		Redeploy    bool              `json:"redeploy,omitempty" jsonschema:"redeploy the service(s) so the change takes effect"`
	}
	register(s, "set_variables", "Create or update environment variables (plain and secret). Values are never echoed back.", auth.LevelDeploy, write("Set variables", true),
		func(ctx context.Context, c *call, in setVarsIn) (any, error) {
			if len(in.Variables)+len(in.Secrets) == 0 {
				return nil, errors.New("variables or secrets required")
			}
			envID, err := c.environmentID(ctx, in.Project, in.Environment)
			if err != nil {
				return nil, err
			}
			sid := ""
			if in.Service != "" {
				svc, err := c.service(ctx, ref{in.Service, in.Project, in.Environment})
				if err != nil {
					return nil, err
				}
				sid = svc.Id
			}
			var vars []*v1.Variable
			for k, v := range in.Variables {
				vars = append(vars, &v1.Variable{Key: k, Value: v})
			}
			for k, v := range in.Secrets {
				vars = append(vars, &v1.Variable{Key: k, Value: v, Secret: true})
			}
			sort.Slice(vars, func(i, j int) bool { return vars[i].Key < vars[j].Key })
			r, err := c.api.Project.SetVariables(ctx, connect.NewRequest(&v1.SetVariablesRequest{EnvironmentId: envID, ServiceId: sid, Variables: vars, Redeploy: in.Redeploy}))
			if err != nil {
				return nil, err
			}
			set := make([]any, 0, len(r.Msg.Variables))
			for _, v := range r.Msg.Variables {
				set = append(set, toAny(redactedVariable(v)))
			}
			return map[string]any{"set": set, "redeploy": in.Redeploy}, nil
		})

	type addDomainIn struct {
		ref
		Hostname string `json:"hostname" jsonschema:"fully qualified domain, e.g. app.example.com"`
	}
	register(s, "add_domain", "Route a custom domain to a service (TLS certificates are issued automatically).", auth.LevelDeploy, write("Add domain", true),
		func(ctx context.Context, c *call, in addDomainIn) (any, error) {
			svc, err := c.service(ctx, in.ref)
			if err != nil {
				return nil, err
			}
			r, err := c.api.Service.AddDomain(ctx, connect.NewRequest(&v1.AddDomainRequest{ServiceId: svc.Id, Hostname: in.Hostname}))
			if err != nil {
				return nil, err
			}
			return map[string]any{"domain": toAny(r.Msg.Domain), "dns_hints": r.Msg.DnsHints}, nil
		})

	register(s, "restart_service", "Restart all replicas of a service.", auth.LevelDeploy, write("Restart service", false),
		func(ctx context.Context, c *call, in ref) (any, error) {
			svc, err := c.service(ctx, in)
			if err != nil {
				return nil, err
			}
			if _, err := c.api.Service.RestartService(ctx, connect.NewRequest(&v1.RestartServiceRequest{ServiceId: svc.Id})); err != nil {
				return nil, err
			}
			return map[string]any{"restarted": svc.Name}, nil
		})

	type composeIn struct {
		Project     string   `json:"project" jsonschema:"project name or ID"`
		Environment string   `json:"environment,omitempty" jsonschema:"default production"`
		ComposeYAML string   `json:"compose_yaml" jsonschema:"contents of compose.yaml (x-ports, x-machines, ... extensions supported)"`
		Services    []string `json:"services,omitempty" jsonschema:"only deploy these compose services"`
		Prune       bool     `json:"prune,omitempty" jsonschema:"remove services that are not in the file (destructive: needs confirmation)"`
		DryRun      bool     `json:"dry_run,omitempty" jsonschema:"validate and show the plan without changing anything"`
		Confirmation
	}
	register(s, "deploy_compose", "Create/update services from a compose file and deploy them. prune=true removes services missing from the file and needs confirm=true.",
		auth.LevelDeploy, destructive("Deploy compose"),
		func(ctx context.Context, c *call, in composeIn) (any, error) {
			req := &v1.DeployComposeRequest{Project: in.Project, Environment: in.Environment, ComposeYaml: in.ComposeYAML, Services: in.Services, Prune: in.Prune, DryRun: in.DryRun}
			if in.Prune && !in.DryRun {
				if dry, err := c.gate("deploy_compose", in.Project+"/"+in.Environment, in.Confirmation, func() (map[string]any, error) {
					pr := &v1.DeployComposeRequest{Project: in.Project, Environment: in.Environment, ComposeYaml: in.ComposeYAML, Services: in.Services, Prune: true, DryRun: true}
					r, err := c.api.Deploy.DeployCompose(ctx, connect.NewRequest(pr))
					if err != nil {
						return nil, err
					}
					return map[string]any{"action": "deploy compose with prune", "services_to_remove": r.Msg.Removed, "warnings": r.Msg.Warnings}, nil
				}); err != nil || dry != nil {
					return dry, err
				}
			}
			r, err := c.api.Deploy.DeployCompose(ctx, connect.NewRequest(req))
			if err != nil {
				return nil, err
			}
			svcs := make([]any, 0, len(r.Msg.Services))
			for _, sv := range r.Msg.Services {
				svcs = append(svcs, toAny(redactedService(sv)))
			}
			deps := make([]any, 0, len(r.Msg.Deployments))
			for _, d := range r.Msg.Deployments {
				deps = append(deps, toAny(redactedDeployment(d)))
			}
			return map[string]any{"dry_run": in.DryRun, "services": svcs, "deployments": deps, "warnings": r.Msg.Warnings, "removed": r.Msg.Removed}, nil
		})
}
