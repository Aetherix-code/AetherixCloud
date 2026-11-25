package api

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// H2CHandler lets a plaintext listener serve HTTP/2 (needed for gRPC and
// bidirectional streams such as Exec).
func H2CHandler(h http.Handler) http.Handler { return h2c.NewHandler(h, &http2.Server{}) }

// H2CClient is an HTTP client that speaks cleartext HTTP/2 (node-to-node
// calls over the mesh).
func H2CClient() *http.Client {
	return &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}}
}
