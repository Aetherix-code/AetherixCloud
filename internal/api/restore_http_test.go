package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	v1 "github.com/aetherix/aetherixcloud/gen/aetherix/v1"
	"github.com/aetherix/aetherixcloud/internal/cluster"
	"github.com/aetherix/aetherixcloud/internal/snapshot"
	"github.com/aetherix/aetherixcloud/internal/store"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestRestoreHTTP(t *testing.T) {
	ctx := context.Background()
	src := store.NewMemory()
	require.NoError(t, store.PutCluster(ctx, src, &v1.Cluster{
		Id: "c1", Name: "src", CreatedAt: timestamppb.Now(),
	}))
	raw, err := snapshot.Encode(ctx, src, &snapshot.Identity{
		ClusterID:  "c1",
		ClusterKey: "0123456789abcdef0123456789abcdef",
		CACertPEM:  "cert",
		CAKeyPEM:   "key",
	})
	require.NoError(t, err)

	dst := store.NewMemory()
	require.NoError(t, store.PutCluster(ctx, dst, &v1.Cluster{Id: "new", Name: "fresh"}))
	require.NoError(t, store.PutMachine(ctx, dst, &v1.Machine{Id: "m-restore", Name: "node-1"}))

	st := &cluster.NodeState{
		MachineID:   "m-restore",
		ClusterID:   "c1",
		ClusterKey:  "0123456789abcdef0123456789abcdef",
		MachineName: "node-1",
		Subnet:      "10.210.1.0/24",
		MeshIP:      "10.210.1.1",
	}
	s := &server{Deps: Deps{Store: dst, State: st}}
	h := restoreHTTPHandler(s, nil, false)

	req := httptest.NewRequest(http.MethodPost, RestoreHTTPPath+"?force=1", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Greater(t, out["rows"].(float64), 0.0)

	c, err := store.GetCluster(ctx, dst)
	require.NoError(t, err)
	require.Equal(t, "src", c.Name)

	m, err := store.GetMachine(ctx, dst, "m-restore")
	require.NoError(t, err)
	require.Equal(t, "m-restore", m.Id)
}
