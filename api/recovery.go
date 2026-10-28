package api

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
)

// Recovery gates the API while a restored control plane catches up (plan §13.4): /readyz answers 503 and every
// request that could change state gets 503 with Retry-After, until End is called. Reads keep working, so operators
// can inspect grants and audit while cells catch up. The zero value (and a nil *Recovery) is open.
type Recovery struct{ active atomic.Bool }

// Begin closes the gate.
func (r *Recovery) Begin() { r.active.Store(true) }

// End opens the gate.
func (r *Recovery) End() { r.active.Store(false) }

// Active reports whether the gate is closed.
func (r *Recovery) Active() bool { return r != nil && r.active.Load() }

// Middleware refuses state-changing requests while recovery is active.
func (r *Recovery) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if r.Active() {
				w.Header().Set("Retry-After", "15")
				writeProblem(w, http.StatusServiceUnavailable, "the control plane is recovering; cells are catching up")
				return
			}
		}
		next.ServeHTTP(w, req)
	})
}

// writeProblem writes an RFC 9457 problem document, matching the errors Huma returns for API operations.
func writeProblem(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Title  string `json:"title"`
		Status int    `json:"status"`
		Detail string `json:"detail"`
	}{http.StatusText(status), status, detail})
}
