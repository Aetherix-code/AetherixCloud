package snapshot_test

import (
	"context"
	"testing"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/snapshot"
	"github.com/aetherix/aetherixcloud/internal/store"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestRoundTripSQLite(t *testing.T) {
	ctx := context.Background()
	src := store.NewMemory()

	require.NoError(t, store.PutCluster(ctx, src, &v1.Cluster{
		Id: "c1", Name: "prod", Network: "10.210.0.0/16", CaPem: "PEM", CreatedAt: timestamppb.Now(),
	}))
	require.NoError(t, store.PutMachine(ctx, src, &v1.Machine{
		Id: "m1", Name: "node-1", Subnet: "10.210.1.0/24", MeshIp: "10.210.1.1",
		State: v1.MachineState_MACHINE_STATE_UP, CreatedAt: timestamppb.Now(),
	}))
	require.NoError(t, store.PutMachine(ctx, src, &v1.Machine{
		Id: "m2", Name: "node-2", Subnet: "10.210.2.0/24", MeshIp: "10.210.2.1",
		State: v1.MachineState_MACHINE_STATE_UP, CreatedAt: timestamppb.Now(),
	}))
	require.NoError(t, store.PutProject(ctx, src, &v1.Project{Id: "p1", Name: "demo"}))
	require.NoError(t, store.PutService(ctx, src, &v1.Service{
		Id: "s1", ProjectId: "p1", EnvironmentId: "e1", Name: "web",
		Spec: &v1.ServiceSpec{Image: "nginx:latest", Replicas: 2},
	}))
	require.NoError(t, store.PutReplica(ctx, src, &v1.Replica{
		Id: "r1", ServiceId: "s1", DeploymentId: "d1", MachineId: "m2", DesiredRunning: true,
	}))
	require.NoError(t, store.PutJSON(ctx, src, store.TableLeases, "lease1",
		map[string]string{"name": "controller", "owner": "m2"}, map[string]any{"owner": "m2"}))
	require.NoError(t, store.PutJSON(ctx, src, store.TableJoinTokens, "tok1",
		map[string]string{"token_hash": "abc"}, map[string]any{"hash": "abc"}))

	ident := &snapshot.Identity{ClusterID: "c1", ClusterKey: "key", CACertPEM: "cert", CAKeyPEM: "cakey"}
	raw, err := snapshot.Encode(ctx, src, ident)
	require.NoError(t, err)

	parsed, err := snapshot.Decode(raw)
	require.NoError(t, err)
	require.Equal(t, snapshot.Version, parsed.Version)
	require.Equal(t, "key", parsed.Identity.ClusterKey)
	require.Contains(t, parsed.Tables, store.TableProjects)

	// Destination looks fresh (bootstrap-only).
	dst := store.NewMemory()
	require.NoError(t, store.PutCluster(ctx, dst, &v1.Cluster{Id: "new", Name: "fresh"}))
	require.NoError(t, store.PutMachine(ctx, dst, &v1.Machine{Id: "recovery", Name: "node-1"}))

	self := &v1.Machine{
		Id: "recovery", Name: "node-1", Subnet: "10.210.1.0/24", MeshIp: "10.210.1.1",
		PublicKey: "pk", State: v1.MachineState_MACHINE_STATE_UP, CreatedAt: timestamppb.Now(),
	}
	res, err := snapshot.Apply(ctx, dst, parsed, snapshot.ApplyOptions{
		SelfMachineID: "recovery",
		SelfMachine:   self,
	})
	require.NoError(t, err)
	require.Greater(t, res.Rows, 0)
	require.Equal(t, 2, res.MachinesRemoved) // m1 and m2 from snapshot gone; self upserted
	require.Equal(t, 1, res.ReplicasCleared)
	require.Equal(t, 1, res.LeasesCleared)
	require.Equal(t, 1, res.JoinTokensCleared)

	cl, err := store.GetCluster(ctx, dst)
	require.NoError(t, err)
	require.Equal(t, "prod", cl.Name)
	require.Equal(t, "c1", cl.Id)

	ms, err := store.ListMachines(ctx, dst)
	require.NoError(t, err)
	require.Len(t, ms, 1)
	require.Equal(t, "recovery", ms[0].Id)

	ps, err := store.ListProto[v1.Project](ctx, dst, store.TableProjects, store.Query{})
	require.NoError(t, err)
	require.Len(t, ps, 1)
	require.Equal(t, "demo", ps[0].Name)

	svcs, err := store.ListServices(ctx, dst, nil)
	require.NoError(t, err)
	require.Len(t, svcs, 1)
	require.Equal(t, "nginx:latest", svcs[0].Spec.Image)

	reps, err := store.ListReplicas(ctx, dst, nil)
	require.NoError(t, err)
	require.Empty(t, reps)
}

func TestRefuseNonFresh(t *testing.T) {
	ctx := context.Background()
	src := store.NewMemory()
	require.NoError(t, store.PutProject(ctx, src, &v1.Project{Id: "p1", Name: "x"}))
	raw, err := snapshot.Encode(ctx, src, nil)
	require.NoError(t, err)
	snap, err := snapshot.Decode(raw)
	require.NoError(t, err)

	dst := store.NewMemory()
	require.NoError(t, store.PutProject(ctx, dst, &v1.Project{Id: "p2", Name: "live"}))
	_, err = snapshot.Apply(ctx, dst, snap, snapshot.ApplyOptions{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "non-fresh")

	_, err = snapshot.Apply(ctx, dst, snap, snapshot.ApplyOptions{Force: true, SelfMachineID: "none"})
	require.NoError(t, err)
}

func TestIsFresh(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	ok, err := snapshot.IsFresh(ctx, st)
	require.NoError(t, err)
	require.True(t, ok)

	require.NoError(t, store.PutMachine(ctx, st, &v1.Machine{Id: "m1", Name: "a"}))
	ok, err = snapshot.IsFresh(ctx, st)
	require.NoError(t, err)
	require.True(t, ok)

	require.NoError(t, store.PutMachine(ctx, st, &v1.Machine{Id: "m2", Name: "b"}))
	ok, err = snapshot.IsFresh(ctx, st)
	require.NoError(t, err)
	require.False(t, ok)
}
