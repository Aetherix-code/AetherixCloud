//go:build !(darwin && cgo)

package registry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHandlerServesV2(t *testing.T) {
	s := &Server{Dir: t.TempDir(), Addrs: []string{"127.0.0.1:0"}}
	h, err := s.Handler(context.Background())
	require.NoError(t, err)
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v2/")
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
}
