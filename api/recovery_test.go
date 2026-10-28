package api_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/headmesh-dev/headmesh/api"
)

// TestRecoveryRefusesWritesUntilItEnds checks plan §13.4's recovery mode: while it is active, /readyz and every
// state-changing request answer 503 with Retry-After, and reads still work.
func TestRecoveryRefusesWritesUntilItEnds(t *testing.T) {
	rec := &api.Recovery{}
	router := chi.NewRouter()
	router.Use(rec.Middleware)
	api.Health(router, nil, rec)
	router.HandleFunc("/v1/grants", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	do := func(method, path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), method, path, nil))
		return w
	}

	rec.Begin()
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		w := do(method, "/v1/grants")
		if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
			t.Errorf("%s during recovery: %d (Retry-After %q), want 503 with Retry-After", method, w.Code, w.Header().Get("Retry-After"))
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/problem+json" {
			t.Errorf("%s during recovery: Content-Type %q, want application/problem+json", method, ct)
		}
	}
	if w := do(http.MethodGet, "/v1/grants"); w.Code != http.StatusOK {
		t.Errorf("GET during recovery: %d, want 200", w.Code)
	}
	if w := do(http.MethodGet, "/readyz"); w.Code != http.StatusServiceUnavailable {
		t.Errorf("/readyz during recovery: %d, want 503", w.Code)
	}
	if w := do(http.MethodGet, "/healthz"); w.Code != http.StatusNoContent {
		t.Errorf("/healthz during recovery: %d, want 204", w.Code)
	}

	rec.End()
	if w := do(http.MethodPost, "/v1/grants"); w.Code != http.StatusOK {
		t.Errorf("POST after recovery: %d, want 200", w.Code)
	}
}

// TestNilRecoveryIsOpen keeps a nil gate harmless, so callers that don't use recovery mode need no setup.
func TestNilRecoveryIsOpen(t *testing.T) {
	var rec *api.Recovery
	if rec.Active() {
		t.Fatal("nil Recovery reports active")
	}
}
