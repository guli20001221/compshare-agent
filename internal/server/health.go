package server

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// handleHealthz is the liveness probe. Returns 200 unconditionally — a
// failure means the process is unable to respond at all (i.e. needs
// restart). Used by k8s liveness or load-balancer health checks.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
}

// handleReadyz is the readiness probe. Returns 200 if the process is
// healthy AND ready to receive traffic; 503 otherwise. Used by k8s
// readiness or load-balancer routing decisions.
//
// Failure cases:
//   - shuttingDown flag set (during graceful drain): always 503 so the
//     load balancer stops sending NEW traffic while in-flight handlers
//     finish.
//   - MySQL ping fails (when DB is configured): 503 with checks payload
//     describing the failure. The trace sink can still work for a brief
//     outage thanks to the buffered queue, but readyz fails fast so ops
//     can route around the bad pod.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	checks := map[string]string{}

	if s.shuttingDown.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":     false,
			"reason": "shutting_down",
		})
		return
	}

	// Engine deps are mandatory; if Run was called, this is non-nil
	// because New rejects nil deps.
	checks["engine"] = "ok"

	if s.db != nil {
		if err := s.db.PingContext(r.Context()); err != nil {
			checks["mysql"] = fmt.Sprintf("failed: %v", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":     false,
				"checks": checks,
			})
			return
		}
		checks["mysql"] = "ok"
	} else {
		checks["mysql"] = "skipped (DB not configured)"
	}

	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":     true,
		"checks": checks,
	})
}
