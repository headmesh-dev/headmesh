//go:build integration

package login

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/headmesh-dev/headmesh/db"
)

func testStore(t *testing.T) *Store {
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
	defer func() { _ = sqlDB.Close() }()
	goose.SetBaseFS(db.Migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		t.Fatal(err)
	}
	if err := goose.UpContext(ctx, sqlDB, "migrations"); err != nil {
		t.Fatal(err)
	}
	_, err = owner.Exec(ctx, `CREATE ROLE login_app LOGIN PASSWORD 'test-only' NOSUPERUSER NOBYPASSRLS;
		GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO login_app`)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.User = "login_app"
	app, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	return &Store{Pool: app}
}

func TestBrowserLoginStorage(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	identity := Identity{Provider: "github", Subject: "github-id", Name: "owner", Email: "owner@example.test", Verified: true}
	user, first, err := s.Complete(ctx, identity, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if user.ID == "" || len(first) != 43 {
		t.Fatal("missing user or browser session")
	}
	if _, err = s.Session(ctx, first); err != nil {
		t.Fatal(err)
	}
	t.Run("session rotation", func(t *testing.T) {
		_, next, err := s.Complete(ctx, identity, "", first)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.Session(ctx, first); err == nil {
			t.Fatal("rotated session still accepted")
		}
		first = next
	})
	hf := Identity{Provider: "huggingface", Subject: "hf-id", Name: "owner", Email: identity.Email, Verified: true}
	t.Run("same email never links automatically", func(t *testing.T) {
		if _, _, err := s.Complete(ctx, hf, "", ""); !errors.Is(err, ErrAccountLink) {
			t.Fatalf("email collision error %v", err)
		}
	})
	t.Run("explicit link requires current session", func(t *testing.T) {
		if _, _, err := s.Complete(ctx, hf, user.ID, "wrong-session"); err == nil {
			t.Fatal("linked without browser ownership")
		}
		linked, token, err := s.Complete(ctx, hf, user.ID, first)
		if err != nil || linked.ID != user.ID {
			t.Fatalf("link %v err %v", linked, err)
		}
		first = token
		linked, _, err = s.Complete(ctx, hf, "", "")
		if err != nil || linked.ID != user.ID {
			t.Fatalf("linked provider did not sign into same account: %v", err)
		}
	})
	t.Run("no sandbox or org access created", func(t *testing.T) {
		var count int
		if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM memberships WHERE user_id=$1::uuid`, user.ID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("membership count %d err %v", count, err)
		}
	})
	t.Run("callback is browser bound and consumed once", func(t *testing.T) {
		if err := s.SaveFlow(ctx, "state", "browser", Flow{Provider: "github", Verifier: strings.Repeat("v", 43), Nonce: "nonce", ReturnPath: "/oauth/account"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ConsumeFlow(ctx, "state", "other-browser", "github"); !errors.Is(err, ErrInvalidFlow) {
			t.Fatal("other browser consumed flow")
		}
		if _, err := s.ConsumeFlow(ctx, "state", "browser", "huggingface"); !errors.Is(err, ErrInvalidFlow) {
			t.Fatal("wrong provider consumed flow")
		}
		var wg sync.WaitGroup
		results := make(chan bool, 2)
		for range 2 {
			wg.Go(func() { _, err := s.ConsumeFlow(ctx, "state", "browser", "github"); results <- err == nil })
		}
		wg.Wait()
		close(results)
		success := 0
		for ok := range results {
			if ok {
				success++
			}
		}
		if success != 1 {
			t.Fatalf("one-use callback succeeded %d times", success)
		}
	})
	t.Run("logout revokes session", func(t *testing.T) {
		if err := s.Logout(ctx, first); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Session(ctx, first); err == nil {
			t.Fatal("signed-out session accepted")
		}
	})
	t.Run("disabled account rejected", func(t *testing.T) {
		if _, err := s.Pool.Exec(ctx, `UPDATE users SET disabled_at=now() WHERE id=$1::uuid`, user.ID); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.Complete(ctx, identity, "", ""); err == nil {
			t.Fatal("disabled user signed in")
		}
	})
}

func TestBrowserHTTPFlowAndCSRF(t *testing.T) {
	s := testStore(t)
	provider := testProvider{}
	h := (&Handler{PublicURL: "https://headmesh.test", Store: s, Providers: []Provider{provider}}).HTTP()
	begin := httptest.NewRequest(http.MethodPost, "https://headmesh.test/oauth/login/github", strings.NewReader("next=https%3A%2F%2Fevil.test"))
	begin.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	begin.Header.Set("Origin", "https://evil.test")
	denied := httptest.NewRecorder()
	h.ServeHTTP(denied, begin)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("cross-origin sign-in got %d", denied.Code)
	}
	begin = httptest.NewRequest(http.MethodPost, "https://headmesh.test/oauth/login/github", strings.NewReader("next=https%3A%2F%2Fevil.test"))
	begin.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	begin.Header.Set("Origin", "https://headmesh.test")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, begin)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("begin %d: %s", response.Code, response.Body.String())
	}
	location, _ := url.Parse(response.Header().Get("Location"))
	state := location.Query().Get("state")
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].Path != "/" {
		t.Fatal("login cookie is not protected")
	}
	callback := httptest.NewRequest(http.MethodGet, "https://headmesh.test/oauth/callback/github?code=one-use&state="+state, nil)
	callback.AddCookie(cookies[0])
	completed := httptest.NewRecorder()
	h.ServeHTTP(completed, callback)
	if completed.Code != http.StatusSeeOther || completed.Header().Get("Location") != "/oauth/account" {
		t.Fatalf("callback %d: %s", completed.Code, completed.Body.String())
	}
	for _, cookie := range completed.Result().Cookies() {
		if cookie.Name == SessionCookie {
			if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
				t.Fatal("session cookie is not protected")
			}
			if _, err := s.Session(context.Background(), cookie.Value); err != nil {
				t.Fatal(err)
			}
		}
	}
	replay := httptest.NewRecorder()
	h.ServeHTTP(replay, callback)
	if replay.Code != http.StatusBadRequest {
		t.Fatal("callback replay succeeded")
	}
}

type testProvider struct{}

func (testProvider) Name() string { return "github" }
func (testProvider) AuthURL(state, verifier, nonce string) string {
	return "https://provider.test/authorize?state=" + state
}
func (testProvider) Exchange(context.Context, string, string, string) (Identity, error) {
	return Identity{Provider: "github", Subject: "http-user", Name: "owner", Email: "http@example.test", Verified: true}, nil
}
