package api

import (
	"context"
	"errors"
	"io"
	"sync"

	"connectrpc.com/connect"
	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/gen/aetherix/v1/aetherixv1connect"
	"github.com/aetherix/aetherixcloud/internal/runtime"
	"github.com/aetherix/aetherixcloud/internal/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const proxiedHeader = "Ax-Proxied"

type logSvc struct{ *server }

func (s *logSvc) StreamLogs(ctx context.Context, req *connect.Request[v1.StreamLogsRequest], stream *connect.ServerStream[v1.LogEntry]) error {
	where := map[string]string{"service_id": req.Msg.ServiceId}
	reps, err := store.ListReplicas(ctx, s.Store, where)
	if err != nil {
		return internalErr(err)
	}
	var targets []*v1.Replica
	for _, r := range reps {
		if req.Msg.ReplicaId != "" && r.Id != req.Msg.ReplicaId {
			continue
		}
		if r.ContainerId == "" || (!r.DesiredRunning && r.State == v1.ReplicaState_REPLICA_STATE_STOPPED) {
			continue
		}
		targets = append(targets, r)
	}
	if req.Msg.ReplicaId != "" && len(targets) == 0 {
		return errNotFound("replica", req.Msg.ReplicaId)
	}
	proxied := req.Header().Get(proxiedHeader) != ""
	var mu sync.Mutex
	send := func(e *v1.LogEntry) error { mu.Lock(); defer mu.Unlock(); return stream.Send(e) }
	var wg sync.WaitGroup
	errs := make(chan error, len(targets))
	for _, r := range targets {
		local := s.State == nil || r.MachineId == s.State.MachineID
		if !local && proxied {
			continue // peers only serve their own replicas
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if local {
				errs <- s.localLogs(ctx, r, req.Msg, send)
			} else {
				errs <- s.remoteLogs(ctx, r, req.Msg, send)
			}
		}()
	}
	wg.Wait()
	close(errs)
	var first error
	for e := range errs {
		if e != nil && first == nil && ctx.Err() == nil {
			first = e
		}
	}
	return first
}

func (s *logSvc) localLogs(ctx context.Context, r *v1.Replica, q *v1.StreamLogsRequest, send func(*v1.LogEntry) error) error {
	if s.Runtime == nil {
		return connect.NewError(connect.CodeUnavailable, errors.New("no container runtime on this node"))
	}
	opts := runtime.LogOptions{Follow: q.Follow, Tail: int(q.Tail)}
	if q.Since != nil {
		opts.Since = q.Since.AsTime()
	}
	ch, err := s.Runtime.StreamLogs(ctx, r.ContainerId, opts)
	if err != nil {
		return err
	}
	for ll := range ch {
		e := &v1.LogEntry{Time: timestamppb.New(ll.Time), ReplicaId: r.Id, MachineId: r.MachineId, Stream: ll.Stream, Line: ll.Line}
		if err := send(e); err != nil {
			return err
		}
	}
	return nil
}

func (s *logSvc) remoteLogs(ctx context.Context, r *v1.Replica, q *v1.StreamLogsRequest, send func(*v1.LogEntry) error) error {
	m, err := store.GetMachine(ctx, s.Store, r.MachineId)
	if err != nil {
		return err
	}
	c := aetherixv1connect.NewLogServiceClient(s.HTTPClient, s.PeerURL(m.MeshIp), connect.WithGRPC())
	creq := connect.NewRequest(&v1.StreamLogsRequest{ServiceId: q.ServiceId, ReplicaId: r.Id, Follow: q.Follow, Tail: q.Tail, Since: q.Since, Timestamps: q.Timestamps})
	creq.Header().Set(proxiedHeader, "1")
	st, err := c.StreamLogs(ctx, creq)
	if err != nil {
		return err
	}
	defer st.Close()
	for st.Receive() {
		if err := send(st.Msg()); err != nil {
			return err
		}
	}
	if err := st.Err(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func (s *logSvc) StreamBuildLogs(context.Context, *connect.Request[v1.StreamBuildLogsRequest], *connect.ServerStream[v1.LogEntry]) error {
	return connect.NewError(connect.CodeUnimplemented, errors.New("build logs are provided by the build worker"))
}
