// Package web embeds the built dashboard SPA and serves it from every node.
//
// internal/web/dist is populated by `make web`; a minimal placeholder is
// committed so `go build` works without node.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

// apiPrefixes are request paths that belong to other handlers (API, MCP,
// OAuth, webhooks). Anything else that isn't a static file gets index.html.
var apiPrefixes = []string{"/aetherix.", "/healthz", "/api/", "/mcp", "/oauth", "/.well-known/", "/authorize", "/token", "/register"}

// Handler returns the SPA wrapper: Fallback(next) serves static assets and
// the SPA shell, and passes API traffic to next.
func Handler() func(next http.Handler) http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic(err)
	}
	return wrap(sub)
}

// spaPaths are served by the SPA even though they sit under an API prefix.
var spaPaths = map[string]bool{"/oauth/consent": true}

func isAPI(p string) bool {
	if spaPaths[strings.TrimSuffix(p, "/")] {
		return false
	}
	for _, pre := range apiPrefixes {
		if p == strings.TrimSuffix(pre, "/") || strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

func wrap(root fs.FS) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		files := http.FileServer(http.FS(root))
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if (r.Method != http.MethodGet && r.Method != http.MethodHead) || isAPI(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
			if name == "" {
				name = "index.html"
			}
			if st, err := fs.Stat(root, name); err == nil && !st.IsDir() && name != "index.html" {
				if strings.HasPrefix(name, "assets/") {
					// Vite content-hashes everything under assets/.
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				} else {
					w.Header().Set("Cache-Control", "public, max-age=300")
				}
				files.ServeHTTP(w, r)
				return
			}
			if strings.Contains(path.Base(name), ".") && strings.HasPrefix(name, "assets/") {
				http.NotFound(w, r) // missing hashed asset: don't mask with HTML
				return
			}
			// SPA fallback: client-side route.
			b, err := fs.ReadFile(root, "index.html")
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			if r.Method == http.MethodHead {
				return
			}
			_, _ = w.Write(b)
		})
	}
}
