package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/cluster"
	"github.com/aetherix/aetherixcloud/internal/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type machineSvc struct{ *server }

func (s *machineSvc) find(ctx context.Context, ref string) (*v1.Machine, error) {
	ms, err := store.ListMachines(ctx, s.Store)
	if err != nil {
		return nil, internalErr(err)
	}
	for _, m := range ms {
		if m.Id == ref || m.Name == ref {
			return m, nil
		}
	}
	return nil, errNotFound("machine", ref)
}

func (s *machineSvc) ListMachines(ctx context.Context, _ *connect.Request[v1.ListMachinesRequest]) (*connect.Response[v1.ListMachinesResponse], error) {
	ms, err := store.ListMachines(ctx, s.Store)
	if err != nil {
		return nil, internalErr(err)
	}
	return connect.NewResponse(&v1.ListMachinesResponse{Machines: ms}), nil
}

func (s *machineSvc) GetMachine(ctx context.Context, req *connect.Request[v1.GetMachineRequest]) (*connect.Response[v1.GetMachineResponse], error) {
	m, err := s.find(ctx, req.Msg.Machine)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.GetMachineResponse{Machine: m}), nil
}

func (s *machineSvc) joiner() *cluster.Server {
	return &cluster.Server{Store: s.Store, Leases: s.Leases, State: s.State}
}

func (s *machineSvc) CreateJoinToken(ctx context.Context, req *connect.Request[v1.CreateJoinTokenRequest]) (*connect.Response[v1.CreateJoinTokenResponse], error) {
	ttl := time.Duration(req.Msg.TtlSeconds) * time.Second
	tok, exp, err := s.joiner().CreateToken(ctx, ttl, req.Msg.Roles, req.Msg.MachineName)
	if err != nil {
		return nil, internalErr(err)
	}
	return connect.NewResponse(&v1.CreateJoinTokenResponse{
		Token:          tok,
		InstallCommand: fmt.Sprintf("curl -fsSL https://get.aetherix.cloud/install.sh | sudo sh -s -- --join %s", tok),
		ExpiresUnix:    exp.Unix(),
	}), nil
}

func (s *machineSvc) JoinMachine(ctx context.Context, req *connect.Request[v1.JoinMachineRequest]) (*connect.Response[v1.JoinMachineResponse], error) {
	resp, err := s.joiner().Join(ctx, req.Msg, remoteIP(req))
	switch {
	case err == nil:
		return connect.NewResponse(resp), nil
	case errors.Is(err, cluster.ErrTokenInvalid), errors.Is(err, cluster.ErrTokenExpired), errors.Is(err, cluster.ErrTokenUsed):
		return nil, connect.NewError(connect.CodePermissionDenied, err)
	default:
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
}

func (s *machineSvc) UpdateMachine(ctx context.Context, req *connect.Request[v1.UpdateMachineRequest]) (*connect.Response[v1.UpdateMachineResponse], error) {
	m, err := s.find(ctx, req.Msg.Machine)
	if err != nil {
		return nil, err
	}
	if req.Msg.Name != nil && *req.Msg.Name != "" {
		m.Name = *req.Msg.Name
	}
	if req.Msg.SetLabels {
		m.Labels = req.Msg.Labels
	}
	if req.Msg.SetRoles {
		m.Roles = req.Msg.Roles
	}
	if req.Msg.SetEndpoints {
		m.Endpoints = req.Msg.Endpoints
	}
	if err := store.PutMachine(ctx, s.Store, m); err != nil {
		return nil, internalErr(err)
	}
	s.changed()
	return connect.NewResponse(&v1.UpdateMachineResponse{Machine: m}), nil
}

func (s *machineSvc) CordonMachine(ctx context.Context, req *connect.Request[v1.CordonMachineRequest]) (*connect.Response[v1.CordonMachineResponse], error) {
	m, err := s.find(ctx, req.Msg.Machine)
	if err != nil {
		return nil, err
	}
	m.Cordoned = req.Msg.Cordoned
	if !m.Cordoned {
		m.Draining = false
	}
	if err := store.PutMachine(ctx, s.Store, m); err != nil {
		return nil, internalErr(err)
	}
	return connect.NewResponse(&v1.CordonMachineResponse{Machine: m}), nil
}

func (s *machineSvc) DrainMachine(ctx context.Context, req *connect.Request[v1.DrainMachineRequest]) (*connect.Response[v1.DrainMachineResponse], error) {
	m, err := s.find(ctx, req.Msg.Machine)
	if err != nil {
		return nil, err
	}
	m.Cordoned, m.Draining = true, true
	if err := store.PutMachine(ctx, s.Store, m); err != nil {
		return nil, internalErr(err)
	}
	// The placement controller moves reschedulable replicas off draining
	// machines; report what is stuck.
	resp := &v1.DrainMachineResponse{Machine: m}
	reps, _ := store.ListReplicas(ctx, s.Store, map[string]string{"machine_id": m.Id})
	seen := map[string]bool{}
	for _, r := range reps {
		if !r.DesiredRunning {
			continue
		}
		svc, err := store.GetService(ctx, s.Store, r.ServiceId)
		if err != nil {
			continue
		}
		if svc.Spec != nil && svc.Spec.Reschedulable {
			resp.ReplicasMoved++
		} else if !seen[svc.Id] {
			seen[svc.Id] = true
			resp.StrandedServiceIds = append(resp.StrandedServiceIds, svc.Id)
		}
	}
	return connect.NewResponse(resp), nil
}

func (s *machineSvc) RemoveMachine(ctx context.Context, req *connect.Request[v1.RemoveMachineRequest]) (*connect.Response[v1.RemoveMachineResponse], error) {
	m, err := s.find(ctx, req.Msg.Machine)
	if err != nil {
		return nil, err
	}
	if s.State != nil && m.Id == s.State.MachineID {
		return nil, errInvalid("cannot remove the machine serving this request")
	}
	reps, _ := store.ListReplicas(ctx, s.Store, map[string]string{"machine_id": m.Id})
	active := 0
	for _, r := range reps {
		if r.DesiredRunning {
			active++
		}
	}
	if active > 0 && !req.Msg.Force {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("machine %s still runs %d replicas; drain it or use force", m.Name, active))
	}
	for _, r := range reps {
		_ = s.Store.Delete(ctx, store.TableReplicas, r.Id)
	}
	if err := s.Store.Delete(ctx, store.TableMachines, m.Id); err != nil {
		return nil, internalErr(err)
	}
	_ = store.PutEvent(ctx, s.Store, &v1.Event{Id: newID(), Type: "machine.removed", SubjectId: m.Id,
		Message: "machine " + m.Name + " removed", Time: timestamppb.Now()})
	return connect.NewResponse(&v1.RemoveMachineResponse{}), nil
}

func (s *machineSvc) ReportHeartbeat(ctx context.Context, req *connect.Request[v1.ReportHeartbeatRequest]) (*connect.Response[v1.ReportHeartbeatResponse], error) {
	m, err := store.GetMachine(ctx, s.Store, req.Msg.MachineId)
	if err != nil {
		return nil, errNotFound("machine", req.Msg.MachineId)
	}
	m.Used, m.Capacity = req.Msg.Used, req.Msg.Capacity
	m.LastSeen = timestamppb.Now()
	if err := store.PutMachine(ctx, s.Store, m); err != nil {
		return nil, internalErr(err)
	}
	return connect.NewResponse(&v1.ReportHeartbeatResponse{}), nil
}
