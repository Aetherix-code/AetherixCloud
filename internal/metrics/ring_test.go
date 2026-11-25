package metrics_test

import (
	"testing"
	"time"

	"github.com/aetherix/aetherixcloud/internal/metrics"
	"github.com/stretchr/testify/require"
)

func TestRing(t *testing.T) {
	s := metrics.NewSeries(3)
	for i := 0; i < 5; i++ {
		s.Push(metrics.Sample{Time: time.Unix(int64(i), 0), CPUPercent: float64(i)})
	}
	snap := s.Snapshot()
	require.Len(t, snap, 3)
	require.Equal(t, 2.0, snap[0].CPUPercent)
	require.Equal(t, 4.0, snap[2].CPUPercent)

	b := metrics.NewBuffer(8, 16)
	b.Record("r1", metrics.Meta{ServiceID: "svc"}, metrics.Sample{CPUPercent: 10})
	b.Record("r2", metrics.Meta{ServiceID: "other"}, metrics.Sample{CPUPercent: 20})
	views := b.ForService("svc")
	require.Len(t, views, 1)
	agg := metrics.Aggregate(views)
	require.NotEmpty(t, agg.CPUPercent)
}
