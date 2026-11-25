package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func TestSPAHandler(t *testing.T) {
	root := fstest.MapFS{
		"index.html":    {Data: []byte("<html>spa</html>")},
		"assets/a-1.js": {Data: []byte("js")},
		"favicon.svg":   {Data: []byte("svg")},
	}
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("api")) })
	h := wrap(root)(api)
	get := func(method, p string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, p, nil))
		return rec
	}
	if b := get("GET", "/projects/x/y").Body.String(); b != "<html>spa</html>" {
		t.Fatalf("fallback: %q", b)
	}
	if rec := get("GET", "/"); rec.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("index cache: %q", rec.Header().Get("Cache-Control"))
	}
	if rec := get("GET", "/assets/a-1.js"); rec.Body.String() != "js" || rec.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("asset: %q %q", rec.Body.String(), rec.Header().Get("Cache-Control"))
	}
	if rec := get("GET", "/assets/missing.js"); rec.Code != 404 {
		t.Fatalf("missing asset: %d", rec.Code)
	}
	if b := get("GET", "/aetherix.v1.ClusterService/GetStatus").Body.String(); b != "api" {
		t.Fatalf("api passthrough: %q", b)
	}
	if b := get("POST", "/login").Body.String(); b != "api" {
		t.Fatalf("post passthrough: %q", b)
	}
	if b := get("GET", "/oauth/consent?request=abc").Body.String(); b != "<html>spa</html>" {
		t.Fatalf("consent page must be the SPA: %q", b)
	}
	if b := get("GET", "/oauth/api/consent").Body.String(); b != "api" {
		t.Fatalf("consent api passthrough: %q", b)
	}
	if b := get("GET", "/mcp").Body.String(); b != "api" {
		t.Fatalf("mcp passthrough: %q", b)
	}
}

func TestEmbeddedPlaceholder(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler()(http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest("GET", "/some/route", nil))
	if rec.Code != 200 {
		t.Fatalf("code %d", rec.Code)
	}
}
