package devcluster

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLoadGenZeroFailed(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)

	g := &LoadGen{URL: srv.URL, Client: srv.Client()}
	g.Start(20)
	time.Sleep(300 * time.Millisecond)
	stats := g.Stop()
	require.True(t, stats.Total > 0)
	require.True(t, stats.ZeroFailed(), "ok=%d failed=%d total=%d", stats.OK, stats.Failed, stats.Total)
	require.Equal(t, stats.Total, stats.OK)
	require.Greater(t, hits.Load(), int64(0))
}

func TestLoadGenCountsFailures(t *testing.T) {
	g := &LoadGen{URL: "http://127.0.0.1:1", Client: &http.Client{Timeout: 50 * time.Millisecond}}
	g.Start(10)
	time.Sleep(200 * time.Millisecond)
	stats := g.Stop()
	require.True(t, stats.Failed > 0)
	require.False(t, stats.ZeroFailed())
}

func TestHarnessOptionsApplyEnv(t *testing.T) {
	o := HarnessOptions{}
	o.applyEnv()
	require.Equal(t, 3, o.Nodes)
	require.Equal(t, DefaultImage, o.Image)
	require.Equal(t, 18443, o.APIHostPort)
	require.NotEmpty(t, o.RepoDir)
}

func TestLoadStatsZeroFailed(t *testing.T) {
	require.False(t, LoadStats{}.ZeroFailed())
	require.False(t, LoadStats{Total: 10, OK: 9, Failed: 1}.ZeroFailed())
	require.True(t, LoadStats{Total: 10, OK: 10}.ZeroFailed())
}
