package authn

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

func TestOAuthJWTAndRevocation(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var active atomic.Bool
	active.Store(true)
	var keyFetches, statusCalls atomic.Int32
	issuer := ""
	resource := "https://sandbox.headmesh.test/mcp"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/jwks":
			keyFetches.Add(1)
			_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: public, KeyID: "signing-key", Algorithm: "EdDSA", Use: "sig"}}})
		case "/oauth/token-status":
			statusCalls.Add(1)
			if !active.Load() {
				w.WriteHeader(401)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"active": true, "sub": "user-id", "jti": "token-id", "aud": resource, "scope": "sandbox:read sandbox:exec"})
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	issuer = server.URL
	rs, err := NewOAuthResourceServer(issuer, resource, issuer+"/oauth/jwks")
	if err != nil {
		t.Fatal(err)
	}
	rs.client = server.Client()
	makeToken := func(change func(*accessClaims), key ed25519.PrivateKey) string {
		claims := accessClaims{Claims: jwt.Claims{Issuer: issuer, Subject: "user-id", ID: "token-id", Audience: jwt.Audience{resource}, IssuedAt: jwt.NewNumericDate(time.Now()), NotBefore: jwt.NewNumericDate(time.Now()), Expiry: jwt.NewNumericDate(time.Now().Add(15 * time.Minute))}, Use: "access", Scope: "sandbox:read sandbox:exec"}
		if change != nil {
			change(&claims)
		}
		signer, e := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "signing-key"))
		if e != nil {
			t.Fatal(e)
		}
		token, e := jwt.Signed(signer).Claims(claims).Serialize()
		if e != nil {
			t.Fatal(e)
		}
		return token
	}
	ctx := context.Background()
	good := makeToken(nil, private)
	if info, e := rs.VerifyAccessToken(ctx, good); e != nil || info.UserID != "user-id" || len(info.Scopes) != 2 {
		t.Fatalf("valid JWT rejected %v", e)
	}
	_, forgedKey, _ := ed25519.GenerateKey(rand.Reader)
	for name, change := range map[string]func(*accessClaims){
		"wrong issuer":       func(c *accessClaims) { c.Issuer = "https://foreign.test" },
		"wrong audience":     func(c *accessClaims) { c.Audience = jwt.Audience{"https://other.test/mcp"} },
		"multiple audiences": func(c *accessClaims) { c.Audience = append(c.Audience, "https://other.test/mcp") },
		"ID token":           func(c *accessClaims) { c.Use = "" },
		"expired":            func(c *accessClaims) { c.Expiry = jwt.NewNumericDate(time.Now().Add(-time.Second)) },
		"future":             func(c *accessClaims) { c.IssuedAt = jwt.NewNumericDate(time.Now().Add(time.Hour)) },
		"no expiry":          func(c *accessClaims) { c.Expiry = nil },
		"long lifetime":      func(c *accessClaims) { c.Expiry = jwt.NewNumericDate(time.Now().Add(time.Hour)) },
		"admin escalation":   func(c *accessClaims) { c.Scope = "sandbox:read sandbox:admin" },
		"missing read":       func(c *accessClaims) { c.Scope = "sandbox:exec" },
		"wrong DB identity":  func(c *accessClaims) { c.Subject = "another-user" },
		"wrong DB scopes":    func(c *accessClaims) { c.Scope = "sandbox:read sandbox:write" },
	} {
		t.Run(name, func(t *testing.T) {
			if _, e := rs.VerifyAccessToken(ctx, makeToken(change, private)); e == nil {
				t.Fatal("invalid JWT accepted")
			}
		})
	}
	if _, e := rs.VerifyAccessToken(ctx, makeToken(nil, forgedKey)); e == nil {
		t.Fatal("forged signature accepted")
	}
	if keyFetches.Load() != 1 {
		t.Fatalf("JWKS cache fetched %d times", keyFetches.Load())
	}
	active.Store(false)
	if _, e := rs.VerifyAccessToken(ctx, good); e == nil {
		t.Fatal("revoked access accepted")
	}
	active.Store(true)
	if _, e := rs.VerifyAccessToken(ctx, good); e != nil {
		t.Fatal(e)
	}
	if statusCalls.Load() < 4 {
		t.Fatal("revocation state was cached")
	}
	server.Close()
	if _, e := rs.VerifyAccessToken(ctx, good); e == nil {
		t.Fatal("issuer outage did not fail closed")
	}
}

func TestOAuthConfigurationAndChallenges(t *testing.T) {
	for _, values := range [][3]string{
		{"http://auth.test", "https://sandbox.test/mcp", "https://auth.test/jwks"},
		{"https://auth.test/", "https://sandbox.test/mcp", "https://auth.test/jwks"},
		{"https://auth.test", "https://sandbox.test/mcp", "https://evil.test/jwks"},
		{"https://user@auth.test", "https://sandbox.test/mcp", "https://auth.test/jwks"},
	} {
		if _, e := NewOAuthResourceServer(values[0], values[1], values[2]); e == nil {
			t.Fatal("invalid configuration accepted")
		}
	}
	rs, e := NewOAuthResourceServer("https://auth.test", "https://sandbox.test/mcp", "https://auth.test/jwks")
	if e != nil {
		t.Fatal(e)
	}
	h := Middleware(NewStore(t.TempDir()+"/tokens.json"), rs, "https://sandbox.test/.well-known/oauth-protected-resource/mcp", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("unauthenticated request reached tool") }))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "https://sandbox.test/mcp", nil))
	if w.Code != 401 || strings.Contains(w.Header().Get("WWW-Authenticate"), ScopeAdmin) {
		t.Fatal("challenge advertised ungrantable admin scope")
	}
}
