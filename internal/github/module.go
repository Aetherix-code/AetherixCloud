package github

import (
	"net/http"

	"connectrpc.com/connect"
	"github.com/aetherix/aetherixcloud/gen/aetherix/v1/aetherixv1connect"
)

// ServicePath is the mount path of GitHubService.
var ServicePath = func() string {
	p, _ := aetherixv1connect.NewGitHubServiceHandler(aetherixv1connect.UnimplementedGitHubServiceHandler{})
	return p
}()

// Paths lists every path Handlers serves (known before the module exists, so
// the daemon hook can reserve them).
func Paths() []string { return []string{WebhookPath, ManifestCallbackPath, ServicePath} }

// Handlers returns the path -> handler map. The webhook (HMAC) and manifest
// callback (single-use state) authenticate themselves; wrap the GitHubService
// entry with the node's API authentication (guard) before mounting.
func (m *Module) Handlers(guard func(http.Handler) http.Handler, opts ...connect.HandlerOption) map[string]http.Handler {
	if guard == nil {
		guard = func(h http.Handler) http.Handler { return h }
	}
	path, h := aetherixv1connect.NewGitHubServiceHandler(&Service{M: m}, opts...)
	return map[string]http.Handler{
		WebhookPath:          m.WebhookHandler(),
		ManifestCallbackPath: http.HandlerFunc(m.manifestCallback),
		path:                 guard(h),
	}
}
