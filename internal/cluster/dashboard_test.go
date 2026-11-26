package cluster

import (
	"testing"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/stretchr/testify/require"
)

func TestPublicDashboardURL(t *testing.T) {
	require.Equal(t, "", PublicDashboardURL(nil))
	require.Equal(t, "", PublicDashboardURL(&v1.Cluster{}))
	require.Equal(t, "https://cloud.example.com", PublicDashboardURL(&v1.Cluster{DashboardHost: "cloud.example.com"}))
	require.Equal(t, "https://cloud.example.com", PublicDashboardURL(&v1.Cluster{DashboardHost: "cloud.example.com", DashboardPort: 443}))
	require.Equal(t, "https://cloud.example.com:8443", PublicDashboardURL(&v1.Cluster{DashboardHost: "cloud.example.com", DashboardPort: 8443}))
}

func TestDashboardHostFromURL(t *testing.T) {
	h, p, err := DashboardHostFromURL("https://cloud.aetherix.io")
	require.NoError(t, err)
	require.Equal(t, "cloud.aetherix.io", h)
	require.EqualValues(t, 0, p)
	h, p, err = DashboardHostFromURL("cloud.aetherix.io:8443")
	require.NoError(t, err)
	require.Equal(t, "cloud.aetherix.io", h)
	require.EqualValues(t, 8443, p)
}
