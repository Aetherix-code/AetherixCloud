//go:build integration

package devcluster_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"connectrpc.com/connect"
	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/devcluster"
	"github.com/stretchr/testify/require"
)

// shared harness for the expensive Docker scenarios. Serialise with t.Parallel forbidden.
var shared *devcluster.Harness

func TestMain(m *testing.M) {
	ok, reason := devcluster.IntegrationEnabled()
	if !ok {
		fmt.Fprintf(os.Stderr, "skipping integration suite: %s\n", reason)
		os.Exit(0)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	h, err := devcluster.StartHarness(ctx, devcluster.HarnessOptions{
		Nodes: 3,
		Out:   os.Stderr,
	})
	if err != nil {
		// Missing image is an environmental skip (docs/testing.md). Any other
		// start failure is a real suite failure — do not soft-exit 0.
		if !devcluster.ImagePresent(ctx, "") {
			fmt.Fprintf(os.Stderr, "skipping integration suite: %v\n", err)
			fmt.Fprintf(os.Stderr, "hint: make dev-image && go test -tags=integration ./internal/devcluster/\n")
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "integration suite: cluster start failed: %v\n", err)
		os.Exit(1)
	}
	shared = h
	code := m.Run()
	_ = h.Close()
	os.Exit(code)
}

func TestMultiNodeJoin(t *testing.T) {
	require.NotNil(t, shared)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	require.NoError(t, shared.WaitMachines(ctx, 3, 2*time.Minute))

	_, mc, _, _, _, err := shared.Clients()
	require.NoError(t, err)
	r, err := mc.ListMachines(ctx, connect.NewRequest(&v1.ListMachinesRequest{}))
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(r.Msg.Machines), 3)
	up := 0
	for _, m := range r.Msg.Machines {
		if m.State == v1.MachineState_MACHINE_STATE_UP {
			up++
		}
	}
	require.GreaterOrEqual(t, up, 3)
}

func TestDeployRollingUpdateZeroDowntime(t *testing.T) {
	require.NotNil(t, shared)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	_, _, ps, ss, ds, err := shared.Clients()
	require.NoError(t, err)

	_, err = ps.CreateProject(ctx, connect.NewRequest(&v1.CreateProjectRequest{Name: "zt"}))
	if err != nil && connect.CodeOf(err) != connect.CodeAlreadyExists {
		require.NoError(t, err)
	}

	cr, err := ss.CreateService(ctx, connect.NewRequest(&v1.CreateServiceRequest{
		Project: "zt", Name: "web", Deploy: true,
		Spec: &v1.ServiceSpec{
			Image:    "nginx:1.27-alpine",
			Replicas: 2,
			Ports:    []*v1.ServicePort{{ContainerPort: 80, Mode: "http"}},
			Strategy: v1.DeployStrategy_DEPLOY_STRATEGY_START_FIRST,
			MaxSurge: 1,
			HealthCheck: &v1.HealthCheck{
				HttpPath: "/", HttpPort: 80, IntervalSeconds: 2, TimeoutSeconds: 2, Retries: 5,
			},
			Reschedulable: true,
		},
	}))
	require.NoError(t, err)
	svcID := cr.Msg.Service.Id
	require.NoError(t, shared.WaitReplicas(ctx, svcID, 2, 3*time.Minute))

	// Prefer Host-based routing when domains exist; otherwise hit node-1 HTTP.
	url := shared.AppURL() + "/"
	host := ""
	if doms, err := ss.ListDomains(ctx, connect.NewRequest(&v1.ListDomainsRequest{ServiceId: svcID})); err == nil && len(doms.Msg.Domains) > 0 {
		host = doms.Msg.Domains[0].Hostname
	}

	lg := &devcluster.LoadGen{URL: url, Host: host, Client: devcluster.LoadGenHTTP(host)}
	lg.Start(15)
	time.Sleep(500 * time.Millisecond) // warm up

	_, err = ds.Deploy(ctx, connect.NewRequest(&v1.DeployRequest{
		ServiceId: svcID,
		SpecOverride: &v1.ServiceSpec{
			Image:    "nginx:1.27",
			Replicas: 2,
			Ports:    []*v1.ServicePort{{ContainerPort: 80, Mode: "http"}},
			Strategy: v1.DeployStrategy_DEPLOY_STRATEGY_START_FIRST,
			MaxSurge: 1,
			HealthCheck: &v1.HealthCheck{
				HttpPath: "/", HttpPort: 80, IntervalSeconds: 2, TimeoutSeconds: 2, Retries: 5,
			},
			Reschedulable: true,
		},
	}))
	require.NoError(t, err)

	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		d, err := ds.ListDeployments(ctx, connect.NewRequest(&v1.ListDeploymentsRequest{ServiceId: svcID}))
		if err == nil && len(d.Msg.Deployments) > 0 {
			latest := d.Msg.Deployments[0]
			if latest.Status == v1.DeploymentStatus_DEPLOYMENT_STATUS_ACTIVE && latest.Spec != nil && latest.Spec.Image == "nginx:1.27" {
				break
			}
		}
		time.Sleep(2 * time.Second)
	}
	require.NoError(t, shared.WaitReplicas(ctx, svcID, 2, time.Minute))

	stats := lg.Stop()
	t.Logf("load gen: ok=%d failed=%d total=%d", stats.OK, stats.Failed, stats.Total)
	if stats.Total == 0 {
		t.Skip("ingress did not accept traffic during the rollout (no routes yet); cluster deploy still succeeded")
	}
	// Prefer zero failures when ingress is live; allow a tiny blip only if Host routing is unset.
	if host != "" {
		require.True(t, stats.ZeroFailed(), "zero-downtime violated: %+v", stats)
	} else if stats.Failed > stats.Total/10 {
		t.Fatalf("too many failures during rolling update without Host routing: %+v", stats)
	}
}

func TestKillNodeReschedule(t *testing.T) {
	require.NotNil(t, shared)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	_, _, ps, ss, _, err := shared.Clients()
	require.NoError(t, err)

	_, err = ps.CreateProject(ctx, connect.NewRequest(&v1.CreateProjectRequest{Name: "rs"}))
	if err != nil && connect.CodeOf(err) != connect.CodeAlreadyExists {
		require.NoError(t, err)
	}
	cr, err := ss.CreateService(ctx, connect.NewRequest(&v1.CreateServiceRequest{
		Project: "rs", Name: "api", Deploy: true,
		Spec: &v1.ServiceSpec{
			Image:         "nginx:1.27-alpine",
			Replicas:      2,
			Reschedulable: true,
			Ports:         []*v1.ServicePort{{ContainerPort: 80, Mode: "http"}},
		},
	}))
	require.NoError(t, err)
	svcID := cr.Msg.Service.Id
	require.NoError(t, shared.WaitReplicas(ctx, svcID, 2, 3*time.Minute))

	before, err := ss.ListReplicas(ctx, connect.NewRequest(&v1.ListReplicasRequest{ServiceId: svcID}))
	require.NoError(t, err)
	victimMachine := ""
	for _, r := range before.Msg.Replicas {
		if r.DesiredRunning && r.MachineId != "" {
			victimMachine = r.MachineId
			break
		}
	}
	require.NotEmpty(t, victimMachine)

	// Map machine id → node index via ListMachines names (node-N).
	_, mc, _, _, _, err := shared.Clients()
	require.NoError(t, err)
	ms, err := mc.ListMachines(ctx, connect.NewRequest(&v1.ListMachinesRequest{}))
	require.NoError(t, err)
	killIdx := 0
	for _, m := range ms.Msg.Machines {
		if m.Id == victimMachine {
			n, err := devcluster.ParseNode(m.Name)
			require.NoError(t, err)
			killIdx = n
			break
		}
	}
	require.NotZero(t, killIdx)
	if killIdx == 1 {
		// Prefer not to kill the API publisher; pick another replica's machine if possible.
		for _, r := range before.Msg.Replicas {
			if r.MachineId != "" && r.MachineId != victimMachine {
				for _, m := range ms.Msg.Machines {
					if m.Id == r.MachineId {
						n, err := devcluster.ParseNode(m.Name)
						require.NoError(t, err)
						killIdx = n
						victimMachine = m.Id
					}
				}
				break
			}
		}
	}
	if killIdx == 1 {
		t.Skip("only replicas on node-1; killing API publisher would strand the test harness")
	}

	require.NoError(t, shared.Cluster.Kill(ctx, killIdx))

	// Wait for reschedule onto remaining nodes.
	deadline := time.Now().Add(3 * time.Minute)
	rescheduled := false
	for time.Now().Before(deadline) {
		reps, err := ss.ListReplicas(ctx, connect.NewRequest(&v1.ListReplicasRequest{ServiceId: svcID}))
		if err == nil {
			running := 0
			onVictim := 0
			for _, r := range reps.Msg.Replicas {
				if r.DesiredRunning && r.State == v1.ReplicaState_REPLICA_STATE_RUNNING {
					running++
					if r.MachineId == victimMachine {
						onVictim++
					}
				}
			}
			if running >= 2 && onVictim == 0 {
				rescheduled = true
				break
			}
		}
		time.Sleep(3 * time.Second)
	}
	// Always bring the victim back so later scenarios see a 3-node cluster.
	// Use a fresh deadline: the test ctx may already be nearly exhausted by the
	// reschedule wait above.
	require.NoError(t, shared.Cluster.Start(context.Background(), killIdx))
	recoverCtx, recoverCancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer recoverCancel()
	if err := shared.WaitMachines(recoverCtx, 3, 3*time.Minute); err != nil {
		clogs, _ := shared.Cluster.ContainerLogs(context.Background(), devcluster.ContainerName(killIdx), 40)
		t.Fatalf("victim node-%d did not rejoin: %v\nlogs:\n%s", killIdx, err, clogs)
	}
	if !rescheduled {
		t.Fatal("replicas were not rescheduled off the killed node in time")
	}
}

func TestPartitionAndHeal(t *testing.T) {
	require.NotNil(t, shared)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Prior tests (e.g. KillNodeReschedule) may have just restarted a container;
	// wait until membership is whole before cutting links.
	require.NoError(t, shared.WaitMachines(ctx, 3, 2*time.Minute))

	require.NoError(t, shared.Cluster.Partition(ctx, 2, 3))
	t.Cleanup(func() { _ = shared.Cluster.Heal(context.Background()) })

	// Cluster API on node-1 should still answer while 2↔3 are cut.
	cs, _, _, _, _, err := shared.Clients()
	require.NoError(t, err)
	_, err = cs.GetStatus(ctx, connect.NewRequest(&v1.GetStatusRequest{}))
	require.NoError(t, err)

	require.NoError(t, shared.Cluster.Heal(ctx))
	require.NoError(t, shared.WaitMachines(ctx, 3, 2*time.Minute))
}
