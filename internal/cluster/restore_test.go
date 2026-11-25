package cluster_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/cluster"
	"github.com/aetherix/aetherixcloud/internal/snapshot"
	"github.com/aetherix/aetherixcloud/internal/store"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestInitFromBackupAndPendingRestore(t *testing.T) {
	ctx := context.Background()
	src := store.NewMemory()
	require.NoError(t, store.PutCluster(ctx, src, &v1.Cluster{
		Id: "cid", Name: "prod", Network: "10.210.0.0/16", CaPem: "OLD_CA", CreatedAt: timestamppb.Now(),
	}))
	require.NoError(t, store.PutProject(ctx, src, &v1.Project{Id: "p1", Name: "app"}))
	require.NoError(t, store.PutMachine(ctx, src, &v1.Machine{Id: "old-m", Name: "gone"}))

	tmp, _, err := cluster.Init(cluster.InitOptions{ClusterName: "x", MachineName: "n"})
	require.NoError(t, err)
	raw, err := snapshot.Encode(ctx, src, &snapshot.Identity{
		ClusterID: "cid", ClusterKey: tmp.ClusterKey, CACertPEM: tmp.CACertPEM, CAKeyPEM: tmp.CAKeyPEM,
	})
	require.NoError(t, err)

	dir := t.TempDir()
	backup := filepath.Join(dir, "c.snapshot")
	require.NoError(t, os.WriteFile(backup, raw, 0o600))

	dataDir := filepath.Join(dir, "data")
	st, admin, err := cluster.InitFromBackup(dataDir, backup, cluster.InitOptions{
		ClusterName: "ignored", MachineName: "recovery", Advertise: []string{"127.0.0.1"},
	})
	require.NoError(t, err)
	require.NotEmpty(t, admin)
	require.Equal(t, "cid", st.ClusterID)
	require.Equal(t, tmp.ClusterKey, st.ClusterKey)
	require.Equal(t, tmp.CACertPEM, st.CACertPEM)
	require.FileExists(t, filepath.Join(dataDir, snapshot.PendingFile))

	mem := store.NewMemory()
	ok, err := st.ApplyPendingRestore(ctx, mem, dataDir)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoFileExists(t, filepath.Join(dataDir, snapshot.PendingFile))
	require.Nil(t, st.Bootstrap)

	cl, err := store.GetCluster(ctx, mem)
	require.NoError(t, err)
	require.Equal(t, "prod", cl.Name)
	require.Equal(t, st.CACertPEM, cl.CaPem)

	ms, err := store.ListMachines(ctx, mem)
	require.NoError(t, err)
	require.Len(t, ms, 1)
	require.Equal(t, st.MachineID, ms[0].Id)

	ps, err := store.ListProto[v1.Project](ctx, mem, store.TableProjects, store.Query{})
	require.NoError(t, err)
	require.Len(t, ps, 1)
}
