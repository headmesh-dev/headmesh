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

	// --- Rules: access inside one org ------------------------------------------------------------------------
	laptop, _ := enroll(own.AdminKey.Key, "ops", "laptop", "100.64.0.21")
	ruleReq := map[string]any{
		"src":   map[string]string{"kind": "role", "role": "ops"},
		"dst":   map[string]string{"kind": "device", "device": gpu["id"].(string)},
		"ports": []string{"TCP:2222"},
	}
	var rule map[string]any
	if code := c.do("POST", "/v1/rules", own.AdminKey.Key, ruleReq, &rule); code != http.StatusCreated {
		t.Fatalf("create rule: %d", code)
	}
	if rule["enforced"] != true || rule["ports"].([]any)[0] != "tcp:2222" || !strings.Contains(hs.livePolicy(), `"tcp:2222"`) {
		t.Fatalf("rule not enforced: %v", rule)
	}
	ruleID := rule["id"].(string)

	// --- Access check: the answer and what gives it, without revealing other orgs' devices -------------------
	type accessOut struct {
		Allowed bool `json:"allowed"`
		Because []struct {
			Kind string `json:"kind"`
			ID   string `json:"id"`
		} `json:"because"`
		Detail string `json:"detail"`
	}
	check := func(key, src, dst, port string) (accessOut, int) {
		t.Helper()
		var out accessOut
		code := c.do("GET", "/v1/access-check?src="+src+"&dst="+dst+"&port="+port, key, nil, &out)
		return out, code
	}
	if a, code := check(own.AdminKey.Key, laptop["id"].(string), gpu["id"].(string), "tcp:2222"); code != http.StatusOK ||
		!a.Allowed || len(a.Because) != 1 || a.Because[0].Kind != "rule" || a.Because[0].ID != ruleID {
		t.Fatalf("laptop → gpu tcp:2222: %d %+v", code, a)
	}
	if a, code := check(own.AdminKey.Key, laptop["id"].(string), gpu["id"].(string), "tcp:22"); code != http.StatusOK || a.Allowed || a.Detail == "" {
		t.Fatalf("laptop → gpu tcp:22 should be refused: %d %+v", code, a)
	}
	var g3 map[string]any
	grant3 := map[string]any{
		"grantee_org": plat.OrgShort, "src": map[string]string{"kind": "role", "role": "gateway"},
		"dst_device_id": gpu["id"], "ports": []string{"tcp:10443"}, "expires_at": time.Now().Add(time.Hour),
		"external_ref": "rental-3",
	}
	if code := c.do("POST", "/v1/grants", own.AdminKey.Key, grant3, &g3); code != http.StatusCreated {
		t.Fatalf("create grant 3: %d", code)
	}
	if a, code := check(plat.AdminKey.Key, gw["id"].(string), gpu["id"].(string), "tcp:10443"); code != http.StatusOK ||
		!a.Allowed || len(a.Because) != 1 || a.Because[0].Kind != "grant" || a.Because[0].ID != g3["id"] {
		t.Fatalf("grantee's gateway → gpu tcp:10443: %d %+v", code, a)
	}
	refused, _ := check(plat.AdminKey.Key, gw["id"].(string), laptop["id"].(string), "tcp:10443")
	missing, code := check(plat.AdminKey.Key, gw["id"].(string), "0193f0c4-0000-7000-8000-000000000001", "tcp:10443")
	if code != http.StatusOK || refused.Allowed || missing.Allowed || refused.Detail != missing.Detail {
		t.Fatalf("an unknown device must read like another org's refused device: %+v vs %+v", missing, refused)
	}
	if _, code := check(plat.AdminKey.Key, laptop["id"].(string), gpu["id"].(string), "tcp:2222"); code != http.StatusNotFound {
		t.Fatalf("checking two devices of another org: %d", code)
	}
	if _, code := check(own.AdminKey.Key, laptop["id"].(string), gpu["id"].(string), "tcp:1-2"); code != http.StatusUnprocessableEntity {
		t.Fatalf("a port range: %d", code)
	}
	if code := c.do("POST", "/v1/grants/"+g3["id"].(string)+"/revoke", own.AdminKey.Key, nil, nil); code != http.StatusOK {
		t.Fatalf("revoke grant 3: %d", code)
	}
	// Another org can't see the rule, delete it, or aim a rule of its own at a device it doesn't own.
	var rules struct {
		Rules []map[string]any `json:"rules"`
	}
	if code := c.do("GET", "/v1/rules", plat.AdminKey.Key, nil, &rules); code != http.StatusOK || len(rules.Rules) != 0 {
		t.Fatalf("the platform org lists %d rules (%d)", len(rules.Rules), code)
	}
	if code := c.do("GET", "/v1/rules/"+ruleID, plat.AdminKey.Key, nil, nil); code != http.StatusNotFound {
		t.Fatalf("another org read the rule: %d", code)
	}
	if code := c.do("DELETE", "/v1/rules/"+ruleID, plat.AdminKey.Key, nil, nil); code != http.StatusNotFound {
		t.Fatalf("another org deleted the rule: %d", code)
	}
	reach := map[string]any{
		"src": map[string]string{"kind": "all"}, "dst": map[string]string{"kind": "device", "device": gpu["id"].(string)},
		"ports": []string{"tcp:22"},
	}
	if code := c.do("POST", "/v1/rules", plat.AdminKey.Key, reach, nil); code != http.StatusUnprocessableEntity {
		t.Fatalf("a rule aimed at another org's device: %d", code)
	}
	// rules:read lists but can't write.
	var reader struct {
		Key string `json:"key"`
	}
	if code := c.do("POST", "/v1/api-keys", own.AdminKey.Key, map[string]any{"name": "rules reader", "scopes": []string{"rules:read"}}, &reader); code != http.StatusCreated {
		t.Fatalf("create rules:read key: %d", code)
	}
	if code := c.do("GET", "/v1/rules", reader.Key, nil, &rules); code != http.StatusOK || len(rules.Rules) != 1 || rules.Rules[0]["id"] != ruleID {
		t.Fatalf("rules:read list: %d, %v", code, rules.Rules)
	}
	if code := c.do("POST", "/v1/rules", reader.Key, ruleReq, nil); code != http.StatusForbidden {
		t.Fatalf("rules:read created a rule: %d", code)
	}
	var deleted map[string]any
	if code := c.do("DELETE", "/v1/rules/"+ruleID, own.AdminKey.Key, nil, &deleted); code != http.StatusOK {
		t.Fatalf("delete rule: %d", code)
	}
	if deleted["enforced"] != true || strings.Contains(hs.livePolicy(), `"tcp:2222"`) {
		t.Fatalf("rule deletion not enforced: %v", deleted)
	}
	if code := c.do("GET", "/v1/rules/"+ruleID, own.AdminKey.Key, nil, nil); code != http.StatusNotFound {
		t.Fatalf("deleted rule still readable: %d", code)
	}

	// --- Expiry sweep: a grant that lapsed while nothing was running is audited exactly once -----------------
	gpuID, _ := store.UUID(gpu["id"].(string))
	platID, _ := store.UUID(plat.OrgID)
	ownOrg, _ := store.UUID(own.OrgID)
	srcJSON, _ := json.Marshal(store.SelectorJSON{Kind: "all"})
	if err := st.Tx(ctx, store.Scope{System: true}, func(q *dbq.Queries, _ pgx.Tx) error {
		_, err := q.CreateGrant(ctx, dbq.CreateGrantParams{
			GrantorOrgID: ownOrg, GranteeOrgID: platID, Src: srcJSON,
			DstDeviceID: gpuID, Ports: []string{"tcp:22"}, StartsAt: time.Now().Add(-2 * time.Hour), ExpiresAt: time.Now().Add(-time.Hour),
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n, err := svc.SweepExpiredGrants(ctx); err != nil || n != 1 {
		t.Fatalf("first sweep audited %d (%v), want 1", n, err)
	}
	if n, err := svc.SweepExpiredGrants(ctx); err != nil || n != 0 {
		t.Fatalf("second sweep audited %d (%v), want 0", n, err)
	}
	gateCtx, gateCancel := context.WithTimeout(ctx, 20*time.Second)
	defer gateCancel()
	if err := svc.AwaitCellsCurrent(gateCtx, hub, quiet); err != nil {
		t.Fatalf("recovery gate: %v", err)
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

	// --- Audit over the API: pages, isolation, verification and the read scope ---------------------------------
	type auditPage struct {
		Events []struct {
			ID     int64  `json:"id"`
			Action string `json:"action"`
			Hash   string `json:"hash"`
		} `json:"events"`
		NextAfter int64 `json:"next_after"`
	}
	var p1, p2 auditPage
	if code := c.do("GET", "/v1/audit-events?limit=2", own.AdminKey.Key, nil, &p1); code != http.StatusOK || len(p1.Events) != 2 {
		t.Fatalf("audit page 1: %d with %d events", code, len(p1.Events))
	}
	if code := c.do("GET", fmt.Sprintf("/v1/audit-events?after=%d", p1.NextAfter), own.AdminKey.Key, nil, &p2); code != http.StatusOK ||
		len(p2.Events) < 3 || p2.Events[0].ID <= p1.NextAfter {
		t.Fatalf("audit page 2: %d, %+v", code, p2)
	}
	ownIDs := map[int64]bool{}
	for _, e := range append(p1.Events, p2.Events...) {
		ownIDs[e.ID] = true
		if len(e.Hash) != 64 {
			t.Fatalf("event %d has hash %q", e.ID, e.Hash)
		}
	}
	var platPage auditPage
	if code := c.do("GET", "/v1/audit-events?limit=500", plat.AdminKey.Key, nil, &platPage); code != http.StatusOK || len(platPage.Events) == 0 {
		t.Fatalf("platform audit: %d with %d events", code, len(platPage.Events))
	}
	for _, e := range platPage.Events {
		if ownIDs[e.ID] {
			t.Fatalf("the platform org can read the owner org's audit event %d (%s)", e.ID, e.Action)
		}
	}
	var verified struct {
		Verified bool   `json:"verified"`
		Checked  int    `json:"checked"`
		Problem  string `json:"problem"`
	}
	if code := c.do("GET", "/v1/audit-events/verify", own.AdminKey.Key, nil, &verified); code != http.StatusOK ||
		!verified.Verified || verified.Checked != len(ownIDs) {
		t.Fatalf("audit verify: %d, %+v (want %d events)", code, verified, len(ownIDs))
	}
	var narrow struct {
		Key string `json:"key"`
	}
	if code := c.do("POST", "/v1/api-keys", own.AdminKey.Key, map[string]any{"name": "devices only", "scopes": []string{"devices:read"}}, &narrow); code != http.StatusCreated {
		t.Fatalf("create devices-only key: %d", code)
	}
	if code := c.do("GET", "/v1/audit-events", narrow.Key, nil, nil); code != http.StatusForbidden {
		t.Fatalf("a key without audit:read read the audit log: %d", code)
	}
}
