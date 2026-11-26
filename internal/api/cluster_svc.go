package api

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/events"
	"github.com/aetherix/aetherixcloud/internal/snapshot"
	"github.com/aetherix/aetherixcloud/internal/store"
	"github.com/aetherix/aetherixcloud/internal/upgrade"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type clusterSvc struct{ *server }

func (s *clusterSvc) GetCluster(ctx context.Context, _ *connect.Request[v1.GetClusterRequest]) (*connect.Response[v1.GetClusterResponse], error) {
	c, err := store.GetCluster(ctx, s.Store)
	if err != nil {
		return nil, internalErr(err)
	}
	return connect.NewResponse(&v1.GetClusterResponse{Cluster: c}), nil
}

func (s *clusterSvc) GetStatus(ctx context.Context, _ *connect.Request[v1.GetStatusRequest]) (*connect.Response[v1.GetStatusResponse], error) {
	c, _ := store.GetCluster(ctx, s.Store)
	ms, err := store.ListMachines(ctx, s.Store)
	if err != nil {
		return nil, internalErr(err)
	}
	resp := &v1.GetStatusResponse{Cluster: c, MachinesTotal: int32(len(ms)), Version: s.Version}
	if s.State != nil {
		resp.ServedBy = s.State.MachineID
	}
	for _, m := range ms {
		switch m.State {
		case v1.MachineState_MACHINE_STATE_UP:
			resp.MachinesUp++
		case v1.MachineState_MACHINE_STATE_SUSPECT:
			resp.MachinesSuspect++
		case v1.MachineState_MACHINE_STATE_DOWN:
			resp.MachinesDown++
		}
	}
	svcs, _ := store.ListServices(ctx, s.Store, nil)
	resp.ServicesTotal = int32(len(svcs))
	reps, _ := store.ListReplicas(ctx, s.Store, nil)
	for _, r := range reps {
		if !r.DesiredRunning {
			continue
		}
		resp.ReplicasDesired++
		if r.State == v1.ReplicaState_REPLICA_STATE_RUNNING {
			resp.ReplicasRunning++
		}
	}
	return connect.NewResponse(resp), nil
}

func (s *clusterSvc) UpdateCluster(ctx context.Context, req *connect.Request[v1.UpdateClusterRequest]) (*connect.Response[v1.UpdateClusterResponse], error) {
	c, err := store.GetCluster(ctx, s.Store)
	if err != nil {
		return nil, internalErr(err)
	}
	if req.Msg.Name != nil {
		c.Name = *req.Msg.Name
	}
	if req.Msg.Domain != nil {
		c.Domain = strings.TrimSpace(*req.Msg.Domain)
	}
	if req.Msg.DashboardHost != nil {
		c.DashboardHost = strings.ToLower(strings.TrimSpace(*req.Msg.DashboardHost))
	}
	if req.Msg.DashboardPort != nil {
		p := *req.Msg.DashboardPort
		if p < 0 || p > 65535 {
			return nil, errInvalid("dashboard_port must be 0–65535")
		}
		c.DashboardPort = p
	}
	if err := store.PutCluster(ctx, s.Store, c); err != nil {
		return nil, internalErr(err)
	}
	return connect.NewResponse(&v1.UpdateClusterResponse{Cluster: c}), nil
}

func (s *clusterSvc) ListEvents(ctx context.Context, req *connect.Request[v1.ListEventsRequest]) (*connect.Response[v1.ListEventsResponse], error) {
	out, err := events.List(ctx, s.Store, req.Msg.TypePrefix, int(req.Msg.Limit))
	if err != nil {
		return nil, internalErr(err)
	}
	return connect.NewResponse(&v1.ListEventsResponse{Events: out}), nil
}

// WatchEvents streams events. The first message is a synthetic
// "stream.open" event (it flushes the response headers so clients/proxies see
// the stream is live even when no events happen), and a "stream.ping" is sent
// every 25s as keepalive. Clients should ignore types with the "stream."
// prefix.
func (s *clusterSvc) WatchEvents(ctx context.Context, req *connect.Request[v1.WatchEventsRequest], stream *connect.ServerStream[v1.Event]) error {
	ch, err := events.Watch(ctx, s.Store, req.Msg.TypePrefix, int(req.Msg.Replay))
	if err != nil {
		return internalErr(err)
	}
	if err := stream.Send(&v1.Event{Type: "stream.open", Time: timestamppb.Now()}); err != nil {
		return err
	}
	tick := time.NewTicker(25 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			if err := stream.Send(&v1.Event{Type: "stream.ping", Time: timestamppb.Now()}); err != nil {
				return err
			}
		case e, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send(e); err != nil {
				return err
			}
		}
	}
}

// Backup streams a JSON snapshot of every store table, plus cluster identity
// secrets when NodeState is available (version 2).
func (s *clusterSvc) Backup(ctx context.Context, _ *connect.Request[v1.BackupRequest], stream *connect.ServerStream[v1.BackupChunk]) error {
	var ident *snapshot.Identity
	if s.State != nil {
		ident = &snapshot.Identity{
			ClusterID:  s.State.ClusterID,
			ClusterKey: s.State.ClusterKey,
			CACertPEM:  s.State.CACertPEM,
			CAKeyPEM:   s.State.CAKeyPEM,
		}
	}
	b, err := snapshot.Encode(ctx, s.Store, ident)
	if err != nil {
		return internalErr(err)
	}
	for len(b) > 0 {
		n := min(len(b), 64<<10)
		if err := stream.Send(&v1.BackupChunk{Data: b[:n]}); err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}

// Restore applies a streamed .snapshot into the store. See snapshot.Apply.
// Browsers cannot client-stream over fetch/HTTP/1.1; the SPA uses
// POST /api/cluster/restore instead (restoreHTTP.go).
func (s *clusterSvc) Restore(ctx context.Context, stream *connect.ClientStream[v1.RestoreChunk]) (*connect.Response[v1.RestoreResponse], error) {
	var buf bytes.Buffer
	force := false
	first := true
	for stream.Receive() {
		msg := stream.Msg()
		if first {
			force = msg.Force
			first = false
		}
		if len(msg.Data) > 0 {
			if _, err := buf.Write(msg.Data); err != nil {
				return nil, internalErr(err)
			}
		}
	}
	if err := stream.Err(); err != nil && err != io.EOF {
		return nil, err
	}
	out, err := s.applySnapshotBytes(ctx, buf.Bytes(), force)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(out), nil
}

func (s *clusterSvc) applySnapshotBytes(ctx context.Context, raw []byte, force bool) (*v1.RestoreResponse, error) {
	if len(raw) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errString("empty snapshot"))
	}
	snap, err := snapshot.Decode(raw)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	opts := snapshot.ApplyOptions{Force: force}
	if s.State != nil {
		opts.SelfMachineID = s.State.MachineID
		opts.SelfMachine = s.State.Machine(timestamppb.Now())
	}
	res, err := snapshot.Apply(ctx, s.Store, snap, opts)
	if err != nil {
		if strings.Contains(err.Error(), "non-fresh") {
			return nil, connect.NewError(connect.CodeFailedPrecondition, err)
		}
		return nil, internalErr(err)
	}
	_ = store.PutEvent(ctx, s.Store, &v1.Event{
		Id: newID(), Type: "cluster.restored",
		Message: "cluster state restored from snapshot", Time: timestamppb.Now(),
	})
	if s.OnChange != nil {
		s.OnChange()
	}
	return &v1.RestoreResponse{
		Tables:          int32(res.Tables),
		Rows:            int32(res.Rows),
		MachinesRemoved: int32(res.MachinesRemoved),
		ReplicasCleared: int32(res.ReplicasCleared),
	}, nil
}

func (s *clusterSvc) CheckUpdates(ctx context.Context, req *connect.Request[v1.CheckUpdatesRequest]) (*connect.Response[v1.CheckUpdatesResponse], error) {
	cfg, err := upgrade.GetConfig(ctx, s.Store)
	if err != nil {
		return nil, internalErr(err)
	}
	channel := req.Msg.Channel
	if channel == "" {
		channel = cfg.Channel
	}
	if channel == "" {
		channel = "latest"
	}
	repo := cfg.Repo
	if repo == "" {
		repo = os.Getenv("AETHERIX_REPO")
	}
	rel, err := upgrade.CheckLatest(ctx, repo, channel, nil)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	cur := s.Version
	return connect.NewResponse(&v1.CheckUpdatesResponse{
		CurrentVersion:  cur,
		LatestVersion:   rel.Tag,
		Channel:         channel,
		ReleaseUrl:      rel.URL,
		UpdateAvailable: !upgrade.Equal(cur, rel.Tag),
		ReleaseNotes:    rel.Notes,
	}), nil
}

func (s *clusterSvc) GetUpgrade(ctx context.Context, _ *connect.Request[v1.GetUpgradeRequest]) (*connect.Response[v1.GetUpgradeResponse], error) {
	resp, err := s.upgradeStatus(ctx)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (s *clusterSvc) StartUpgrade(ctx context.Context, req *connect.Request[v1.StartUpgradeRequest]) (*connect.Response[v1.StartUpgradeResponse], error) {
	ver := upgrade.Normalize(req.Msg.Version)
	if ver == "" || ver == "dev" {
		return nil, errInvalid("version is required (e.g. v0.4.0)")
	}
	cfg, err := upgrade.GetConfig(ctx, s.Store)
	if err != nil {
		return nil, internalErr(err)
	}
	machines, err := store.ListMachines(ctx, s.Store)
	if err != nil {
		return nil, internalErr(err)
	}
	want := map[string]bool{}
	for _, id := range req.Msg.MachineIds {
		want[id] = true
	}
	var behind []string
	for _, m := range machines {
		if len(want) > 0 && !want[m.Id] {
			continue
		}
		if upgrade.Equal(m.AgentVersion, ver) {
			continue
		}
		behind = append(behind, m.Id)
	}
	if len(behind) == 0 {
		if len(want) > 0 {
			return nil, errInvalid("selected machines are already on %s", ver)
		}
		return nil, errInvalid("all machines are already on %s", ver)
	}
	if req.Msg.Channel != "" {
		cfg.Channel = req.Msg.Channel
	}
	cfg.DesiredVersion = ver
	cfg.MachineIDs = behind
	if req.Msg.DownloadBase != "" {
		cfg.DownloadBase = strings.TrimRight(req.Msg.DownloadBase, "/")
	}
	cfg.StartedAt = time.Now().UTC()
	if err := upgrade.PutConfig(ctx, s.Store, cfg); err != nil {
		return nil, internalErr(err)
	}
	_ = store.PutEvent(ctx, s.Store, &v1.Event{
		Id: newID(), Type: "cluster.upgrade.started",
		Message: fmt.Sprintf("upgrade to %s started (%d machine(s))", ver, len(behind)),
		Time:    timestamppb.Now(),
	})
	status, err := s.upgradeStatus(ctx)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.StartUpgradeResponse{Status: status}), nil
}

func (s *clusterSvc) CancelUpgrade(ctx context.Context, _ *connect.Request[v1.CancelUpgradeRequest]) (*connect.Response[v1.CancelUpgradeResponse], error) {
	cfg, err := upgrade.GetConfig(ctx, s.Store)
	if err != nil {
		return nil, internalErr(err)
	}
	cfg.DesiredVersion = ""
	cfg.MachineIDs = nil
	cfg.StartedAt = time.Time{}
	if err := upgrade.PutConfig(ctx, s.Store, cfg); err != nil {
		return nil, internalErr(err)
	}
	_ = store.PutEvent(ctx, s.Store, &v1.Event{
		Id: newID(), Type: "cluster.upgrade.cancelled",
		Message: "cluster upgrade cancelled", Time: timestamppb.Now(),
	})
	status, err := s.upgradeStatus(ctx)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.CancelUpgradeResponse{Status: status}), nil
}

func (s *clusterSvc) upgradeStatus(ctx context.Context) (*v1.GetUpgradeResponse, error) {
	cfg, err := upgrade.GetConfig(ctx, s.Store)
	if err != nil {
		return nil, internalErr(err)
	}
	machines, onTarget, total, err := upgrade.StatusPayload(ctx, s.Store, cfg)
	if err != nil {
		return nil, internalErr(err)
	}
	resp := &v1.GetUpgradeResponse{
		Channel:        cfg.Channel,
		DesiredVersion: cfg.DesiredVersion,
		DownloadBase:   cfg.DownloadBase,
		MachineIds:     cfg.MachineIDs,
		Machines:       machines,
		OnTarget:       onTarget,
		Total:          total,
	}
	if !cfg.StartedAt.IsZero() {
		resp.StartedAt = timestamppb.New(cfg.StartedAt)
	}
	return resp, nil
}

type errString string

func (e errString) Error() string { return string(e) }
