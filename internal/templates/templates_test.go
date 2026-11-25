package templates_test

import (
	"testing"

	"github.com/aetherix/aetherixcloud/internal/templates"
	"github.com/stretchr/testify/require"
)

func TestCatalog(t *testing.T) {
	require.Equal(t, []string{"minio", "mysql", "postgres", "redis"}, templates.Names())
	_, ok := templates.Get("nope")
	require.False(t, ok)
	pg, ok := templates.Get("Postgres")
	require.True(t, ok)
	spec := templates.Spec(pg, "db")
	require.Equal(t, "postgres:16", spec.Image)
	require.Equal(t, "db-data", spec.Volumes[0].Source)
	require.Equal(t, int64(10<<30), spec.Volumes[0].SizeBytes)
	require.Contains(t, pg.Secrets, "POSTGRES_PASSWORD")
	require.NotEmpty(t, templates.RandomSecret())
}
