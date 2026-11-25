package api

import (
	"fmt"
	"github.com/aetherix/aetherixcloud/internal/ids"
	"google.golang.org/protobuf/reflect/protoreflect"
	"net"
	"net/http"

	"connectrpc.com/connect"
)

func errorf(format string, a ...any) error { return fmt.Errorf(format, a...) }

// remoteIP extracts the caller IP from a connect request.
func remoteIP(r interface{ Peer() connect.Peer }) string {
	addr := r.Peer().Addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

var _ = http.StatusOK

func newID() string { return ids.New() }

func protoName(s string) protoreflect.Name { return protoreflect.Name(s) }
