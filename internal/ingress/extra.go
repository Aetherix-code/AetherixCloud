package ingress

import (
	"context"
	"sync"
)

var (
	extraMu sync.Mutex
	extras  []func(ctx context.Context) []ExtraRoute
)

// RegisterExtraRoutes lets other components (dashboard, MCP, webhooks) get
// hostnames routed through the node's Caddy without touching this package.
// Call it from a daemon hook file before the daemon starts.
func RegisterExtraRoutes(f func(ctx context.Context) []ExtraRoute) {
	extraMu.Lock()
	extras = append(extras, f)
	extraMu.Unlock()
}

func registeredExtras(ctx context.Context) []ExtraRoute {
	extraMu.Lock()
	fs := append([]func(context.Context) []ExtraRoute(nil), extras...)
	extraMu.Unlock()
	var out []ExtraRoute
	for _, f := range fs {
		out = append(out, f(ctx)...)
	}
	return out
}
