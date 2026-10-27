package authn

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/headmesh-dev/headmesh/sandbox/internal/testutil"
)

func TestHashRoundTrip(t *testing.T) {
	tok, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, TokenPrefix) || len(tok) != len(TokenPrefix)+43 {
		t.Fatalf("token shape %q", tok)
	}
	h, err := HashToken(tok)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=65536,t=2,p=1$") {
		t.Fatalf("hash format %q", h)
	}
	if ok, err := VerifyHash(tok, h); err != nil || !ok {
		t.Fatalf("verify own hash: %v %v", ok, err)
	}
	if ok, _ := VerifyHash(tok+"x", h); ok {
		t.Fatal("wrong token verified")
	}
	for _, bad := range []string{"", "$bcrypt$x", "$argon2id$v=18$m=1,t=1,p=1$AA$AA", "$argon2id$v=19$m=999999999,t=1,p=1$AA$AA"} {
		if ok, err := VerifyHash(tok, bad); ok || err == nil {
			t.Errorf("VerifyHash(%q) = %v, %v", bad, ok, err)
		}
	}
}

func TestStoreLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "tokens.json")
	f, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	tok, rec, err := f.Create("claude-code-mac", nil, "", now)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Route != RouteMCP || len(rec.Scopes) != 3 || strings.Contains(rec.Hash, tok) {
		t.Fatalf("record %+v", rec)
	}
	tunnelTok, _, err := f.Create("chatgpt-tunnel", []string{ScopeRead, ScopeExec}, RouteTunnel, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.Create("claude-code-mac", nil, "", now); err == nil {
		t.Fatal("duplicate active name should fail")
	}
	if _, _, err := f.Create("x", []string{"sandbox:root"}, "", now); err == nil {
		t.Fatal("unknown scope should fail")
	}
	if err := f.Save(path); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), tok) || strings.Contains(string(raw), tunnelTok) {
		t.Fatal("tokens.json contains a plaintext token")
	}

	s := NewStore(path)
	s.recheck = 0
	got, err := s.Verify(tok)
	if err != nil || got.Name != "claude-code-mac" {
		t.Fatalf("verify: %+v %v", got, err)
	}
	got, err = s.Verify(tunnelTok)
	if err != nil || got.Route != RouteTunnel {
		t.Fatalf("verify tunnel: %+v %v", got, err)
	}
	if _, err := s.Verify("hms_" + strings.Repeat("A", 43)); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("unknown token: %v", err)
	}
	if _, err := s.Verify("Bearer junk"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("junk token: %v", err)
	}
	if s.Count() != 2 {
		t.Fatalf("count = %d", s.Count())
	}

	// Revocation reaches a running store without a restart.
	f2, _ := LoadFile(path)
	if _, err := f2.Revoke("claude-code-mac", now); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond) // distinct mtime
	if err := f2.Save(path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(tok); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("revoked token still verifies: %v", err)
	}
	if _, err := f2.Revoke("claude-code-mac", now); err == nil {
		t.Fatal("double revoke should fail")
	}
}

func TestStoreKeepsLastGoodFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	f := &File{Version: 1}
	tok, _, _ := f.Create("a", nil, "", time.Now())
	if err := f.Save(path); err != nil {
		t.Fatal(err)
	}
	s := NewStore(path)
	s.recheck = 0
	if _, err := s.Verify(tok); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(tok); err != nil {
		t.Fatalf("broken file should keep the last good tokens: %v", err)
	}
	if s.LoadError() == nil {
		t.Fatal("load error should be reported")
	}
}

func newVerifiedServer(t *testing.T, store *Store) *httptest.Server {
	t.Helper()
	h := Middleware(store, StubResourceServer{Resource: "http://sbx/mcp"}, "http://sbx/.well-known/oauth-protected-resource/mcp",
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c := FromTokenInfo(auth.TokenInfoFromContext(r.Context()))
			_ = json.NewEncoder(w).Encode(c)
		}))
	return httptest.NewServer(h)
}

func TestMiddlewareTCP(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	f := &File{Version: 1}
	tok, _, _ := f.Create("tunnel", []string{ScopeRead}, RouteTunnel, time.Now())
	if err := f.Save(path); err != nil {
		t.Fatal(err)
	}
	srv := newVerifiedServer(t, NewStore(path))
	defer srv.Close()

	do := func(bearer string) *http.Response {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := do("")
	ch := resp.Header.Get("WWW-Authenticate")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(ch, `resource_metadata="http://sbx/.well-known/oauth-protected-resource/mcp"`) ||
		!strings.Contains(ch, `scope="sandbox:read sandbox:exec sandbox:write sandbox:admin"`) {
		t.Fatalf("no-token response %d %q", resp.StatusCode, ch)
	}
	for _, bad := range []string{unixBearer, "eyJhbGciOi.jwt.token", "hms_nope"} {
		resp := do(bad)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("bearer %q got %d", bad, resp.StatusCode)
		}
	}
	resp = do(tok)
	defer func() { _ = resp.Body.Close() }()
	var c Caller
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		t.Fatal(err)
	}
	if c.Client != "tunnel" || !c.Tunnel() || !c.Has(ScopeRead) || c.Has(ScopeExec) {
		t.Fatalf("caller %+v", c)
	}
}

func TestMiddlewareUnixSocket(t *testing.T) {
	dir := testutil.ShortDir(t)
	sock := filepath.Join(dir, "s.sock")
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(filepath.Join(dir, "missing.json"))
	h := Middleware(store, nil, "", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(FromTokenInfo(auth.TokenInfoFromContext(r.Context())))
	}))
	srv := &http.Server{Handler: InjectUnixBearer(h), ConnContext: UnixConnContext, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://unix/", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	var c Caller
	if err := json.Unmarshal(body, &c); err != nil {
		t.Fatalf("%s: %v", body, err)
	}
	if c.Route != RouteUnix || !c.Has(ScopeAdmin) || c.Client != "unix:"+strconv.Itoa(os.Getuid()) {
		t.Fatalf("unix caller %+v", c)
	}
}

func TestMetadataHandler(t *testing.T) {
	rs := StubResourceServer{Resource: "https://asus.sbx.headmesh.dev/mcp", AuthorizationServers: []string{"https://headmesh.dev"}}
	rec := httptest.NewRecorder()
	MetadataHandler(rs).ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/.well-known/oauth-protected-resource", nil))
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["resource"] != "https://asus.sbx.headmesh.dev/mcp" || len(m["scopes_supported"].([]any)) != 4 {
		t.Fatalf("metadata %v", m)
	}
	if _, err := rs.VerifyAccessToken(context.Background(), "x"); !errors.Is(err, ErrOAuthNotImplemented) {
		t.Fatalf("stub verify: %v", err)
	}
}
