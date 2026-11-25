package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"connectrpc.com/connect"
	"github.com/aetherix/aetherixcloud/internal/auth"
)

// RestoreHTTPPath accepts a raw .snapshot body. Browsers cannot upload a
// Connect client-stream (ReadableStream) over HTTP/1.1 / Vite / Safari.
const RestoreHTTPPath = "/api/cluster/restore"

const maxRestoreBytes = 64 << 20 // 64 MiB

func restoreHTTPHandler(s *server, authn *auth.Authenticator, public bool) http.Handler {
	svc := &clusterSvc{s}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if public && authn != nil {
			tok := auth.BearerFromHeader(r.Header.Get("Authorization"))
			p, err := authn.Authenticate(r.Context(), tok)
			if err != nil || !p.Allows(auth.LevelAdmin) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		force := r.URL.Query().Get("force") == "1" || r.URL.Query().Get("force") == "true" ||
			r.Header.Get("X-Aetherix-Force") == "1"
		body, err := io.ReadAll(io.LimitReader(r.Body, maxRestoreBytes+1))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(body) > maxRestoreBytes {
			http.Error(w, "snapshot too large", http.StatusRequestEntityTooLarge)
			return
		}
		out, err := svc.applySnapshotBytes(r.Context(), body, force)
		if err != nil {
			writeConnectHTTPError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tables":            out.Tables,
			"rows":              out.Rows,
			"machines_removed":  out.MachinesRemoved,
			"replicas_cleared":  out.ReplicasCleared,
		})
	})
}

func writeConnectHTTPError(w http.ResponseWriter, err error) {
	var ce *connect.Error
	if errors.As(err, &ce) {
		switch ce.Code() {
		case connect.CodeInvalidArgument:
			http.Error(w, ce.Message(), http.StatusBadRequest)
		case connect.CodeFailedPrecondition:
			http.Error(w, ce.Message(), http.StatusConflict)
		case connect.CodePermissionDenied:
			http.Error(w, ce.Message(), http.StatusForbidden)
		default:
			http.Error(w, ce.Message(), http.StatusInternalServerError)
		}
		return
	}
	msg := err.Error()
	if strings.Contains(msg, "non-fresh") {
		http.Error(w, msg, http.StatusConflict)
		return
	}
	http.Error(w, msg, http.StatusInternalServerError)
}
