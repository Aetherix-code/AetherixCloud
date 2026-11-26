package store

import (
	"context"
	"testing"
	"time"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestMigrateIdempotent(t *testing.T) {
	s, err := OpenSQLite(":memory:")
	require.NoError(t, err)
	defer s.Close()
	ctx := context.Background()
	require.NoError(t, s.Migrate(ctx))
	require.NoError(t, s.Migrate(ctx))
	for _, tb := range Tables { // all tables usable
		_, err := s.List(ctx, tb.Name, Query{})
		require.NoError(t, err, tb.Name)
	}
}

func TestPutGetListDelete(t *testing.T) {
	s := NewMemory()
	defer s.Close()
	ctx := context.Background()

	require.NoError(t, PutService(ctx, s, &v1.Service{Id: "s1", ProjectId: "p1", EnvironmentId: "e1", Name: "api", Spec: &v1.ServiceSpec{Image: "nginx", Replicas: 2}}))
	require.NoError(t, PutService(ctx, s, &v1.Service{Id: "s2", ProjectId: "p2", EnvironmentId: "e2", Name: "web"}))

	got, err := GetService(ctx, s, "s1")
	require.NoError(t, err)
	require.Equal(t, "nginx", got.Spec.Image)
	require.Equal(t, int32(2), got.Spec.Replicas)

	l, err := ListServices(ctx, s, map[string]string{"project_id": "p1"})
	require.NoError(t, err)
	require.Len(t, l, 1)
	require.Equal(t, "api", l[0].Name)

	// upsert
	require.NoError(t, PutService(ctx, s, &v1.Service{Id: "s1", ProjectId: "p1", EnvironmentId: "e1", Name: "api2"}))
	got, _ = GetService(ctx, s, "s1")
	require.Equal(t, "api2", got.Name)

	require.NoError(t, s.Delete(ctx, TableServices, "s1"))
	_, err = GetService(ctx, s, "s1")
	require.True(t, IsNotFound(err))

	_, err = s.List(ctx, TableServices, Query{Where: map[string]string{"bogus": "x"}})
	require.Error(t, err)
	_, err = s.List(ctx, "nope", Query{})
	require.Error(t, err)
}

func TestOrderLimit(t *testing.T) {
	s := NewMemory()
	defer s.Close()
	ctx := context.Background()
	for _, id := range []string{"a", "b", "c"} {
		require.NoError(t, PutDeployment(ctx, s, &v1.Deployment{Id: id, ServiceId: "svc"}))
		time.Sleep(time.Millisecond)
	}
	l, err := ListDeployments(ctx, s, "svc", 2)
	require.NoError(t, err)
	require.Len(t, l, 2)
	require.Equal(t, "c", l[0].Id)
}

// Superseding (or any later Put) an older deployment must not float it above a
// newer one: ListDeployments is creation-time newest-first, not row updated_at.
func TestListDeploymentsNewestCreatedFirst(t *testing.T) {
	s := NewMemory()
	defer s.Close()
	ctx := context.Background()
	old := &v1.Deployment{
		Id: "01older0000000000000000000", ServiceId: "svc",
		Status: v1.DeploymentStatus_DEPLOYMENT_STATUS_ACTIVE,
		CreatedAt: timestamppb.New(time.Now().Add(-24 * time.Hour)),
	}
	newDep := &v1.Deployment{
		Id: "01newer0000000000000000000", ServiceId: "svc",
		Status: v1.DeploymentStatus_DEPLOYMENT_STATUS_ACTIVE,
		CreatedAt: timestamppb.New(time.Now().Add(-3 * time.Minute)),
	}
	require.NoError(t, PutDeployment(ctx, s, old))
	time.Sleep(time.Millisecond)
	require.NoError(t, PutDeployment(ctx, s, newDep))
	time.Sleep(time.Millisecond)
	// Simulate reconciler marking the previous deployment superseded after the
	// new one became active — this bumps store updated_at on the old row.
	old.Status = v1.DeploymentStatus_DEPLOYMENT_STATUS_SUPERSEDED
	require.NoError(t, PutDeployment(ctx, s, old))

	l, err := ListDeployments(ctx, s, "svc", 10)
	require.NoError(t, err)
	require.Len(t, l, 2)
	require.Equal(t, newDep.Id, l[0].Id)
	require.Equal(t, old.Id, l[1].Id)
}

func TestWatch(t *testing.T) {
	s := NewMemory()
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := s.Watch(ctx, TableReplicas)
	require.NoError(t, err)
	require.NoError(t, PutReplica(ctx, s, &v1.Replica{Id: "r1", ServiceId: "s", MachineId: "m"}))
	select {
	case c := <-ch:
		require.Equal(t, OpPut, c.Op)
		require.Equal(t, "r1", c.ID)
	case <-time.After(2 * time.Second):
		t.Fatal("no change")
	}
	require.NoError(t, s.Delete(ctx, TableReplicas, "r1"))
	c := <-ch
	require.Equal(t, OpDelete, c.Op)
}

func TestSchemaSQL(t *testing.T) {
	sql := SchemaSQL()
	for _, tb := range Tables {
		require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS "+tb.Name)
	}
}
