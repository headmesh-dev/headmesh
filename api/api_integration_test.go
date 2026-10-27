//go:build integration

package api_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectinprocess"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/headmesh-dev/headmesh/api"
	"github.com/headmesh-dev/headmesh/core/cellagent"
	"github.com/headmesh-dev/headmesh/core/cellserver"
	cellv1 "github.com/headmesh-dev/headmesh/gen/cell/v1"
	"github.com/headmesh-dev/headmesh/gen/cell/v1/cellv1connect"
	headscalev1 "github.com/headmesh-dev/headmesh/gen/headscale/v1"
	"github.com/headmesh-dev/headmesh/internal/headscale"
	"github.com/headmesh-dev/headmesh/internal/service"
	"github.com/headmesh-dev/headmesh/internal/store"
	"github.com/headmesh-dev/headmesh/internal/store/dbq"
)

// simHeadscale simulates Headscale closely enough for enrollment: it mints keys, lets the test "register" a node
// with a key, and records the order of policy writes and key mints.
type simHeadscale struct {
	mu     sync.Mutex
	policy string
	keys   map[string]uint64 // key -> id
	nodes  []*headscalev1.Node
	events []string
}

func (s *simHeadscale) log(e string) { s.events = append(s.events, e) }

func (s *simHeadscale) ListNodes(context.Context) ([]*headscalev1.Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.nodes), nil
}

func (s *simHeadscale) CreatePreAuthKey(_ context.Context, r headscale.PreAuthKeyRequest) (*headscalev1.PreAuthKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := uint64(len(s.keys) + 1)
	key := fmt.Sprintf("hskey-%d", id)
	s.keys[key] = id
	s.log("key:" + strings.Join(r.Tags, ","))
	// Headscale only accepts tags its live policy declares.
	for _, t := range r.Tags {
		if !strings.Contains(s.policy, `"`+t+`"`) {
			return nil, fmt.Errorf("tag %s is not declared in the live policy", t)
		}
	}
	return &headscalev1.PreAuthKey{Id: id, Key: key, AclTags: r.Tags, Expiration: timestamppb.New(r.Expiration)}, nil
}

// register simulates `tailscale up --auth-key`.
func (s *simHeadscale) register(key string, ip string) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := uint64(100 + len(s.nodes))
	s.nodes = append(s.nodes, &headscalev1.Node{
		Id: id, IpAddresses: []string{ip}, Online: true,
		PreAuthKey: &headscalev1.PreAuthKey{Id: s.keys[key]},
	})
	return id
}

func (s *simHeadscale) SetTags(context.Context, uint64, []string) (*headscalev1.Node, error) {
	return &headscalev1.Node{}, nil
}

func (s *simHeadscale) RenameNode(_ context.Context, id uint64, name string) (*headscalev1.Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range s.nodes {
		if n.GetId() == id {
			n.GivenName = name
			return n, nil
		}
	}
	return nil, fmt.Errorf("node %d not found", id)
}
func (s *simHeadscale) DeleteNode(context.Context, uint64) error { return nil }
func (s *simHeadscale) ExpireNode(context.Context, uint64, bool) (*headscalev1.Node, error) {
	return &headscalev1.Node{}, nil
}

func (s *simHeadscale) SetApprovedRoutes(context.Context, uint64, []string) (*headscalev1.Node, error) {
	return &headscalev1.Node{}, nil
}

func (s *simHeadscale) GetPolicy(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.policy, nil
}

func (s *simHeadscale) SetPolicy(_ context.Context, p string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.policy = p
	s.log("policy")
	return nil
}
func (s *simHeadscale) CheckPolicy(context.Context, string) error { return nil }
func (s *simHeadscale) Health(context.Context) error              { return nil }

func (s *simHeadscale) livePolicy() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.policy
}

type client struct {
	t    *testing.T
	base string
}

func (c client) do(method, path, key string, body any, out any) int {
	c.t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, c.base+path, r)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode < 300 {
		if err := json.Unmarshal(raw, out); err != nil {
			c.t.Fatalf("%s %s: decode %s: %v", method, path, raw, err)
		}
	}
	if resp.StatusCode >= 500 {
		c.t.Logf("%s %s -> %d %s", method, path, resp.StatusCode, raw)
	}
	return resp.StatusCode
}

func TestOnboardingAndAccessLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Database: migrate as owner, run the app as a role without BYPASSRLS.
	ctr, err := tcpostgres.Run(ctx, "postgres:18.4-alpine", tcpostgres.WithDatabase("headmesh"),
		tcpostgres.WithUsername("owner"), tcpostgres.WithPassword("owner"), tcpostgres.BasicWaitStrategies())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(ctr) })
	dsn, _ := ctr.ConnectionString(ctx, "sslmode=disable")
	owner, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err := store.Migrate(ctx, owner); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE ROLE headmesh_app LOGIN PASSWORD 'app' NOSUPERUSER NOBYPASSRLS`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO headmesh_app`,
		`REVOKE UPDATE, DELETE ON audit_events FROM headmesh_app`,
	} {
		if _, err := owner.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	pcfg, _ := pgxpool.ParseConfig(dsn)
	pcfg.ConnConfig.User, pcfg.ConnConfig.Password = "headmesh_app", "app"
	appPool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		t.Fatal(err)
	}
	defer appPool.Close()
	st := store.New(appPool)

	// Hub + a real cell agent in front of the simulated Headscale.
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	quiet := slog.New(slog.DiscardHandler)
	hub, err := cellserver.NewHub(cellserver.Config{
		Signer: priv, Store: store.NewCellStore(st), Logger: quiet,
		Authenticate: func(context.Context) (string, error) { return "hs1", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	cs := connect.NewServer()
	cellv1connect.RegisterCellServiceHandler(cs, hub)
	hs := &simHeadscale{keys: map[string]uint64{}}
	svc := &service.Service{Store: st, Hub: hub, PublicURL: "https://api.test", InstallURL: "https://get.test"}

	plat, err := svc.Bootstrap(ctx, service.BootstrapRequest{
		CellName: "hs1", ServerURL: "https://hs1.test",
		BaseDomain: "mesh.test", CellMode: "enforce", OrgSlug: "xcelsior", OrgName: "Xcelsior", Template: "locked", Platform: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	own, err := svc.Bootstrap(ctx, service.BootstrapRequest{
		CellName: "hs1", OrgSlug: "owner-1", OrgName: "GPU owner",
		Template: "locked", ParentSlug: "xcelsior",
	})
	if err != nil {
		t.Fatal(err)
	}

	agent, err := cellagent.New(cellagent.Config{
		CellName: "hs1", Mode: cellv1.Mode_MODE_ENFORCE, StateDir: t.TempDir(),
		ServerKeys: []ed25519.PublicKey{pub},
	}, hs)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = agent.Run(ctx, cellv1connect.NewCellServiceClient(connect.NewClient(connectinprocess.New(cs))),
			cellagent.HelloInfo{AgentVersion: "it"}, cellagent.RunOptions{
				TickInterval:    20 * time.Millisecond,
				ObserveInterval: time.Hour, Logger: quiet,
			})
	}()
	for !hub.Connected("hs1") {
		time.Sleep(10 * time.Millisecond)
	}

	router := chi.NewRouter()
	api.New(router, api.Options{Service: svc, Logger: quiet, Version: "it"})
	srv := httptest.NewServer(router)
	defer srv.Close()
	c := client{t: t, base: srv.URL}

	// --- Unauthenticated and scoped access --------------------------------------------------------------
	if code := c.do("GET", "/v1/devices", "", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("no key: %d", code)
	}
	if code := c.do("GET", "/v1/devices", "hmk_not-a-real-key-at-all", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("bad key: %d", code)
	}

	// --- Enrollment ---------------------------------------------------------------------------------------
	enroll := func(adminKey, role, hostname, ip string) (map[string]any, uint64) {
		t.Helper()
		var tok struct {
			Token string `json:"token"`
		}
		if code := c.do("POST", "/v1/enrollment-tokens", adminKey, map[string]any{"roles": []string{role}, "ttl_seconds": 3600}, &tok); code != http.StatusCreated {
			t.Fatalf("create token: %d", code)
		}
		var e map[string]any
		if code := c.do("POST", "/v1/enroll", "", map[string]any{"token": tok.Token, "hostname": hostname, "os": "linux"}, &e); code != http.StatusOK {
			t.Fatalf("enroll: %d", code)
		}
		// The key is single-use: the same token cannot enroll a second device.
		if code := c.do("POST", "/v1/enroll", "", map[string]any{"token": tok.Token, "hostname": "again"}, nil); code != http.StatusUnauthorized {
			t.Fatalf("token reuse: %d", code)
		}
		node := hs.register(e["auth_key"].(string), ip)
		path := "/v1/enrollments/" + e["enrollment_id"].(string) + "/complete"
		if code := c.do("POST", path, "", map[string]any{"completion_secret": "hmc_wrongwrongwrongwrongwrong", "node_id": node}, nil); code != http.StatusUnauthorized {
			t.Fatalf("wrong completion secret: %d", code)
		}
		var d map[string]any
		if code := c.do("POST", path, "", map[string]any{"completion_secret": e["completion_secret"], "node_id": node}, &d); code != http.StatusOK {
			t.Fatalf("complete: %d", code)
		}
		if d["ipv4"] != ip {
			t.Fatalf("device ip = %v, want %s", d["ipv4"], ip)
		}
		return d, node
	}
	gpu, _ := enroll(own.AdminKey.Key, "gpu-host", "GPU-Box.local", "100.64.0.20")
	if gpu["given_name"] != "gpu-box-owner-1" {
		t.Fatalf("given name = %v", gpu["given_name"])
	}
	gw, _ := enroll(plat.AdminKey.Key, "gateway", "gateway", "100.64.0.10")
	_ = gw

	// The cell declared each device's tags before its key existed.
	hs.mu.Lock()
	events := slices.Clone(hs.events)
	hs.mu.Unlock()
	for i, e := range events {
		if strings.HasPrefix(e, "key:") && (i == 0 || events[i-1] != "policy") {
			t.Fatalf("a key was minted without a preceding policy apply: %v", events)
		}
	}

	// Tenancy: each org sees only its own devices.
	var devs struct {
		Devices []map[string]any `json:"devices"`
	}
	c.do("GET", "/v1/devices", plat.AdminKey.Key, nil, &devs)
	if len(devs.Devices) != 1 || devs.Devices[0]["name"] != "gateway" {
		t.Fatalf("platform sees %v", devs.Devices)
	}

	// --- Grants ---------------------------------------------------------------------------------------------
	grantReq := map[string]any{
		"grantee_org": plat.OrgShort, "src": map[string]string{"kind": "role", "role": "gateway"},
		"dst_device_id": gpu["id"], "ports": []string{"TCP:10443"}, "expires_at": time.Now().Add(time.Hour),
		"external_ref": "rental-1",
	}
	var g1, g2 map[string]any
	if code := c.do("POST", "/v1/grants", own.AdminKey.Key, grantReq, &g1); code != http.StatusCreated {
		t.Fatalf("create grant: %d", code)
	}
	if g1["enforced"] != true || !strings.Contains(hs.livePolicy(), `"tcp:10443"`) {
		t.Fatalf("grant not enforced: %v", g1)
	}
	if code := c.do("POST", "/v1/grants", own.AdminKey.Key, grantReq, &g2); code != http.StatusCreated || g2["id"] != g1["id"] {
		t.Fatalf("retried grant must return the same grant (%d): %v vs %v", code, g2["id"], g1["id"])
	}
	// The grantee cannot revoke, and nobody can grant access to another org's device.
	if code := c.do("POST", "/v1/grants/"+g1["id"].(string)+"/revoke", plat.AdminKey.Key, nil, nil); code != http.StatusForbidden && code != http.StatusNotFound {
		t.Fatalf("grantee revoke: %d", code)
	}
	stolen := map[string]any{
		"grantee_org": own.OrgShort, "src": map[string]string{"kind": "all"},
		"dst_device_id": gpu["id"], "ports": []string{"tcp:22"}, "expires_at": time.Now().Add(time.Hour),
	}
	if code := c.do("POST", "/v1/grants", plat.AdminKey.Key, stolen, nil); code != http.StatusNotFound {
		t.Fatalf("granting another org's device must fail: %d", code)
	}
	wild := map[string]any{
		"grantee_org": plat.OrgShort, "src": map[string]string{"kind": "all"},
		"dst_device_id": gpu["id"], "ports": []string{"*"}, "expires_at": time.Now().Add(time.Hour),
	}
	if code := c.do("POST", "/v1/grants", own.AdminKey.Key, wild, nil); code != http.StatusUnprocessableEntity {
		t.Fatalf("wildcard ports must be rejected: %d", code)
	}

	var revoked map[string]any
	if code := c.do("POST", "/v1/grants/"+g1["id"].(string)+"/revoke", own.AdminKey.Key, nil, &revoked); code != http.StatusOK {
		t.Fatalf("revoke: %d", code)
	}
	if revoked["enforced"] != true || strings.Contains(hs.livePolicy(), `"tcp:10443"`) {
		t.Fatalf("revocation not enforced: %v", revoked)
	}

	// --- Audit ----------------------------------------------------------------------------------------------
	ownID, _ := store.UUID(own.OrgID)
	err = st.Tx(ctx, store.Scope{System: true}, func(q *dbq.Queries, _ pgx.Tx) error {
		n, err := store.VerifyAuditChain(ctx, q, ownID)
		if err == nil && n < 5 {
			err = fmt.Errorf("only %d audit events for the owner org", n)
		}
		return err
	})
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
}
