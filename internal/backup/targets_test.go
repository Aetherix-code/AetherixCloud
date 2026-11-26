package backup_test

import (
	"context"
	"testing"

	"github.com/aetherix/aetherixcloud/internal/backup"
	"github.com/aetherix/aetherixcloud/internal/store"
	"github.com/stretchr/testify/require"
)

func TestMigrateLegacyAndResolve(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	legacy := backup.Config{
		Enabled: true, Endpoint: "http://minio:9000", Region: "us-east-1",
		Bucket: "ax", AccessKeyID: "ak", SecretAccessKey: "sk",
		Prefix: "aetherix/", PathStyle: true, Cron: "0 3 * * *",
	}
	require.NoError(t, store.PutJSON(ctx, st, store.TableSettings, "backup/s3", nil, legacy))

	list, err := backup.ListTargets(ctx, st)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "default", list[0].ID)
	require.Equal(t, "Default", list[0].Name)
	require.Equal(t, "sk", list[0].SecretAccessKey)

	got, err := backup.ResolveTarget(ctx, st, "")
	require.NoError(t, err)
	require.Equal(t, "default", got.ID)

	second, err := backup.PutTarget(ctx, st, backup.Target{
		Name: "R2", Endpoint: "https://r2.example", Bucket: "b",
		AccessKeyID: "a", SecretAccessKey: "s", PathStyle: false,
	})
	require.NoError(t, err)
	require.NotEmpty(t, second.ID)

	list, err = backup.ListTargets(ctx, st)
	require.NoError(t, err)
	require.Len(t, list, 2)

	// Preserve secret on update with placeholder.
	second.SecretAccessKey = "••••••••"
	second.Bucket = "b2"
	saved, err := backup.PutTarget(ctx, st, second)
	require.NoError(t, err)
	require.Equal(t, "s", saved.SecretAccessKey)
	require.Equal(t, "b2", saved.Bucket)
}

func TestClusterScheduleDefaults(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	cs, err := backup.GetClusterSchedule(ctx, st)
	require.NoError(t, err)
	require.False(t, cs.Enabled)
	require.Equal(t, backup.DefaultClusterCron, cs.Cron)
	require.Equal(t, backup.DefaultClusterRetain, cs.Retain)

	cs.Enabled = true
	cs.TargetID = "default"
	cs.Cron = "30 2 * * *"
	cs.Retain = 5
	require.NoError(t, backup.PutClusterSchedule(ctx, st, cs))
	got, err := backup.GetClusterSchedule(ctx, st)
	require.NoError(t, err)
	require.True(t, got.Enabled)
	require.Equal(t, "30 2 * * *", got.Cron)
	require.Equal(t, 5, got.Retain)
}
