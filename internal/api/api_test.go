package api_test

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/gen/aetherix/v1/aetherixv1connect"
	"github.com/aetherix/aetherixcloud/internal/api"
	"github.com/aetherix/aetherixcloud/internal/auth"
	"github.com/aetherix/aetherixcloud/internal/cluster"
	"github.com/aetherix/aetherixcloud/internal/lease"
	"github.com/aetherix/aetherixcloud/internal/pki"
	"github.com/aetherix/aetherixcloud/internal/reconciler"
	"github.com/aetherix/aetherixcloud/internal/runtime"
	"github.com/aetherix/aetherixcloud/internal/store"
	"github.com/stretchr/testify/require"
)

type fixture struct {
	st    store.Store
	state *cluster.NodeState
	admin string
	rt    *runtime.Fake
	srv   *httptest.Server
	mesh  *httptest.Server
	rec   *reconciler.Reconciler
	ctl   *reconciler.Controller
}

func newFixture(t *testing.T) *fixture {
	ctx := context.Background()
	st := store.NewMemory()
	state, admin, err := cluster.Init(cluster.InitOptions{ClusterName: "t", MachineName: "node-1", Advertise: []string{"127.0.0.1"}})
	require.NoError(t, err)
	require.NoError(t, state.ApplyBootstrap(ctx, st))
	require.Nil(t, state.Bootstrap)

	rt := runtime.NewFake()
	rt.AutoHealthy = true
	lm := &lease.Manager{Store: st, Self: state.MachineID, Now: time.Now} // no settle in tests
	rec := reconciler.New(reconciler.Config{Store: st, Runtime: rt, MachineID: state.MachineID, ClusterID: state.ClusterID})
	deps := api.Deps{Store: st, Leases: lm, State: state, Runtime: rt, OnChange: rec.Trigger}

	ca, err := pki.LoadCA([]byte(state.CACertPEM), []byte(state.CAKeyPEM))
	require.NoError(t, err)
	cert, err := ca.Issue("node-1", []string{"127.0.0.1", "localhost"})
	require.NoError(t, err)
	srv := httptest.NewUnstartedServer(api.PublicHandler(deps))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	mesh := httptest.NewServer(api.Handler(deps))
	t.Cleanup(mesh.Close)
	return &fixture{st: st, state: state, admin: admin, rt: rt, srv: srv, mesh: mesh, rec: rec,
		ctl: &reconciler.Controller{Store: st, Leases: lm}}
}

func (f *fixture) converge(t *testing.T) {
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		require.NoError(t, f.ctl.RunOnce(ctx, 1))
		require.NoError(t, f.rec.ReconcileOnce(ctx))
	}
}

func TestPublicAuth(t *testing.T) {
	f := newFixture(t)
	c := aetherixv1connect.NewClusterServiceClient(f.srv.Client(), f.srv.URL)
	_, err := c.GetCluster(context.Background(), connect.NewRequest(&v1.GetClusterRequest{}))
	require.Equal(t, connect.CodeUnauthenticated, connect.CodeOf(err))

	req := connect.NewRequest(&v1.GetClusterRequest{})
	req.Header().Set("Authorization", "Bearer "+f.admin)
	resp, err := c.GetCluster(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, "t", resp.Msg.Cluster.Name)

	req = connect.NewRequest(&v1.GetClusterRequest{})
	req.Header().Set("Authorization", "Bearer axt_wrong")
	_, err = c.GetCluster(context.Background(), req)
	require.Error(t, err)

	tok, err := auth.Verify(context.Background(), f.st, f.admin)
	require.NoError(t, err)
	require.True(t, auth.HasScope(tok, auth.ScopeDeploy))
}

func TestJoinFlow(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	mc := aetherixv1connect.NewMachineServiceClient(f.mesh.Client(), f.mesh.URL)
	tr, err := mc.CreateJoinToken(ctx, connect.NewRequest(&v1.CreateJoinTokenRequest{MachineName: "worker-a"}))
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(tr.Msg.Token, cluster.TokenPrefix))

	// Token endpoints point at https://127.0.0.1:8443; redirect to the test server.
	u, _ := url.Parse(f.srv.URL)
	tok, _ := cluster.ParseJoinToken(tr.Msg.Token)
	tok.Endpoints = []string{"https://" + u.Host}
	// The pinned CA is the cluster CA, which signed the server cert.
	st, err := cluster.Join(ctx, cluster.JoinOptions{Token: tok.Encode(), Advertise: []string{"10.1.1.1"}})
	require.NoError(t, err)
	require.Equal(t, "worker-a", st.MachineName)
	require.Equal(t, "10.210.2.0/24", st.Subnet)
	require.Equal(t, f.state.ClusterKey, st.ClusterKey)
	require.Equal(t, f.state.ClusterID, st.ClusterID)
	require.Len(t, st.Peers, 1)
	require.Equal(t, f.state.MachineID, st.Peers[0].MachineID)
	require.Equal(t, []string{"10.210.1.1:8787"}, st.StoreBootstrap)

	ms, _ := store.ListMachines(ctx, f.st)
	require.Len(t, ms, 2)

	// one-time: second use is rejected
	_, err = cluster.Join(ctx, cluster.JoinOptions{Token: tok.Encode(), Name: "other"})
	require.Error(t, err)
	require.Equal(t, connect.CodePermissionDenied, connect.CodeOf(unwrap(err)))

	// a token for a different CA cannot be used: TLS pin fails
	bad := tok
	other, _ := pki.NewCA("evil")
	bad.CAPEM = string(other.CertPEM)
	_, err = cluster.Join(ctx, cluster.JoinOptions{Token: bad.Encode()})
	require.Error(t, err)

	// second machine gets the next subnet
	tr2, _ := mc.CreateJoinToken(ctx, connect.NewRequest(&v1.CreateJoinTokenRequest{}))
	t2, _ := cluster.ParseJoinToken(tr2.Msg.Token)
	t2.Endpoints = tok.Endpoints
	st2, err := cluster.Join(ctx, cluster.JoinOptions{Token: t2.Encode()})
	require.NoError(t, err)
	require.Equal(t, "10.210.3.0/24", st2.Subnet)
	require.Equal(t, "node-3", st2.MachineName)
}

func unwrap(err error) error {
	for err != nil {
		if _, ok := err.(*connect.Error); ok {
			return err
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return err
		}
		err = u.Unwrap()
	}
	return err
}

func TestServiceLifecycle(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.mesh.Client()
	ps := aetherixv1connect.NewProjectServiceClient(c, f.mesh.URL)
	ss := aetherixv1connect.NewServiceServiceClient(c, f.mesh.URL)
	ds := aetherixv1connect.NewDeployServiceClient(c, f.mesh.URL)

	pr, err := ps.CreateProject(ctx, connect.NewRequest(&v1.CreateProjectRequest{Name: "shop"}))
	require.NoError(t, err)
	_, err = ps.CreateProject(ctx, connect.NewRequest(&v1.CreateProjectRequest{Name: "shop"}))
	require.Equal(t, connect.CodeAlreadyExists, connect.CodeOf(err))

	cr, err := ss.CreateService(ctx, connect.NewRequest(&v1.CreateServiceRequest{
		Project: "shop", Name: "web", Deploy: true, Spec: &v1.ServiceSpec{Image: "nginx:1", Replicas: 2}}))
	require.NoError(t, err)
	svcID := cr.Msg.Service.Id
	require.NotNil(t, cr.Msg.Deployment)

	// a service without image cannot be deployed
	_, err = ss.CreateService(ctx, connect.NewRequest(&v1.CreateServiceRequest{Project: "shop", Name: "noimg", Deploy: true}))
	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err))

	f.converge(t)
	gr, err := ss.GetService(ctx, connect.NewRequest(&v1.GetServiceRequest{Service: "web", Project: "shop"}))
	require.NoError(t, err)
	require.Len(t, gr.Msg.Replicas, 2)
	for _, r := range gr.Msg.Replicas {
		require.Equal(t, v1.ReplicaState_REPLICA_STATE_RUNNING, r.State)
	}

	// variables: secrets are sealed + redacted, and reach the container env
	envID := pr.Msg.Environment.Id
	_, err = ps.SetVariables(ctx, connect.NewRequest(&v1.SetVariablesRequest{EnvironmentId: envID, ServiceId: svcID,
		Variables: []*v1.Variable{{Key: "A", Value: "1"}, {Key: "PASSWORD", Value: "hunter2", Secret: true}}}))
	require.NoError(t, err)
	lv, _ := ps.ListVariables(ctx, connect.NewRequest(&v1.ListVariablesRequest{EnvironmentId: envID, ServiceId: svcID}))
	for _, v := range lv.Msg.Variables {
		if v.Key == "PASSWORD" {
			require.True(t, v.Redacted)
			require.Empty(t, v.Value)
		}
	}
	raw, _ := store.ListProto[v1.Variable](ctx, f.st, store.TableVariables, store.Query{})
	for _, v := range raw {
		require.NotContains(t, v.Value, "hunter2")
	}

	// scale
	_, err = ss.ScaleService(ctx, connect.NewRequest(&v1.ScaleServiceRequest{ServiceId: svcID, Replicas: 3}))
	require.NoError(t, err)
	f.converge(t)
	lr, _ := ss.ListReplicas(ctx, connect.NewRequest(&v1.ListReplicasRequest{ServiceId: svcID}))
	n := 0
	for _, r := range lr.Msg.Replicas {
		if r.DesiredRunning {
			n++
		}
	}
	require.Equal(t, 3, n)

	// redeploy then rollback
	d2, err := ds.Deploy(ctx, connect.NewRequest(&v1.DeployRequest{ServiceId: svcID, SpecOverride: &v1.ServiceSpec{Image: "nginx:2", Replicas: 1}}))
	require.NoError(t, err)
	f.converge(t)
	got, _ := ds.GetDeployment(ctx, connect.NewRequest(&v1.GetDeploymentRequest{DeploymentId: d2.Msg.Deployment.Id}))
	require.Equal(t, v1.DeploymentStatus_DEPLOYMENT_STATUS_ACTIVE, got.Msg.Deployment.Status)
	rb, err := ds.Rollback(ctx, connect.NewRequest(&v1.RollbackRequest{ServiceId: svcID}))
	require.NoError(t, err)
	require.Equal(t, "nginx:1", rb.Msg.Deployment.Spec.Image)

	// delete requires confirm and stops containers
	_, err = ss.DeleteService(ctx, connect.NewRequest(&v1.DeleteServiceRequest{ServiceId: svcID}))
	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err))
	_, err = ss.DeleteService(ctx, connect.NewRequest(&v1.DeleteServiceRequest{ServiceId: svcID, Confirm: true}))
	require.NoError(t, err)
	f.converge(t)
	cs, _ := f.rt.ListContainers(ctx, map[string]string{runtime.LabelManaged: "true"})
	for _, c := range cs {
		require.False(t, c.Running())
	}
}

func TestDeployCompose(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ds := aetherixv1connect.NewDeployServiceClient(f.mesh.Client(), f.mesh.URL)
	yaml := "name: demo\nservices:\n  db:\n    image: postgres:16\n  web:\n    image: nginx\n    depends_on: [db]\n    deploy: {replicas: 2}\n"
	r, err := ds.DeployCompose(ctx, connect.NewRequest(&v1.DeployComposeRequest{ComposeYaml: yaml}))
	require.NoError(t, err)
	require.Len(t, r.Msg.Services, 2)
	require.Len(t, r.Msg.Deployments, 2)
	require.Equal(t, "db", r.Msg.Services[0].Name)
	f.converge(t)

	// unchanged re-deploy is a no-op
	r, err = ds.DeployCompose(ctx, connect.NewRequest(&v1.DeployComposeRequest{ComposeYaml: yaml}))
	require.NoError(t, err)
	require.Empty(t, r.Msg.Deployments)

	// changed image redeploys; prune removes services missing from the file
	yaml2 := "name: demo\nservices:\n  web:\n    image: nginx:2\n    deploy: {replicas: 2}\n"
	r, err = ds.DeployCompose(ctx, connect.NewRequest(&v1.DeployComposeRequest{ComposeYaml: yaml2, Prune: true}))
	require.NoError(t, err)
	require.Len(t, r.Msg.Deployments, 1)
	require.Equal(t, []string{"db"}, r.Msg.Removed)

	// build: sections need the (future) build pipeline
	_, err = ds.DeployCompose(ctx, connect.NewRequest(&v1.DeployComposeRequest{Project: "x", ComposeYaml: "services:\n  a:\n    build: .\n"}))
	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err))
	_, err = ds.DeployCompose(ctx, connect.NewRequest(&v1.DeployComposeRequest{ComposeYaml: "not: [valid"}))
	require.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))
}

func TestStatusAndEvents(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	cc := aetherixv1connect.NewClusterServiceClient(f.mesh.Client(), f.mesh.URL)
	r, err := cc.GetStatus(ctx, connect.NewRequest(&v1.GetStatusRequest{}))
	require.NoError(t, err)
	require.Equal(t, int32(1), r.Msg.MachinesTotal)
	require.Equal(t, int32(1), r.Msg.MachinesUp)

	st, err := cc.Backup(ctx, connect.NewRequest(&v1.BackupRequest{}))
	require.NoError(t, err)
	var snap []byte
	for st.Receive() {
		snap = append(snap, st.Msg().Data...)
	}
	require.NoError(t, st.Err())
	require.Greater(t, len(snap), 100)
	require.Contains(t, string(snap), `"identity"`)

	// Restore into the same fixture requires --force (not fresh: has bootstrap machine only is fresh actually).
	// Add a project so non-force fails, then force succeeds.
	require.NoError(t, store.PutProject(ctx, f.st, &v1.Project{Id: "p-live", Name: "live"}))
	stream := cc.Restore(ctx)
	require.NoError(t, stream.Send(&v1.RestoreChunk{Data: snap, Force: false}))
	_, err = stream.CloseAndReceive()
	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err))

	stream = cc.Restore(ctx)
	require.NoError(t, stream.Send(&v1.RestoreChunk{Data: snap, Force: true}))
	resp, err := stream.CloseAndReceive()
	require.NoError(t, err)
	require.Greater(t, resp.Msg.Rows, int32(0))

	// unimplemented services answer cleanly
	as := aetherixv1connect.NewGitHubServiceClient(f.mesh.Client(), f.mesh.URL)
	_, err = as.GetApp(ctx, connect.NewRequest(&v1.GetAppRequest{}))
	require.Equal(t, connect.CodeUnimplemented, connect.CodeOf(err))
	_ = http.StatusOK
}

func TestGitHubCreateAndDeployEnqueueBuild(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	state, _, err := cluster.Init(cluster.InitOptions{ClusterName: "t", MachineName: "node-1", Advertise: []string{"127.0.0.1"}})
	require.NoError(t, err)
	require.NoError(t, state.ApplyBootstrap(ctx, st))

	trig := &api.BuildTrigger{}
	var enqueued []string
	trig.Set(func(_ context.Context, serviceID, ref string) (string, error) {
		enqueued = append(enqueued, serviceID+":"+ref)
		return "bld_1", nil
	})
	deps := api.Deps{Store: st, Leases: &lease.Manager{Store: st, Self: state.MachineID, Now: time.Now}, State: state, BuildTrigger: trig}
	mesh := httptest.NewServer(api.Handler(deps))
	t.Cleanup(mesh.Close)

	ps := aetherixv1connect.NewProjectServiceClient(mesh.Client(), mesh.URL)
	ss := aetherixv1connect.NewServiceServiceClient(mesh.Client(), mesh.URL)
	ds := aetherixv1connect.NewDeployServiceClient(mesh.Client(), mesh.URL)
	_, err = ps.CreateProject(ctx, connect.NewRequest(&v1.CreateProjectRequest{Name: "app"}))
	require.NoError(t, err)

	cr, err := ss.CreateService(ctx, connect.NewRequest(&v1.CreateServiceRequest{
		Project: "app", Name: "web", Deploy: true,
		Spec: &v1.ServiceSpec{Source: &v1.ServiceSource{Github: &v1.GitHubSource{Repo: "o/r", Branch: "main"}}},
	}))
	require.NoError(t, err)
	require.Nil(t, cr.Msg.Deployment)
	require.Equal(t, []string{cr.Msg.Service.Id + ":"}, enqueued)

	enqueued = nil
	_, err = ds.Deploy(ctx, connect.NewRequest(&v1.DeployRequest{ServiceId: cr.Msg.Service.Id}))
	require.NoError(t, err)
	require.Equal(t, []string{cr.Msg.Service.Id + ":"}, enqueued)

	// Redeploy with no image still builds.
	enqueued = nil
	_, err = ds.Deploy(ctx, connect.NewRequest(&v1.DeployRequest{ServiceId: cr.Msg.Service.Id, Redeploy: true}))
	require.NoError(t, err)
	require.Equal(t, []string{cr.Msg.Service.Id + ":"}, enqueued)
}

func TestUpdateMachineLastEssentialRole(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	mc := aetherixv1connect.NewMachineServiceClient(f.mesh.Client(), f.mesh.URL)

	// Bootstrap machine is the sole holder of worker/ingress/builder.
	_, err := mc.UpdateMachine(ctx, connect.NewRequest(&v1.UpdateMachineRequest{
		Machine:  f.state.MachineID,
		Roles:    []v1.MachineRole{v1.MachineRole_MACHINE_ROLE_WORKER},
		SetRoles: true,
	}))
	require.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err))
	require.ErrorContains(t, err, "ingress")

	// Turning roles on remains allowed.
	_, err = mc.UpdateMachine(ctx, connect.NewRequest(&v1.UpdateMachineRequest{
		Machine: f.state.MachineID,
		Roles: []v1.MachineRole{
			v1.MachineRole_MACHINE_ROLE_WORKER,
			v1.MachineRole_MACHINE_ROLE_INGRESS,
			v1.MachineRole_MACHINE_ROLE_BUILDER,
		},
		SetRoles: true,
	}))
	require.NoError(t, err)
}
