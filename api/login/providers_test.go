package login

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/zitadel/oidc/v3/pkg/client/rp"
	"golang.org/x/oauth2"
)

func TestGitHubIdentityAndPKCE(t *testing.T) {
	var verifier string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			_ = r.ParseForm()
			verifier = r.Form.Get("code_verifier")
			if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("redirect_uri") != "https://headmesh.test/oauth/callback/github" {
				t.Error("missing token request binding")
			}
			_, _ = w.Write([]byte(`{"access_token":"upstream-secret","token_type":"bearer"}`))
		case "/user":
			if r.Header.Get("Authorization") != "Bearer upstream-secret" {
				t.Error("missing upstream bearer")
			}
			_, _ = w.Write([]byte(`{"id":1234,"login":"developer"}`))
		case "/user/emails":
			_, _ = w.Write([]byte(`[{"email":"Owner@EXAMPLE.TEST","primary":true,"verified":true}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	p := &github{config: oauth2.Config{ClientID: "app", ClientSecret: "secret", RedirectURL: "https://headmesh.test/oauth/callback/github",
		Endpoint: oauth2.Endpoint{AuthURL: server.URL + "/authorize", TokenURL: server.URL + "/token", AuthStyle: oauth2.AuthStyleInParams}},
		client: server.Client(), apiBase: server.URL}
	u, _ := url.Parse(p.AuthURL("browser-state", "pkce-verifier", "unused"))
	if u.Query().Get("code_challenge_method") != "S256" || u.Query().Get("code_challenge") != oauth2.S256ChallengeFromVerifier("pkce-verifier") {
		t.Fatal("authorization request is not bound to PKCE")
	}
	identity, err := p.Exchange(context.Background(), "code", "pkce-verifier", "unused")
	if err != nil || identity.Subject != "1234" || identity.Email != "owner@example.test" || !identity.Verified || verifier != "pkce-verifier" {
		t.Fatalf("identity %v err %v verifier %q", identity, err, verifier)
	}
}

func TestHuggingFaceOIDCVerification(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, nonce, subject, audience string
		verified                       bool
		ok                             bool
	}{
		{"valid", "expected-nonce", "hf-subject", "client-id", true, true},
		{"wrong nonce", "attacker-nonce", "hf-subject", "client-id", true, false},
		{"wrong audience", "expected-nonce", "hf-subject", "other-client", true, false},
		{"userinfo subject mismatch", "expected-nonce", "another-subject", "client-id", true, false},
		{"unverified email", "expected-nonce", "hf-subject", "client-id", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var base string
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/.well-known/openid-configuration":
					_ = json.NewEncoder(w).Encode(map[string]any{"issuer": base, "authorization_endpoint": base + "/authorize", "token_endpoint": base + "/token", "userinfo_endpoint": base + "/userinfo", "jwks_uri": base + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"}})
				case "/jwks":
					_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "test-key", Algorithm: "RS256", Use: "sig"}}})
				case "/token":
					_ = r.ParseForm()
					if r.Form.Get("code_verifier") != "pkce-verifier" {
						t.Error("PKCE verifier was not sent")
					}
					signer, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "test-key"))
					claims, _ := json.Marshal(map[string]any{"iss": base, "sub": "hf-subject", "aud": tc.audience, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(), "nonce": tc.nonce})
					signed, _ := signer.Sign(claims)
					token, _ := signed.CompactSerialize()
					_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "upstream-secret", "token_type": "Bearer", "id_token": token})
				case "/userinfo":
					_ = json.NewEncoder(w).Encode(map[string]any{"sub": tc.subject, "preferred_username": "developer", "email": "owner@example.test", "email_verified": tc.verified})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			base = server.URL
			party, err := rp.NewRelyingPartyOIDC(context.Background(), base, "client-id", "", "https://headmesh.test/oauth/callback/huggingface", []string{"openid", "profile", "email"}, rp.WithHTTPClient(server.Client()), rp.WithSigningAlgsFromDiscovery(), rp.WithVerifierOpts(rp.WithNonce(func(ctx context.Context) string { nonce, _ := ctx.Value(nonceKey{}).(string); return nonce })))
			if err != nil {
				t.Fatal(err)
			}
			p := &huggingface{rp: party}
			identity, err := p.Exchange(context.Background(), "code", "pkce-verifier", "expected-nonce")
			if (err == nil) != tc.ok {
				t.Fatalf("identity %v error %v", identity, err)
			}
			if err != nil && strings.Contains(err.Error(), "upstream-secret") {
				t.Fatal("provider credential leaked in error")
			}
		})
	}
}

func TestProviderConfigurationRejectsIncompleteOrInsecureConfig(t *testing.T) {
	for _, c := range []Config{
		{PublicURL: "http://headmesh.test"},
		{PublicURL: "https://headmesh.test/evil"},
		{PublicURL: "https://headmesh.test", GitHubClientID: "id"},
		{PublicURL: "https://headmesh.test", GitHubSecret: "secret"},
		{PublicURL: "https://headmesh.test", HuggingFace: true, HFSecret: "secret"},
	} {
		if _, err := Providers(context.Background(), c); err == nil {
			t.Fatalf("accepted invalid provider config %+v", c)
		}
	}
}
