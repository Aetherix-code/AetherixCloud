package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"connectrpc.com/connect"
	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/gen/aetherix/v1/aetherixv1connect"
	"github.com/aetherix/aetherixcloud/internal/ingress"
	"github.com/aetherix/aetherixcloud/internal/runtime"
	"github.com/aetherix/aetherixcloud/internal/store"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type serviceSvc struct{ *server }

// NormalizeSpec fills defaults for a new service spec.
func NormalizeSpec(spec *v1.ServiceSpec) *v1.ServiceSpec {
	if spec == nil {
		spec = &v1.ServiceSpec{}
	}
	spec = proto.Clone(spec).(*v1.ServiceSpec)
	if spec.Mode == v1.ServiceMode_SERVICE_MODE_UNSPECIFIED {
		spec.Mode = v1.ServiceMode_SERVICE_MODE_REPLICATED
	}
	if spec.Mode == v1.ServiceMode_SERVICE_MODE_REPLICATED && spec.Replicas == 0 {
		spec.Replicas = 1
	}
	if spec.RestartPolicy == "" {
		spec.RestartPolicy = "always"
	}
	if spec.Strategy == v1.DeployStrategy_DEPLOY_STRATEGY_UNSPECIFIED {
		spec.Strategy = v1.DeployStrategy_DEPLOY_STRATEGY_START_FIRST
	}
	if spec.Image == "" && spec.GetSource().GetImage() != "" {
		spec.Image = spec.Source.Image
	}
	return spec
}

func (s *serviceSvc) CreateService(ctx context.Context, req *connect.Request[v1.CreateServiceRequest]) (*connect.Response[v1.CreateServiceResponse], error) {
	if req.Msg.Name == "" {
		return nil, errInvalid("name is required")
	}
	p, err := s.project(ctx, req.Msg.Project)
	if err != nil {
		return nil, err
	}
	env, err := s.environment(ctx, p.Id, req.Msg.Environment)
	if err != nil {
		return nil, err
	}
	if existing, _ := store.ListServices(ctx, s.Store, map[string]string{"project_id": p.Id, "environment_id": env.Id, "name": req.Msg.Name}); len(existing) > 0 {
		return nil, connect.NewError(connect.CodeAlreadyExists, errorf("service %q exists", req.Msg.Name))
	}
	spec := NormalizeSpec(req.Msg.Spec)
	if err := AssignTCPPorts(ctx, s.Store, spec, ""); err != nil {
		return nil, internalErr(err)
	}
	svc := &v1.Service{Id: newID(), ProjectId: p.Id, EnvironmentId: env.Id, Name: req.Msg.Name,
		Spec: spec, CreatedAt: timestamppb.Now(), UpdatedAt: timestamppb.Now()}
	if err := store.PutService(ctx, s.Store, svc); err != nil {
		return nil, internalErr(err)
	}
	resp := &v1.CreateServiceResponse{Service: svc}
	if req.Msg.Deploy {
		d, err := (&deploySvc{s.server}).start(ctx, svc, nil)
		if err != nil {
			return nil, err
		}
		resp.Deployment = d
	}
	return connect.NewResponse(resp), nil
}

func (s *serviceSvc) GetService(ctx context.Context, req *connect.Request[v1.GetServiceRequest]) (*connect.Response[v1.GetServiceResponse], error) {
	svc, err := s.service(ctx, req.Msg.Service, req.Msg.Project, req.Msg.Environment)
	if err != nil {
		return nil, err
	}
	reps, _ := store.ListReplicas(ctx, s.Store, map[string]string{"service_id": svc.Id})
	doms, _ := store.ListDomains(ctx, s.Store, svc.Id)
	return connect.NewResponse(&v1.GetServiceResponse{Service: svc, Replicas: visibleReplicas(reps), Domains: doms}), nil
}

func (s *serviceSvc) ListServices(ctx context.Context, req *connect.Request[v1.ListServicesRequest]) (*connect.Response[v1.ListServicesResponse], error) {
	where := map[string]string{}
	if req.Msg.Project != "" {
		p, err := s.project(ctx, req.Msg.Project)
		if err != nil {
			return nil, err
		}
		where["project_id"] = p.Id
		if req.Msg.Environment != "" {
			e, err := s.environment(ctx, p.Id, req.Msg.Environment)
			if err != nil {
				return nil, err
			}
			where["environment_id"] = e.Id
		}
	}
	svcs, err := store.ListServices(ctx, s.Store, where)
	if err != nil {
		return nil, internalErr(err)
	}
	return connect.NewResponse(&v1.ListServicesResponse{Services: svcs}), nil
}

// mergeSpec replaces the masked top-level fields of dst with those of src.
func mergeSpec(dst, src *v1.ServiceSpec, mask []string) error {
	if len(mask) == 0 {
		proto.Reset(dst)
		proto.Merge(dst, src)
		return nil
	}
	dm, sm := dst.ProtoReflect(), src.ProtoReflect()
	for _, name := range mask {
		fd := dm.Descriptor().Fields().ByName(protoName(name))
		if fd == nil {
			return fmt.Errorf("unknown ServiceSpec field %q", name)
		}
		if sm.Has(fd) {
			dm.Set(fd, sm.Get(fd))
		} else {
			dm.Clear(fd)
		}
	}
	return nil
}

func (s *serviceSvc) UpdateService(ctx context.Context, req *connect.Request[v1.UpdateServiceRequest]) (*connect.Response[v1.UpdateServiceResponse], error) {
	svc, err := store.GetService(ctx, s.Store, req.Msg.ServiceId)
	if err != nil {
		return nil, errNotFound("service", req.Msg.ServiceId)
	}
	if req.Msg.Spec == nil {
		return nil, errInvalid("spec is required")
	}
	if svc.Spec == nil {
		svc.Spec = &v1.ServiceSpec{}
	}
	if err := mergeSpec(svc.Spec, req.Msg.Spec, req.Msg.UpdateMask); err != nil {
		return nil, errInvalid("%v", err)
	}
	svc.Spec = NormalizeSpec(svc.Spec)
	if err := AssignTCPPorts(ctx, s.Store, svc.Spec, svc.Id); err != nil {
		return nil, internalErr(err)
	}
	svc.UpdatedAt = timestamppb.Now()
	if err := store.PutService(ctx, s.Store, svc); err != nil {
		return nil, internalErr(err)
	}
	return connect.NewResponse(&v1.UpdateServiceResponse{Service: svc}), nil
}

func (s *serviceSvc) deleteService(ctx context.Context, svc *v1.Service) error {
	reps, _ := store.ListReplicas(ctx, s.Store, map[string]string{"service_id": svc.Id})
	for _, r := range reps {
		r.DesiredRunning = false
		r.State = v1.ReplicaState_REPLICA_STATE_STOPPING
		_ = store.PutReplica(ctx, s.Store, r)
	}
	doms, _ := store.ListDomains(ctx, s.Store, svc.Id)
	for _, d := range doms {
		_ = s.Store.Delete(ctx, store.TableDomains, d.Id)
	}
	vars, _ := store.ListProto[v1.Variable](ctx, s.Store, store.TableVariables, store.Query{Where: map[string]string{"service_id": svc.Id}})
	for _, v := range vars {
		_ = s.Store.Delete(ctx, store.TableVariables, v.Id)
	}
	if err := s.Store.Delete(ctx, store.TableServices, svc.Id); err != nil {
		return internalErr(err)
	}
	s.changed()
	return nil
}

func (s *serviceSvc) DeleteService(ctx context.Context, req *connect.Request[v1.DeleteServiceRequest]) (*connect.Response[v1.DeleteServiceResponse], error) {
	svc, err := store.GetService(ctx, s.Store, req.Msg.ServiceId)
	if err != nil {
		return nil, errNotFound("service", req.Msg.ServiceId)
	}
	if !req.Msg.Confirm {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errorf("deleting %s stops all its replicas; set confirm", svc.Name))
	}
	if err := s.deleteService(ctx, svc); err != nil {
		return nil, err
	}
	return connect.NewResponse(&v1.DeleteServiceResponse{}), nil
}

func (s *serviceSvc) ScaleService(ctx context.Context, req *connect.Request[v1.ScaleServiceRequest]) (*connect.Response[v1.ScaleServiceResponse], error) {
	svc, err := store.GetService(ctx, s.Store, req.Msg.ServiceId)
	if err != nil {
		return nil, errNotFound("service", req.Msg.ServiceId)
	}
	if req.Msg.Replicas < 0 {
		return nil, errInvalid("replicas must be >= 0")
	}
	if svc.Spec.Mode == v1.ServiceMode_SERVICE_MODE_GLOBAL {
		return nil, errInvalid("global services cannot be scaled")
	}
	svc.Spec.Replicas = req.Msg.Replicas
	svc.UpdatedAt = timestamppb.Now()
	if err := store.PutService(ctx, s.Store, svc); err != nil {
		return nil, internalErr(err)
	}
	// Scaling edits the active deployment in place (no rollout).
	if svc.ActiveDeploymentId != "" {
		if d, err := store.GetDeployment(ctx, s.Store, svc.ActiveDeploymentId); err == nil {
			d.Spec.Replicas = req.Msg.Replicas
			if err := store.PutDeployment(ctx, s.Store, d); err != nil {
				return nil, internalErr(err)
			}
		}
	}
	s.changed()
	return connect.NewResponse(&v1.ScaleServiceResponse{Service: svc}), nil
}

func (s *serviceSvc) RestartService(ctx context.Context, req *connect.Request[v1.RestartServiceRequest]) (*connect.Response[v1.RestartServiceResponse], error) {
	svc, err := store.GetService(ctx, s.Store, req.Msg.ServiceId)
	if err != nil {
		return nil, errNotFound("service", req.Msg.ServiceId)
	}
	reps, _ := store.ListReplicas(ctx, s.Store, map[string]string{"service_id": svc.Id})
	for _, r := range reps {
		if r.DesiredRunning {
			r.DesiredRunning = false
			r.State = v1.ReplicaState_REPLICA_STATE_STOPPING
			if err := store.PutReplica(ctx, s.Store, r); err != nil {
				return nil, internalErr(err)
			}
		}
	}
	s.changed()
	return connect.NewResponse(&v1.RestartServiceResponse{}), nil
}

func (s *serviceSvc) ListReplicas(ctx context.Context, req *connect.Request[v1.ListReplicasRequest]) (*connect.Response[v1.ListReplicasResponse], error) {
	where := map[string]string{}
	if req.Msg.ServiceId != "" {
		where["service_id"] = req.Msg.ServiceId
	}
	if req.Msg.MachineId != "" {
		where["machine_id"] = req.Msg.MachineId
	}
	reps, err := store.ListReplicas(ctx, s.Store, where)
	if err != nil {
		return nil, internalErr(err)
	}
	return connect.NewResponse(&v1.ListReplicasResponse{Replicas: visibleReplicas(reps)}), nil
}

// visibleReplicas drops retired rows (desired_running=false and STOPPED).
// Those linger ~1m for GC after stop/restart/scale-down but must not surface
// as current replica state in the UI (status badges, replica lists).
func visibleReplicas(reps []*v1.Replica) []*v1.Replica {
	out := make([]*v1.Replica, 0, len(reps))
	for _, r := range reps {
		if r.DesiredRunning || r.State != v1.ReplicaState_REPLICA_STATE_STOPPED {
			out = append(out, r)
		}
	}
	return out
}

func (s *serviceSvc) ListDomains(ctx context.Context, req *connect.Request[v1.ListDomainsRequest]) (*connect.Response[v1.ListDomainsResponse], error) {
	d, err := store.ListDomains(ctx, s.Store, req.Msg.ServiceId)
	if err != nil {
		return nil, internalErr(err)
	}
	return connect.NewResponse(&v1.ListDomainsResponse{Domains: d}), nil
}

// AddDomain records the domain. Certificates/routing are the ingress worker's job.
// An empty hostname enables the generated default HTTPS URL for the service
// (sslip.io / cluster domain / .localhost), identified by id "gen-<service>".
func (s *serviceSvc) AddDomain(ctx context.Context, req *connect.Request[v1.AddDomainRequest]) (*connect.Response[v1.AddDomainResponse], error) {
	host := strings.ToLower(strings.TrimSpace(req.Msg.Hostname))
	svc, err := store.GetService(ctx, s.Store, req.Msg.ServiceId)
	if err != nil {
		return nil, errNotFound("service", req.Msg.ServiceId)
	}
	if req.Msg.Port <= 0 {
		return nil, errInvalid("container port is required")
	}

	var d *v1.Domain
	if host == "" {
		ho, err := s.hostOptions(ctx)
		if err != nil {
			return nil, internalErr(err)
		}
		pname := ""
		if p, err := store.GetProto[v1.Project](ctx, s.Store, store.TableProjects, svc.ProjectId); err == nil {
			pname = p.Name
		}
		host = ingress.DefaultHostname(svc.Name, pname, ho)
		if host == "" {
			return nil, errInvalid("cannot generate a default hostname (set a cluster domain or join an ingress node with a public IP)")
		}
		d = &v1.Domain{Id: "gen-" + svc.Id, ServiceId: svc.Id, Hostname: host, Port: req.Msg.Port, TlsState: "pending", Generated: true}
	} else {
		if strings.ContainsAny(host, " /:") {
			return nil, errInvalid("invalid hostname %q", req.Msg.Hostname)
		}
		d = &v1.Domain{Id: newID(), ServiceId: svc.Id, Hostname: host, Port: req.Msg.Port, TlsState: "pending"}
	}
	if existing, _ := store.ListDomains(ctx, s.Store, ""); len(existing) > 0 {
		for _, row := range existing {
			if row.Hostname == host && row.Id != d.Id {
				return nil, connect.NewError(connect.CodeAlreadyExists, errorf("hostname %s is already in use", host))
			}
		}
	}
	if err := store.PutDomain(ctx, s.Store, d); err != nil {
		return nil, internalErr(err)
	}
	ms, _ := store.ListMachines(ctx, s.Store)
	ips := ingress.IngressIPs(ms)
	defaultHost := ""
	if d.Generated {
		defaultHost = d.Hostname
	} else {
		for _, row := range existingOrEmpty(ctx, s.Store, svc.Id) {
			if row.Generated {
				defaultHost = row.Hostname
				break
			}
		}
	}
	return connect.NewResponse(&v1.AddDomainResponse{Domain: d, DnsHints: ingress.DNSHints(d.Hostname, ips, defaultHost)}), nil
}

func existingOrEmpty(ctx context.Context, st store.Store, serviceID string) []*v1.Domain {
	ds, _ := store.ListDomains(ctx, st, serviceID)
	return ds
}

func (s *serviceSvc) hostOptions(ctx context.Context) (ingress.HostOptions, error) {
	o := ingress.HostOptions{
		Dev: os.Getenv("AX_DEV") == "1" || strings.EqualFold(os.Getenv("AX_DEV"), "true"),
	}
	if cl, err := store.GetCluster(ctx, s.Store); err == nil {
		o.ClusterDomain = cl.Domain
	}
	ms, err := store.ListMachines(ctx, s.Store)
	if err != nil {
		return o, err
	}
	if ips := ingress.IngressIPs(ms); len(ips) > 0 {
		o.PublicIP = ips[0]
	}
	return o, nil
}

func (s *serviceSvc) RemoveDomain(ctx context.Context, req *connect.Request[v1.RemoveDomainRequest]) (*connect.Response[v1.RemoveDomainResponse], error) {
	if err := s.Store.Delete(ctx, store.TableDomains, req.Msg.DomainId); err != nil {
		return nil, internalErr(err)
	}
	return connect.NewResponse(&v1.RemoveDomainResponse{}), nil
}

// ---- exec

func (s *serviceSvc) pickReplica(ctx context.Context, serviceID, replicaID string) (*v1.Replica, error) {
	if replicaID != "" {
		r, err := store.GetReplica(ctx, s.Store, replicaID)
		if err != nil {
			return nil, errNotFound("replica", replicaID)
		}
		return r, nil
	}
	reps, _ := store.ListReplicas(ctx, s.Store, map[string]string{"service_id": serviceID})
	var best *v1.Replica
	for _, r := range reps {
		if r.DesiredRunning && r.State == v1.ReplicaState_REPLICA_STATE_RUNNING {
			// prefer a local replica
			if s.State != nil && r.MachineId == s.State.MachineID {
				return r, nil
			}
			if best == nil {
				best = r
			}
		}
	}
	if best == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("no running replica"))
	}
	return best, nil
}

func (s *serviceSvc) Exec(ctx context.Context, stream *connect.BidiStream[v1.ExecRequest, v1.ExecResponse]) error {
	first, err := stream.Receive()
	if err != nil {
		return err
	}
	start := first.GetStart()
	if start == nil {
		return errInvalid("first message must be start")
	}
	rep, err := s.pickReplica(ctx, start.ServiceId, start.ReplicaId)
	if err != nil {
		return err
	}
	start.ReplicaId = rep.Id
	if s.State != nil && rep.MachineId != s.State.MachineID {
		return s.proxyExec(ctx, rep, first, stream)
	}
	if s.Runtime == nil {
		return connect.NewError(connect.CodeUnavailable, errors.New("no container runtime on this node"))
	}
	if len(start.Command) == 0 {
		return errInvalid("command is required")
	}
	var env []string
	for k, v := range start.Env {
		env = append(env, k+"="+v)
	}
	sess, err := s.Runtime.Exec(ctx, rep.ContainerId, runtime.ExecOptions{Command: start.Command, Env: env, TTY: start.Tty,
		Stdin: start.Stdin, Rows: int(start.Rows), Cols: int(start.Cols)})
	if err != nil {
		return internalErr(err)
	}
	defer sess.Close()

	var sendMu sync.Mutex
	send := func(m *v1.ExecResponse) error { sendMu.Lock(); defer sendMu.Unlock(); return stream.Send(m) }
	var wg sync.WaitGroup
	pump := func(r io.Reader, stderr bool) {
		defer wg.Done()
		buf := make([]byte, 32<<10)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				b := append([]byte(nil), buf[:n]...)
				m := &v1.ExecResponse{Msg: &v1.ExecResponse_Stdout{Stdout: b}}
				if stderr {
					m = &v1.ExecResponse{Msg: &v1.ExecResponse_Stderr{Stderr: b}}
				}
				if send(m) != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}
	wg.Add(1)
	go pump(sess.Stdout(), false)
	if e := sess.Stderr(); e != nil {
		wg.Add(1)
		go pump(e, true)
	}
	go func() { // client -> container
		for {
			m, err := stream.Receive()
			if err != nil {
				if w := sess.Stdin(); w != nil {
					w.Close()
				}
				return
			}
			switch x := m.Msg.(type) {
			case *v1.ExecRequest_Stdin:
				if w := sess.Stdin(); w != nil {
					_, _ = w.Write(x.Stdin)
				}
			case *v1.ExecRequest_Resize:
				_ = sess.Resize(ctx, int(x.Resize.Rows), int(x.Resize.Cols))
			}
		}
	}()
	wg.Wait()
	code, err := sess.Wait(ctx)
	if err != nil {
		return internalErr(err)
	}
	return send(&v1.ExecResponse{Msg: &v1.ExecResponse_ExitCode{ExitCode: int32(code)}})
}

func (s *serviceSvc) proxyExec(ctx context.Context, rep *v1.Replica, first *v1.ExecRequest, in *connect.BidiStream[v1.ExecRequest, v1.ExecResponse]) error {
	m, err := store.GetMachine(ctx, s.Store, rep.MachineId)
	if err != nil {
		return errNotFound("machine", rep.MachineId)
	}
	if in.RequestHeader().Get(proxiedHeader) != "" {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("replica is not on this machine"))
	}
	c := aetherixv1connect.NewServiceServiceClient(s.HTTPClient, s.PeerURL(m.MeshIp), connect.WithGRPC())
	out := c.Exec(ctx)
	out.RequestHeader().Set(proxiedHeader, "1")
	if err := out.Send(first); err != nil {
		return internalErr(err)
	}
	go func() {
		for {
			m, err := in.Receive()
			if err != nil {
				_ = out.CloseRequest()
				return
			}
			if out.Send(m) != nil {
				return
			}
		}
	}()
	for {
		m, err := out.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if err := in.Send(m); err != nil {
			return err
		}
	}
}
