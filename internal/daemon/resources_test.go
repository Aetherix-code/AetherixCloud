package daemon

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCapacityAndUsage(t *testing.T) {
	c := capacity()
	require.NotNil(t, c)
	require.Greater(t, c.CpuMillis, int64(0), "cpu capacity should be at least one core")
	// Memory/disk may be unavailable in some sandboxes; when present they must be sane.
	if c.MemoryBytes > 0 {
		require.Greater(t, c.MemoryBytes, int64(1<<20))
	}
	if c.DiskBytes > 0 {
		require.Greater(t, c.DiskBytes, int64(1<<20))
	}

	_ = usage() // prime CPU delta
	u := usage()
	require.NotNil(t, u)
	if c.CpuMillis > 0 {
		require.LessOrEqual(t, u.CpuMillis, c.CpuMillis)
	}
	if c.MemoryBytes > 0 && u.MemoryBytes > 0 {
		require.LessOrEqual(t, u.MemoryBytes, c.MemoryBytes)
	}
	if c.DiskBytes > 0 && u.DiskBytes > 0 {
		require.LessOrEqual(t, u.DiskBytes, c.DiskBytes)
	}
}
