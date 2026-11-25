package mcp

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/auth"
)

func (c *call) machine(ctx context.Context, ref string) (*v1.Machine, error) {
	r, err := c.api.Machine.GetMachine(ctx, connect.NewRequest(&v1.GetMachineRequest{Machine: ref}))
	if err != nil {
		return nil, err
	}
	return r.Msg.Machine, nil
}

func (c *call) replicasOn(ctx context.Context, machineID string) []*v1.Replica {
	r, err := c.api.Service.ListReplicas(ctx, connect.NewRequest(&v1.ListReplicasRequest{MachineId: machineID}))
	if err != nil {
		return nil
	}
	var out []*v1.Replica
	for _, x := range r.Msg.Replicas {
		if x.DesiredRunning {
			out = append(out, x)
		}
	}
	return out
}

func (s *Server) registerAdminTools() {
	type joinIn struct {
		MachineName string `json:"machine_name,omitempty" jsonschema:"name for the new machine"`
		TTLSeconds  int    `json:"ttl_seconds,omitempty" jsonschema:"token lifetime (default 3600)"`
		Builder     bool   `json:"builder,omitempty" jsonschema:"also give the machine the builder role"`
		Ingress     bool   `json:"ingress,omitempty" jsonschema:"also give the machine the ingress role"`
	}
	register(s, "create_join_command", "Create a one-time command that joins a new machine to the cluster. The command contains a short-lived join token: treat it as secret.",
		auth.LevelAdmin, write("Create join command", false),
		func(ctx context.Context, c *call, in joinIn) (any, error) {
			roles := []v1.MachineRole{v1.MachineRole_MACHINE_ROLE_WORKER}
			if in.Builder {
				roles = append(roles, v1.MachineRole_MACHINE_ROLE_BUILDER)
			}
			if in.Ingress {
				roles = append(roles, v1.MachineRole_MACHINE_ROLE_INGRESS)
			}
			r, err := c.api.Machine.CreateJoinToken(ctx, connect.NewRequest(&v1.CreateJoinTokenRequest{TtlSeconds: int32(in.TTLSeconds), Roles: roles, MachineName: in.MachineName}))
			if err != nil {
				return nil, err
			}
			return map[string]any{"install_command": r.Msg.InstallCommand, "expires_unix": r.Msg.ExpiresUnix, "one_time": true}, nil
		})

	type machineIn struct {
		Machine string `json:"machine" jsonschema:"machine name or ID"`
	}
	type cordonIn struct {
		Machine  string `json:"machine" jsonschema:"machine name or ID"`
		Cordoned *bool  `json:"cordoned,omitempty" jsonschema:"true (default) stops scheduling new replicas; false uncordons"`
	}
	register(s, "cordon_machine", "Stop (or resume) scheduling new replicas on a machine. Existing replicas keep running.", auth.LevelAdmin, write("Cordon machine", true),
		func(ctx context.Context, c *call, in cordonIn) (any, error) {
			v := in.Cordoned == nil || *in.Cordoned
			r, err := c.api.Machine.CordonMachine(ctx, connect.NewRequest(&v1.CordonMachineRequest{Machine: in.Machine, Cordoned: v}))
			if err != nil {
				return nil, err
			}
			return map[string]any{"machine": toAny(r.Msg.Machine)}, nil
		})

	type drainIn struct {
		machineIn
		Confirmation
	}
	register(s, "drain_machine", "Cordon a machine and move its reschedulable replicas elsewhere. Disruptive: needs confirm=true.", auth.LevelAdmin, destructive("Drain machine"),
		func(ctx context.Context, c *call, in drainIn) (any, error) {
			m, err := c.machine(ctx, in.Machine)
			if err != nil {
				return nil, err
			}
			if dry, err := c.gate("drain_machine", m.Id, in.Confirmation, func() (map[string]any, error) {
				reps := c.replicasOn(ctx, m.Id)
				return map[string]any{"action": "drain", "machine": m.Name, "replicas_on_machine": len(reps),
					"effect": "the machine is cordoned and reschedulable replicas are moved; replicas of non-reschedulable services (or with local volumes) become stranded"}, nil
			}); err != nil || dry != nil {
				return dry, err
			}
			r, err := c.api.Machine.DrainMachine(ctx, connect.NewRequest(&v1.DrainMachineRequest{Machine: m.Id}))
			if err != nil {
				return nil, err
			}
			return map[string]any{"executed": true, "machine": toAny(r.Msg.Machine), "replicas_moved": r.Msg.ReplicasMoved, "stranded_service_ids": r.Msg.StrandedServiceIds}, nil
		})

	type removeIn struct {
		machineIn
		Force bool `json:"force,omitempty" jsonschema:"remove even if the machine is unreachable or still runs replicas"`
		Confirmation
	}
	register(s, "remove_machine", "Remove a machine from the cluster. Destructive: needs confirm=true.", auth.LevelAdmin, destructive("Remove machine"),
		func(ctx context.Context, c *call, in removeIn) (any, error) {
			m, err := c.machine(ctx, in.Machine)
			if err != nil {
				return nil, err
			}
			if dry, err := c.gate("remove_machine", fmt.Sprintf("%s/%t", m.Id, in.Force), in.Confirmation, func() (map[string]any, error) {
				return map[string]any{"action": "remove machine", "machine": m.Name, "state": m.State.String(),
					"replicas_on_machine": len(c.replicasOn(ctx, m.Id)), "force": in.Force}, nil
			}); err != nil || dry != nil {
				return dry, err
			}
			if _, err := c.api.Machine.RemoveMachine(ctx, connect.NewRequest(&v1.RemoveMachineRequest{Machine: m.Id, Force: in.Force})); err != nil {
				return nil, err
			}
			return map[string]any{"executed": true, "removed": m.Name}, nil
		})

	type deleteServiceIn struct {
		ref
		Confirmation
	}
	register(s, "delete_service", "Delete a service, stopping its replicas and removing its domains. Destructive: needs confirm=true.", auth.LevelAdmin, destructive("Delete service"),
		func(ctx context.Context, c *call, in deleteServiceIn) (any, error) {
			g, err := c.api.Service.GetService(ctx, connect.NewRequest(&v1.GetServiceRequest{Service: in.Service, Project: in.Project, Environment: in.Environment}))
			if err != nil {
				return nil, err
			}
			svc := g.Msg.Service
			if dry, err := c.gate("delete_service", svc.Id, in.Confirmation, func() (map[string]any, error) {
				return map[string]any{"action": "delete service", "service": svc.Name, "id": svc.Id, "replicas_to_stop": len(g.Msg.Replicas),
					"domains_to_remove": len(g.Msg.Domains), "effect": "traffic stops immediately; deployment history is removed"}, nil
			}); err != nil || dry != nil {
				return dry, err
			}
			if _, err := c.api.Service.DeleteService(ctx, connect.NewRequest(&v1.DeleteServiceRequest{ServiceId: svc.Id, Confirm: true})); err != nil {
				return nil, err
			}
			return map[string]any{"executed": true, "deleted": svc.Name}, nil
		})

	type deleteProjectIn struct {
		Project string `json:"project" jsonschema:"project name or ID"`
		Confirmation
	}
	register(s, "delete_project", "Delete a project with all its environments, services and variables. Destructive: needs confirm=true.", auth.LevelAdmin, destructive("Delete project"),
		func(ctx context.Context, c *call, in deleteProjectIn) (any, error) {
			g, err := c.api.Project.GetProject(ctx, connect.NewRequest(&v1.GetProjectRequest{Project: in.Project}))
			if err != nil {
				return nil, err
			}
			p := g.Msg.Project
			if dry, err := c.gate("delete_project", p.Id, in.Confirmation, func() (map[string]any, error) {
				names := make([]string, 0, len(g.Msg.Services))
				for _, sv := range g.Msg.Services {
					names = append(names, sv.Name)
				}
				return map[string]any{"action": "delete project", "project": p.Name, "environments": len(g.Msg.Environments),
					"services_to_delete": names, "effect": "every service stops and all variables are deleted"}, nil
			}); err != nil || dry != nil {
				return dry, err
			}
			if _, err := c.api.Project.DeleteProject(ctx, connect.NewRequest(&v1.DeleteProjectRequest{Project: p.Id, Confirm: true})); err != nil {
				return nil, err
			}
			return map[string]any{"executed": true, "deleted": p.Name}, nil
		})
}
