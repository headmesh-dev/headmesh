//go:build integration

package oauth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/headmesh-dev/headmesh/api/login"
	"github.com/headmesh-dev/headmesh/db"
)

const testIssuer = "https://auth.headmesh.test"
const testResource = "https://sandbox.headmesh.test/mcp"
const testRedirect = "https://chatgpt.com/connector_platform/oauth/redirect"

type testSystem struct {
	h       *Handler
	http    http.Handler
	pool    *pgxpool.Pool
	owner   *pgxpool.Pool
	user    login.User
	session string
	client  string
}

func setup(t *testing.T) *testSystem {
	t.Helper()
	ctx := context.Background()
	ctr, err := tcpostgres.Run(ctx, "postgres:18.4-alpine", tcpostgres.WithDatabase("headmesh"), tcpostgres.WithUsername("owner"), tcpostgres.WithPassword("test-only"), tcpostgres.BasicWaitStrategies())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(ctr) })
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	owner, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	sqlDB := stdlib.OpenDBFromPool(owner)
	goose.SetBaseFS(db.Migrations)
	if err = goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err = goose.UpContext(ctx, sqlDB, "migrations"); err != nil {
		t.Fatal(err)
	}
	_ = sqlDB.Close()
	if _, err = owner.Exec(ctx, `CREATE ROLE oauth_app LOGIN PASSWORD 'test-only' NOSUPERUSER NOBYPASSRLS; GRANT SELECT,INSERT,UPDATE,DELETE ON ALL TABLES IN SCHEMA public TO oauth_app`); err != nil {
		t.Fatal(err)
	}
	cfg, _ := pgxpool.ParseConfig(dsn)
	cfg.ConnConfig.User = "oauth_app"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	storage, err := NewStorage(ctx, pool, testIssuer, private)
	if err != nil {
		t.Fatal(err)
	}
	accounts := &login.Store{Pool: pool}
	user, session, err := accounts.Complete(ctx, login.Identity{Provider: "github", Subject: "oauth-owner", Name: "Owner", Email: "owner@example.test", Verified: true}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = owner.Exec(ctx, `INSERT INTO oauth_resources VALUES($1,'Test sandbox')`, testResource); err != nil {
		t.Fatal(err)
	}
	if _, err = owner.Exec(ctx, `INSERT INTO oauth_resource_grants(user_id,resource,scopes) VALUES($1::uuid,$2,ARRAY['sandbox:read','sandbox:write','sandbox:exec'])`, user.ID, testResource); err != nil {
		t.Fatal(err)
	}
	var encryption [32]byte
	if _, err = rand.Read(encryption[:]); err != nil {
		t.Fatal(err)
	}
	h, err := NewHandler(storage, accounts, encryption)
	if err != nil {
		t.Fatal(err)
	}
	client, err := storage.Clients.Register(ctx, ClientMetadata{Name: "Integration client", Redirects: []string{testRedirect}})
	if err != nil {
		t.Fatal(err)
	}
	return &testSystem{h: h, http: h.HTTP(), pool: pool, owner: owner, user: user, session: session, client: client.ID}
}

func (s *testSystem) request(method, path string, data url.Values, session string) *httptest.ResponseRecorder {
	var body *strings.Reader
	if data == nil {
		body = strings.NewReader("")
	} else {
		body = strings.NewReader(data.Encode())
	}
	r := httptest.NewRequest(method, testIssuer+path, body)
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if session != "" {
		r.AddCookie(&http.Cookie{Name: login.SessionCookie, Value: session})
		r.Header.Set("Origin", testIssuer)
	}
	w := httptest.NewRecorder()
	s.http.ServeHTTP(w, r)
	return w
}

func (s *testSystem) authorization(t *testing.T, scopes string) (string, string, url.Values) {
	t.Helper()
	verifier, _ := secret()
	digest := sha256.Sum256([]byte(verifier))
	params := url.Values{"client_id": {s.client}, "redirect_uri": {testRedirect}, "resource": {testResource}, "response_type": {"code"}, "scope": {scopes}, "state": {"client-state"}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(digest[:])}}
	w := s.request("GET", "/oauth/authorize?"+params.Encode(), nil, "")
	if w.Code != http.StatusFound {
		t.Fatalf("authorize %d: %s %s", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	location, err := url.Parse(w.Header().Get("Location"))
	if err != nil || location.Path != "/oauth/consent" {
		t.Fatalf("invalid consent redirect %s", w.Header().Get("Location"))
	}
	id := location.Query().Get("request")
	w = s.request("GET", location.RequestURI(), nil, s.session)
	if w.Code != http.StatusOK {
		t.Fatalf("consent %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "form-action 'self' https://chatgpt.com;") {
		t.Fatal("consent CSP would block the registered callback after a form redirect")
	}
	match := regexp.MustCompile(`name="csrf" value="([^"]+)"`).FindStringSubmatch(w.Body.String())
	if len(match) != 2 {
		t.Fatalf("missing CSRF field: %s", w.Body.String())
	}
	return id, verifier, url.Values{"request": {id}, "csrf": {match[1]}, "decision": {"allow"}}
}

func (s *testSystem) code(t *testing.T, scopes string) (string, string) {
	t.Helper()
	_, verifier, consent := s.authorization(t, scopes)
	w := s.request("POST", "/oauth/consent", consent, s.session)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("allow %d: %s", w.Code, w.Body.String())
	}
	u, _ := url.Parse(w.Header().Get("Location"))
	w = s.request("GET", u.RequestURI(), nil, s.session)
	if w.Code != http.StatusFound {
		t.Fatalf("callback %d: %s", w.Code, w.Body.String())
	}
	u, _ = url.Parse(w.Header().Get("Location"))
	if u.Query().Get("iss") != testIssuer || u.Query().Get("state") != "client-state" || u.Query().Get("code") == "" {
		t.Fatalf("invalid code response %s", u.String())
	}
	return u.Query().Get("code"), verifier
}

func (s *testSystem) exchange(code, verifier string) *httptest.ResponseRecorder {
	return s.request("POST", "/oauth/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {s.client}, "redirect_uri": {testRedirect}, "resource": {testResource}, "code": {code}, "code_verifier": {verifier}}, "")
}

func (s *testSystem) tokens(t *testing.T) map[string]any {
	t.Helper()
	code, verifier := s.code(t, "sandbox:read sandbox:write offline_access openid")
	w := s.exchange(code, verifier)
	if w.Code != http.StatusOK {
		t.Fatalf("exchange %d: %s", w.Code, w.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func (s *testSystem) status(token string) int {
	r := httptest.NewRequest("GET", testIssuer+"/oauth/token-status", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	s.http.ServeHTTP(w, r)
	return w.Code
}

func TestMCPAuthorization(t *testing.T) {
	s := setup(t)
	t.Run("new sign-in has no machine grant", func(t *testing.T) {
		_, otherSession, err := (&login.Store{Pool: s.pool}).Complete(context.Background(), login.Identity{Provider: "huggingface", Subject: "another-user", Email: "other@example.test", Verified: true}, "", "")
		if err != nil {
			t.Fatal(err)
		}
		id, _, form := s.authorization(t, "sandbox:read")
		if w := s.request("GET", "/oauth/consent?request="+id, nil, otherSession); w.Code != 403 {
			t.Fatal("ungranted sign-in could review a sandbox grant")
		}
		if w := s.request("POST", "/oauth/consent", form, otherSession); w.Code != 403 {
			t.Fatal("another browser/user could consent")
		}
		r := httptest.NewRequest("POST", testIssuer+"/oauth/consent", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "https://evil.test")
		r.AddCookie(&http.Cookie{Name: login.SessionCookie, Value: s.session})
		w := httptest.NewRecorder()
		s.http.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal("cross-origin consent accepted")
		}
	})
	t.Run("wire rejects duplicate and unknown scopes", func(t *testing.T) {
		for _, params := range []url.Values{
			{"client_id": {s.client}, "scope": {"sandbox:read sandbox:admin"}},
			{"client_id": {s.client, s.client}},
		} {
			params.Set("resource", testResource)
			params.Set("response_type", "code")
			params.Set("redirect_uri", testRedirect)
			params.Set("code_challenge_method", "S256")
			params.Set("code_challenge", strings.Repeat("a", 43))
			if w := s.request("GET", "/oauth/authorize?"+params.Encode(), nil, ""); w.Code != 400 {
				t.Fatal("invalid authorization parameters accepted")
			}
		}
	})
	t.Run("DCR accepts public clients and rejects foreign redirects", func(t *testing.T) {
		for _, redirect := range []string{testRedirect, "https://evil.test/callback"} {
			body, _ := json.Marshal(ClientMetadata{Name: "Wire registered", Redirects: []string{redirect}, Auth: "none"})
			r := httptest.NewRequest("POST", testIssuer+"/oauth/register", strings.NewReader(string(body)))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			s.http.ServeHTTP(w, r)
			if redirect == testRedirect && w.Code != 201 {
				t.Fatalf("registration failed: %s", w.Body.String())
			}
			if redirect != testRedirect && w.Code != 400 {
				t.Fatal("foreign redirect registered")
			}
		}
	})
	t.Run("metadata matches implemented protocols", func(t *testing.T) {
		w := s.request("GET", "/.well-known/oauth-authorization-server", nil, "")
		var metadata map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &metadata)
		if w.Code != 200 || metadata["issuer"] != testIssuer || metadata["client_id_metadata_document_supported"] != true {
			t.Fatal("invalid discovery")
		}
		if strings.Contains(w.Body.String(), "implicit") || strings.Contains(w.Body.String(), "client_secret_basic") {
			t.Fatal("advertised unsupported protocol")
		}
	})
	t.Run("RLS hides other users grants", func(t *testing.T) {
		var count int
		if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM oauth_resource_grants`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("grant leak %d %v", count, err)
		}
	})
	t.Run("full consent PKCE and immediate revocation", func(t *testing.T) {
		tokens := s.tokens(t)
		access := tokens["access_token"].(string)
		if s.status(access) != 200 {
			t.Fatal("issued access token rejected")
		}
		if s.status(tokens["id_token"].(string)) != 401 {
			t.Fatal("ID token accepted as access")
		}
		w := s.request("POST", "/oauth/revoke", url.Values{"client_id": {s.client}, "token": {access}, "token_type_hint": {"access_token"}}, "")
		if w.Code != 200 || s.status(access) != 401 {
			t.Fatalf("revocation %d was not immediate", w.Code)
		}
	})
	t.Run("code replay and races", func(t *testing.T) {
		code, verifier := s.code(t, "sandbox:read")
		var wg sync.WaitGroup
		codes := make(chan int, 2)
		for range 2 {
			wg.Go(func() { codes <- s.exchange(code, verifier).Code })
		}
		wg.Wait()
		close(codes)
		ok := 0
		for status := range codes {
			if status == 200 {
				ok++
			}
		}
		if ok != 1 {
			t.Fatalf("concurrent code exchange succeeded %d times", ok)
		}
	})
	t.Run("wrong PKCE never issues credentials", func(t *testing.T) {
		code, _ := s.code(t, "sandbox:read")
		if w := s.exchange(code, strings.Repeat("a", 43)); w.Code == 200 {
			t.Fatal("bad PKCE accepted")
		}
	})
	t.Run("browser consent CSRF and refusal", func(t *testing.T) {
		_, _, form := s.authorization(t, "sandbox:read")
		wrong := form.Get("csrf")
		form.Set("csrf", "wrong")
		if w := s.request("POST", "/oauth/consent", form, s.session); w.Code != 403 {
			t.Fatal("wrong CSRF accepted")
		}
		form.Set("csrf", wrong)
		form.Set("decision", "deny")
		w := s.request("POST", "/oauth/consent", form, s.session)
		u, _ := url.Parse(w.Header().Get("Location"))
		if w.Code != 303 || u.Query().Get("error") != "access_denied" || u.Query().Get("iss") != testIssuer {
			t.Fatal("consent refusal did not return to client")
		}
	})
	t.Run("refresh rotates and reuse invalidates family", func(t *testing.T) {
		tokens := s.tokens(t)
		params := url.Values{"client_id": {s.client}, "resource": {testResource}, "grant_type": {"refresh_token"}, "refresh_token": {tokens["refresh_token"].(string)}}
		w := s.request("POST", "/oauth/token", params, "")
		if w.Code != 200 {
			t.Fatalf("refresh %d: %s", w.Code, w.Body.String())
		}
		var rotated map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &rotated)
		if rotated["refresh_token"] == tokens["refresh_token"] || s.status(rotated["access_token"].(string)) != 200 {
			t.Fatal("refresh did not rotate")
		}
		if w = s.request("POST", "/oauth/token", params, ""); w.Code == 200 {
			t.Fatal("reused refresh accepted")
		}
		if s.status(rotated["access_token"].(string)) != 401 || s.status(tokens["access_token"].(string)) != 401 {
			t.Fatal("reuse did not revoke family")
		}
	})
	t.Run("wrong resource refuses token exchange", func(t *testing.T) {
		code, verifier := s.code(t, "sandbox:read")
		w := s.request("POST", "/oauth/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {s.client}, "redirect_uri": {testRedirect}, "resource": {"https://other.test/mcp"}, "code": {code}, "code_verifier": {verifier}}, "")
		if w.Code == 200 {
			t.Fatal("wrong resource accepted")
		}
	})
	t.Run("grant revocation denies already issued access", func(t *testing.T) {
		tokens := s.tokens(t)
		if _, err := s.owner.Exec(context.Background(), `UPDATE oauth_resource_grants SET revoked_at=now() WHERE user_id=$1::uuid`, s.user.ID); err != nil {
			t.Fatal(err)
		}
		if s.status(tokens["access_token"].(string)) != 401 {
			t.Fatal("revoked grant retained access")
		}
		if _, err := s.owner.Exec(context.Background(), `UPDATE oauth_resource_grants SET revoked_at=NULL WHERE user_id=$1::uuid`, s.user.ID); err != nil {
			t.Fatal(err)
		}
		if s.status(tokens["access_token"].(string)) != 401 {
			t.Fatal("restoring a grant resurrected a revoked credential")
		}
	})
}
