package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

// The SDK rejects unrecognized methods at its HTTP decoding layer with a plain
// 400. Well-formed JSON-RPC requests instead need a -32601 response carrying
// their original ID. Authentication wraps this handler, including error probes.
func methodErrors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		const maxBody = 10 << 20
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			http.Error(w, "request body too large or unreadable", http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var request struct {
			Version string          `json:"jsonrpc"`
			Method  string          `json:"method"`
			ID      json.RawMessage `json:"id"`
		}
		if json.Unmarshal(body, &request) != nil || request.Version != "2.0" || request.Method == "" {
			next.ServeHTTP(w, r)
			return
		}
		if len(request.ID) > 0 {
			var id any
			_ = json.Unmarshal(request.ID, &id)
			switch id.(type) {
			case string, float64:
			default:
				// Leave invalid IDs to the SDK's invalid-request handling.
				next.ServeHTTP(w, r)
				return
			}
		}
		switch request.Method {
		case "initialize", "ping", "tools/list", "tools/call", "resources/list", "resources/read",
			"resources/templates/list", "resources/subscribe", "resources/unsubscribe",
			"prompts/list", "prompts/get", "logging/setLevel", "completion/complete",
			"tasks/get", "tasks/list", "tasks/result", "tasks/cancel",
			"notifications/initialized", "notifications/cancelled", "notifications/progress",
			"notifications/roots/list_changed", "notifications/tasks/status":
			next.ServeHTTP(w, r)
			return
		}
		if len(request.ID) == 0 {
			// Notifications never receive JSON-RPC responses, including unknown
			// methods. Acknowledge receipt without pretending an action ran.
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": request.ID,
			"error": map[string]any{"code": -32601, "message": "Method not found"},
		})
	})
}
