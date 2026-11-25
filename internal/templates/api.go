package templates

import (
	"encoding/json"
	"net/http"
)

// Path is GET /api/templates.
const Path = "/api/templates"

// Handler returns the template catalog (public read for authenticated clients).
func Handler(authenticate func(*http.Request) error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authenticate != nil {
			if err := authenticate(r); err != nil {
				http.Error(w, err.Error(), http.StatusUnauthorized)
				return
			}
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"templates": List()})
	})
}
