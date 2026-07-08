package sshops

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"

	"github.com/compshare-agent/internal/sanitizer"
)

// apiProxy is a per-task, loopback-only HTTP endpoint that lets the spawned harness PULL
// tenant-scoped, read-only CompShare API data per turn — the destination-B move where the agent
// decides in-loop whether to read an API or shell into the box. Design invariants:
//
//   - No signing in the harness. The harness holds NO AK/SK and does NO request signing (which the
//     repo warns is a footgun). It POSTs {action, params}; this proxy calls the already-signed,
//     tenant-bound Describer (the same executor FetchCredential/ListCandidates use) and returns the
//     result. Signing stays in Go where it is tested.
//   - Deny-by-default. Only actions in `allow` are dispatched (read-only Describe*); anything else
//     is 403 and never reaches the executor.
//   - Every response is run through sanitizer.Sanitize before it leaves this process, stripping
//     credential fields (Password/*Token/*Key) — the same boundary the product's LLM-callable
//     describe path uses.
//   - Loopback + one-time token. The listener binds 127.0.0.1:0 (OS-ephemeral, reachable only by
//     the child harness on this host); a per-task random bearer token gates every request as
//     defense-in-depth. Closed when the task ends — same lifetime as the SSH credential.
type apiProxy struct {
	ln    net.Listener
	srv   *http.Server
	token string
	url   string
}

// apiReadRequest is the harness -> proxy call body: a whitelisted read-only action + its params.
type apiReadRequest struct {
	Action string         `json:"action"`
	Params map[string]any `json:"params"`
}

func randToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// startAPIProxy binds a loopback listener and serves allowlisted, read-only API reads via d, bound
// to ctx (the task's tenant-scoped, time-bounded context). allow is the set of permitted action
// names (deny-by-default). The caller MUST call Close() when the task ends.
func startAPIProxy(ctx context.Context, d Describer, allow []string) (*apiProxy, error) {
	if d == nil {
		return nil, fmt.Errorf("sshops: api proxy needs a describer")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("sshops: api proxy listen: %w", err)
	}
	tok, err := randToken()
	if err != nil {
		_ = ln.Close()
		return nil, err
	}
	allowSet := make(map[string]bool, len(allow))
	for _, a := range allow {
		allowSet[a] = true
	}
	p := &apiProxy{
		ln:    ln,
		token: tok,
		url:   "http://" + ln.Addr().String() + "/api_read",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api_read", p.handle(ctx, d, allowSet))
	p.srv = &http.Server{Handler: mux}
	go func() { _ = p.srv.Serve(ln) }()
	return p, nil
}

func (p *apiProxy) handle(ctx context.Context, d Describer, allow map[string]bool) http.HandlerFunc {
	want := "Bearer " + p.token
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// constant-time token compare; reject before touching the body or the executor.
		got := r.Header.Get("Authorization")
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req apiReadRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if !allow[req.Action] {
			// deny-by-default: never reaches the executor.
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "action not allowed", "action": req.Action})
			return
		}
		raw, err := d.Execute(ctx, req.Action, req.Params)
		if err != nil {
			// Do not echo the upstream error verbatim to the model — it may carry internal detail.
			writeJSON(w, http.StatusBadGateway, map[string]any{"error": "upstream read failed", "action": req.Action})
			return
		}
		writeJSON(w, http.StatusOK, sanitizer.Sanitize(req.Action, raw))
	}
}

// URL is the loopback endpoint the harness POSTs to. Token is the per-task bearer secret.
func (p *apiProxy) URL() string   { return p.url }
func (p *apiProxy) Token() string { return p.token }

// Close stops the listener; safe to call once at task end.
func (p *apiProxy) Close() {
	if p.srv != nil {
		_ = p.srv.Close()
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
