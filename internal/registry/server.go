//go:build !(darwin && cgo)

package registry

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/distribution/distribution/v3/configuration"
	"github.com/distribution/distribution/v3/registry/handlers"
	_ "github.com/distribution/distribution/v3/registry/storage/driver/filesystem" // storage driver
)

// Handler builds the registry HTTP handler.
func (s *Server) Handler(ctx context.Context) (http.Handler, error) {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return nil, err
	}
	cfg := &configuration.Configuration{}
	cfg.Version = "0.1"
	cfg.Storage = configuration.Storage{
		"filesystem": configuration.Parameters{"rootdirectory": s.Dir},
		"delete":     configuration.Parameters{"enabled": true},
		"cache":      configuration.Parameters{"blobdescriptor": "inmemory"},
	}
	cfg.HTTP.Addr = s.Addrs[0]
	cfg.Log.Level = "warn"
	cfg.HTTP.Secret = "aetherix-registry" // only signs upload state; registry is mesh-internal
	return handlers.NewApp(ctx, cfg), nil
}

// Run serves the registry until ctx is done.
func (s *Server) Run(ctx context.Context) error {
	h, err := s.Handler(ctx)
	if err != nil {
		return err
	}
	errc := make(chan error, len(s.Addrs))
	var srvs []*http.Server
	for _, a := range s.Addrs {
		ln, err := net.Listen("tcp", a)
		if err != nil {
			return fmt.Errorf("registry: %w", err)
		}
		srv := &http.Server{Handler: h}
		srvs = append(srvs, srv)
		go func() { errc <- srv.Serve(ln) }()
	}
	select {
	case <-ctx.Done():
	case err := <-errc:
		for _, s := range srvs {
			s.Close()
		}
		return err
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, s := range srvs {
		_ = s.Shutdown(sctx)
	}
	return nil
}
