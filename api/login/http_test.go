package login

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReturnPathsCannotRedirectOutsideAccountOrConsent(t *testing.T) {
	for _, path := range []string{"https://evil.test", "//evil.test", "/\\evil.test", "/oauth/%61ccount", "/oauth/account#https://evil.test", "/oauth/authorize", "\r\nLocation: https://evil.test"} {
		if got := safeNext(path); got != "/oauth/account" {
			t.Errorf("unsafe return %q accepted as %q", path, got)
		}
	}
	if got := safeNext("/oauth/consent?request=abc"); got != "/oauth/consent?request=abc" {
		t.Fatalf("consent return %q", got)
	}
}

func TestCIMDHasExactPublicCallbackAndNoSecrets(t *testing.T) {
	h := (&Handler{PublicURL: "https://headmesh.test"}).HTTP()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "https://headmesh.test/.well-known/oauth-cimd", nil))
	var meta struct {
		ClientID  string   `json:"client_id"`
		Redirects []string `json:"redirect_uris"`
		Auth      string   `json:"token_endpoint_auth_method"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || meta.ClientID != "https://headmesh.test/.well-known/oauth-cimd" || len(meta.Redirects) != 1 || meta.Redirects[0] != "https://headmesh.test/oauth/callback/huggingface" || meta.Auth != "none" {
		t.Fatalf("invalid CIMD %+v", meta)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store, no-transform" {
		t.Fatalf("auth payload can be rewritten: %q", got)
	}
}
