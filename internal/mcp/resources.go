package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Resource URIs.
const (
	URIClusterStatus = "aetherix://cluster/status"
	uriProject       = "aetherix://project/"
	uriService       = "aetherix://service/"
)

func jsonResource(uri string, v any) (*sdk.ReadResourceResult, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{URI: uri, MIMEType: "application/json", Text: string(b)}}}, nil
}

func (s *Server) registerResources() {
	read := func(h func(ctx context.Context, c *call, uri string) (*sdk.ReadResourceResult, error)) sdk.ResourceHandler {
		return func(ctx context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
			c, err := s.callFor(req.Extra, req.Session)
			if err != nil {
				return nil, err
			}
			res, err := h(ctx, c, req.Params.URI)
			c.audit(ctx, "mcp.resource.read", map[string]string{"uri": req.Params.URI}, err)
			if err != nil {
				if connect.CodeOf(err) == connect.CodeNotFound {
					return nil, sdk.ResourceNotFoundError(req.Params.URI)
				}
				return nil, friendly(err)
			}
			return res, nil
		}
	}

	s.srv.AddResource(&sdk.Resource{URI: URIClusterStatus, Name: "cluster-status", Title: "Cluster status",
		Description: "Machines up/down, services and replica counts.", MIMEType: "application/json"},
		read(func(ctx context.Context, c *call, uri string) (*sdk.ReadResourceResult, error) {
			r, err := c.api.Cluster.GetStatus(ctx, connect.NewRequest(&v1.GetStatusRequest{}))
			if err != nil {
				return nil, err
			}
			v := toAny(r.Msg).(map[string]any)
			if cl, ok := v["cluster"].(map[string]any); ok {
				delete(cl, "ca_pem")
			}
			return jsonResource(uri, v)
		}))

	s.srv.AddResourceTemplate(&sdk.ResourceTemplate{URITemplate: uriProject + "{id}", Name: "project", Title: "Project",
		Description: "A project with its environments and services (project ID or name).", MIMEType: "application/json"},
		read(func(ctx context.Context, c *call, uri string) (*sdk.ReadResourceResult, error) {
			id := strings.TrimPrefix(uri, uriProject)
			r, err := c.api.Project.GetProject(ctx, connect.NewRequest(&v1.GetProjectRequest{Project: id}))
			if err != nil {
				return nil, err
			}
			svcs := make([]any, 0, len(r.Msg.Services))
			for _, sv := range r.Msg.Services {
				svcs = append(svcs, toAny(redactedService(sv)))
			}
			return jsonResource(uri, map[string]any{"project": toAny(r.Msg.Project), "environments": listAny(r.Msg.Environments), "services": svcs})
		}))

	s.srv.AddResourceTemplate(&sdk.ResourceTemplate{URITemplate: uriService + "{id}", Name: "service", Title: "Service",
		Description: "A service with replicas and domains (service ID).", MIMEType: "application/json"},
		read(func(ctx context.Context, c *call, uri string) (*sdk.ReadResourceResult, error) {
			id := strings.TrimPrefix(uri, uriService)
			r, err := c.api.Service.GetService(ctx, connect.NewRequest(&v1.GetServiceRequest{Service: id}))
			if err != nil {
				return nil, err
			}
			return jsonResource(uri, map[string]any{"service": toAny(redactedService(r.Msg.Service)), "replicas": listAny(r.Msg.Replicas), "domains": listAny(r.Msg.Domains)})
		}))

	s.srv.AddResourceTemplate(&sdk.ResourceTemplate{URITemplate: uriService + "{id}/logs", Name: "service-logs", Title: "Service logs",
		Description: "The last 200 log lines of a service (credentials redacted).", MIMEType: "text/plain"},
		read(func(ctx context.Context, c *call, uri string) (*sdk.ReadResourceResult, error) {
			id := strings.TrimSuffix(strings.TrimPrefix(uri, uriService), "/logs")
			out, err := c.logs(ctx, id, "", 200, 0)
			if err != nil {
				return nil, err
			}
			lines, _ := out["lines"].([]string)
			return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{URI: uri, MIMEType: "text/plain", Text: strings.Join(lines, "\n")}}}, nil
		}))
}

func (s *Server) registerPrompts() {
	prompt := func(name, title, desc string, args []*sdk.PromptArgument, build func(a map[string]string) string) {
		s.srv.AddPrompt(&sdk.Prompt{Name: name, Title: title, Description: desc, Arguments: args},
			func(ctx context.Context, req *sdk.GetPromptRequest) (*sdk.GetPromptResult, error) {
				a := req.Params.Arguments
				for _, ar := range args {
					if ar.Required && strings.TrimSpace(a[ar.Name]) == "" {
						return nil, fmt.Errorf("missing required argument %q", ar.Name)
					}
				}
				return &sdk.GetPromptResult{Description: desc, Messages: []*sdk.PromptMessage{{Role: "user", Content: &sdk.TextContent{Text: build(a)}}}}, nil
			})
	}
	prompt("diagnose_failing_deployment", "Diagnose a failing deployment",
		"Investigate why a service's latest deployment is failing and propose a fix.",
		[]*sdk.PromptArgument{{Name: "service", Description: "service name or ID", Required: true}, {Name: "project", Description: "project name (when service is a name)"}},
		func(a map[string]string) string {
			return fmt.Sprintf(`Diagnose the failing deployment of service %q (project %q) on AetherixCloud.
1. Call get_service and list_deployments to see the current and recent deployments and their status messages.
2. Call get_deployment for the newest failed one; if it has a build_id call get_build_logs.
3. Call get_logs for the running replicas and look for crashes, bad configuration or failing health checks.
4. Check list_variables for missing configuration (secret values are redacted) and get_cluster_status / list_machines for capacity or node problems.
5. Summarise the root cause and propose a concrete fix. Do not change anything until I agree; if a rollback is the safest fix, say so and ask first.`, a["service"], a["project"])
		})
	prompt("deploy_repo", "Deploy a GitHub repo",
		"Create a service from a GitHub repository, configure it and deploy it.",
		[]*sdk.PromptArgument{{Name: "repo", Description: "owner/name", Required: true}, {Name: "project", Description: "target project (created if it does not exist)"}, {Name: "port", Description: "port the app listens on"}},
		func(a map[string]string) string {
			return fmt.Sprintf(`Deploy the GitHub repository %q on AetherixCloud.
1. Call list_projects; use project %q if it exists, otherwise ask me which project to use.
2. Call create_service with github_repo=%q (port %q if given) and deploy=true.
3. Ask me for any environment variables the app needs and set them with set_variables (use secrets for credentials).
4. Watch list_deployments / get_deployment until the deployment is active or failed, calling get_build_logs and get_logs on failure.
5. Report the public URL from list_domains.`, a["repo"], a["project"], a["repo"], a["port"])
		})
	prompt("review_cluster", "Review cluster health", "Check machines, services and recent events and report problems.", nil,
		func(map[string]string) string {
			return `Review the health of this AetherixCloud cluster: call get_cluster_status, list_machines, list_services and list_events. Report machines that are suspect/down/cordoned, services with fewer running replicas than desired, recent failed deployments, and recommend next steps. Do not change anything.`
		})
}
