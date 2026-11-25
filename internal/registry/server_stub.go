//go:build darwin && cgo

package registry

import (
	"context"
	"errors"
	"net/http"
)

// errUnsupported: the distribution registry pulls in prometheus, whose cgo
// darwin file breaks external linking on some macOS SDKs. The daemon runs on
// Linux (also inside `ax dev` node containers) where the real implementation
// is used; build with CGO_ENABLED=0 on macOS to get it too.
var errUnsupported = errors.New("registry: embedded registry unavailable in darwin+cgo builds (use CGO_ENABLED=0)")

// Handler is unavailable in this build configuration.
func (s *Server) Handler(ctx context.Context) (http.Handler, error) { return nil, errUnsupported }

// Run is unavailable in this build configuration.
func (s *Server) Run(ctx context.Context) error { return errUnsupported }
