//go:build e2e

// Package e2e runs Headmesh against a real Headscale and real Tailscale clients in Docker and checks isolation
// on the wire: what the compiler emits must be accepted by Headscale, tenants must not reach (or even see) each
// other, a grant must open exactly its port, and revocation must close it within the SLO.
//
//	go test -tags e2e -v ./test/e2e/
package e2e

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectinprocess"
	"github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/network"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/headmesh-dev/headmesh/core/cellagent"
	"github.com/headmesh-dev/headmesh/core/cellserver"
	"github.com/headmesh-dev/headmesh/core/policy"
	cellv1 "github.com/headmesh-dev/headmesh/gen/cell/v1"
	"github.com/headmesh-dev/headmesh/gen/cell/v1/cellv1connect"
	"github.com/headmesh-dev/headmesh/internal/headscale"
)

const (
	headscaleImage = "headscale/headscale:v0.29.6"
	tailscaleImage = "tailscale/tailscale:v1.104.2"
	cellName       = "e2e"

	orgA = "oaaaaaaaab"
	orgB = "oaaaaaaaac"
	a1   = "daaaaaaaab"
	a2   = "daaaaaaaac"
	b1   = "daaaaaaaad"
)

// headscaleConfig serves the control plane over TLS with the embedded DERP relay and STUN, as production does:
// Tailscale coordinates NAT traversal through DERP, so a relay is what makes path setup prompt and reliable.
const headscaleConfig = `server_url: https://headscale:8443
listen_addr: 0.0.0.0:8443
metrics_listen_addr: 127.0.0.1:9090
grpc_listen_addr: 127.0.0.1:50443
tls_cert_path: /etc/headscale/tls/server.crt
tls_key_path: /etc/headscale/tls/server.key
noise:
  private_key_path: /var/lib/headscale/noise_private.key
prefixes:
  v4: 100.64.0.0/10
  v6: fd7a:115c:a1e0::/48
  allocation: sequential
derp:
  server:
    enabled: true
    region_id: 999
    region_code: e2e
    region_name: e2e embedded
    verify_clients: true
    stun_listen_addr: 0.0.0.0:3478
    private_key_path: /var/lib/headscale/derp_server_private.key
    automatically_add_embedded_derp_region: true
  urls: []
  paths: []
  auto_update_enabled: false
disable_check_updates: true
database:
  type: sqlite
  sqlite:
    path: /var/lib/headscale/db.sqlite
    write_ahead_log: true
log:
  level: info
policy:
  mode: database
dns:
  magic_dns: true
  base_domain: mesh.test
  override_local_dns: false
unix_socket: /var/run/headscale/headscale.sock
unix_socket_permission: "0777"
logtail:
  enabled: false
`

// snapStore serves a mutable snapshot to the hub.
type snapStore struct {
	mu   sync.Mutex
	snap policy.Snapshot
	rev  uint64
}

func (s *snapStore) set(snap policy.Snapshot) { s.mu.Lock(); s.snap = snap; s.mu.Unlock() }
func (s *snapStore) Snapshot(context.Context, string, time.Time, time.Duration) (policy.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snap, nil
}
func (s *snapStore) RecordHello(context.Context, string, *cellv1.Hello) error       { return nil }
func (s *snapStore) RecordObserved(context.Context, string, *cellv1.Observed) error { return nil }
func (s *snapStore) RecordApplied(context.Context, string, *cellv1.PolicyApplied) error {
	return nil
}

func (s *snapStore) RecordTimeline(context.Context, string, []*cellv1.PolicyRevision) error {
	return nil
}

func (s *snapStore) NextRevision(context.Context, string) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rev += 100
	return s.rev, nil
}

// baseSnapshot is two team orgs; registered marks devices as having Headscale nodes, which lets the compiler emit
// isolation canaries that Headscale itself evaluates.
func baseSnapshot(registered bool) policy.Snapshot {
	s := policy.Snapshot{
		Cell: cellName,
		Orgs: []policy.Org{{Short: orgA, Template: policy.TemplateTeam}, {Short: orgB, Template: policy.TemplateTeam}},
		Devices: []policy.Device{
			{Short: a1, Org: orgA, State: policy.DeviceActive},
			{Short: a2, Org: orgA, State: policy.DeviceActive},
			{Short: b1, Org: orgB, Roles: []string{"gpu-host"}, State: policy.DeviceActive},
		},
	}
	for i := range s.Devices {
		s.Devices[i].Registered = registered
	}
	return s
}

type node struct {
	name string
	ctr  testcontainers.Container
	ip   string
}

func (n *node) exec(ctx context.Context, t *testing.T, cmd ...string) (int, string) {
	t.Helper()
	code, r, err := n.ctr.Exec(ctx, cmd, tcexec.Multiplexed())
	if err != nil {
		t.Fatalf("%s: exec %v: %v", n.name, cmd, err)
	}
	b, _ := io.ReadAll(r)
	return code, string(b)
}

// reach reports whether n can open a TCP connection to ip:port and read the listener's banner.
func (n *node) reach(ctx context.Context, t *testing.T, ip string, port int) bool {
	t.Helper()
	_, out := n.exec(ctx, t, "sh", "-c", fmt.Sprintf("echo x | timeout 5 nc -w 3 %s %d", ip, port))
	return strings.Contains(out, "banner")
}

func TestTenantIsolationOnRealHeadscale(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	quiet := slog.New(slog.DiscardHandler)

	// --- Headscale -------------------------------------------------------------------------------------------
	dir := t.TempDir()
	runDir, dataDir := filepath.Join(dir, "run"), filepath.Join(dir, "data")
	for _, d := range []string{runDir, dataDir} {
		if err := os.MkdirAll(d, 0o777); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(d, 0o777)
	}
	tlsDir := filepath.Join(dir, "tls")
	caPath := writeTestPKI(t, tlsDir, "headscale")
	cfgPath := filepath.Join(dir, "config.yaml")
	for p, body := range map[string]string{cfgPath: headscaleConfig} {
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	net, err := network.New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = net.Remove(context.Background()) })
	hsCtr, err := testcontainers.Run(ctx, headscaleImage,
		testcontainers.WithCmd("serve"),
		network.WithNetwork([]string{"headscale"}, net),
		testcontainers.WithExposedPorts("8443/tcp"),
		testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
			hc.Binds = []string{cfgPath + ":/etc/headscale/config.yaml:ro", tlsDir + ":/etc/headscale/tls:ro", dataDir + ":/var/lib/headscale", runDir + ":/var/run/headscale"}
		}),
	)
	if err != nil {
		t.Fatalf("start headscale: %v", err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			if logs, err := hsCtr.Logs(context.Background()); err == nil {
				b, _ := io.ReadAll(logs)
				t.Logf("headscale logs:\n%s", tail(string(b), 60))
			}
		}
		_ = testcontainers.TerminateContainer(hsCtr)
	})
	socket := filepath.Join(runDir, "headscale.sock")
	waitUntil(t, 60*time.Second, "headscale socket", func() bool { _, err := os.Stat(socket); return err == nil })
	hs, err := headscale.DialUnix(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hs.Close() }()
	waitUntil(t, 30*time.Second, "headscale health", func() bool { return hs.Health(ctx) == nil })

	// --- Hub and cell agent ---------------------------------------------------------------------------------
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	store := &snapStore{snap: baseSnapshot(false)}
	hub, err := cellserver.NewHub(cellserver.Config{
		Signer: priv, Store: store, Logger: quiet,
		Authenticate: func(context.Context) (string, error) { return cellName, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	cs := connect.NewServer()
	cellv1connect.RegisterCellServiceHandler(cs, hub)
	agent, err := cellagent.New(cellagent.Config{
		CellName: cellName, Mode: cellv1.Mode_MODE_ENFORCE, StateDir: t.TempDir(),
		ServerKeys: []ed25519.PublicKey{pub},
	}, hs)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = agent.Run(ctx, cellv1connect.NewCellServiceClient(connect.NewClient(connectinprocess.New(cs))),
			cellagent.HelloInfo{AgentVersion: "e2e"}, cellagent.RunOptions{
				TickInterval:    50 * time.Millisecond,
				ObserveInterval: time.Hour, Logger: quiet,
			})
	}()
	waitUntil(t, 20*time.Second, "agent connection", func() bool { return hub.Connected(cellName) })
	if err := hub.RefreshAndWait(ctx, cellName); err != nil {
		t.Fatalf("real Headscale refused the compiled policy: %v", err)
	}
	live, _ := hs.GetPolicy(ctx)
	if !strings.Contains(live, policy.DeviceTag(b1)) {
		t.Fatalf("live policy does not declare device tags:\n%s", live)
	}

	// --- Tailscale clients ------------------------------------------------------------------------------------
	nodes := map[string]*node{}
	for _, d := range baseSnapshot(false).Devices {
		tags := policy.DeviceTags(d)
		res, err := hub.Do(ctx, cellName, &cellv1.Op{Kind: &cellv1.Op_CreatePreAuthKey{CreatePreAuthKey: &cellv1.CreatePreAuthKey{
			AclTags: tags, Expiration: timestamppb.New(time.Now().Add(10 * time.Minute)),
		}}})
		if err != nil || !res.GetOk() {
			t.Fatalf("mint key for %s: %v %s", d.Short, err, res.GetError())
		}
		ctr, err := testcontainers.Run(ctx, tailscaleImage,
			network.WithNetwork(nil, net),
			testcontainers.WithEnv(map[string]string{
				"TS_AUTHKEY": res.GetPreAuthKey().GetKey(), "TS_HOSTNAME": d.Short, "TS_USERSPACE": "false",
				"TS_STATE_DIR": "/var/lib/tailscale", "TS_ACCEPT_DNS": "false",
				"TS_EXTRA_ARGS": "--login-server=https://headscale:8443", "SSL_CERT_FILE": "/etc/e2e/ca.pem",
			}),
			testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
				hc.Binds = []string{caPath + ":/etc/e2e/ca.pem:ro"}
				hc.CapAdd = []string{"NET_ADMIN", "NET_RAW"}
				hc.Devices = []container.DeviceMapping{{PathOnHost: "/dev/net/tun", PathInContainer: "/dev/net/tun", CgroupPermissions: "rwm"}}
			}),
		)
		if err != nil {
			t.Fatalf("start tailscale %s: %v", d.Short, err)
		}
		t.Cleanup(func() { _ = testcontainers.TerminateContainer(ctr) })
		nodes[d.Short] = &node{name: d.Short, ctr: ctr}
	}
	for _, n := range nodes {
		waitUntil(t, 90*time.Second, n.name+" online", func() bool {
			code, out := n.exec(ctx, t, "tailscale", "ip", "-4")
			if code == 0 && strings.HasPrefix(strings.TrimSpace(out), "100.") {
				n.ip = strings.TrimSpace(out)
				return true
			}
			return false
		})
		for _, port := range []int{8080, 9090} {
			if code, out := n.exec(ctx, t, "sh", "-c",
				fmt.Sprintf("nohup sh -c 'while true; do echo banner | nc -l -p %d; done' >/dev/null 2>&1 &", port)); code != 0 {
				t.Fatalf("listener on %s: %s", n.name, out)
			}
		}
	}

	// --- Canaries: now that every device has a node, Headscale must accept and pass the isolation tests ----------
	store.set(baseSnapshot(true))
	if err := hub.RefreshAndWait(ctx, cellName); err != nil {
		t.Fatalf("Headscale rejected the policy with isolation canaries: %v", err)
	}
	if live, _ := hs.GetPolicy(ctx); !strings.Contains(live, `"tests"`) {
		t.Fatalf("expected canary tests in the live policy:\n%s", live)
	}

	// --- Isolation -------------------------------------------------------------------------------------------
	A1, A2, B1 := nodes[a1], nodes[a2], nodes[b1]
	waitUntil(t, 30*time.Second, "intra-org reachability (team template)", func() bool { return A1.reach(ctx, t, A2.ip, 8080) })
	for _, c := range []struct {
		from *node
		to   *node
	}{{A1, B1}, {A2, B1}, {B1, A1}, {B1, A2}} {
		if c.from.reach(ctx, t, c.to.ip, 8080) {
			t.Errorf("cross-tenant connection %s -> %s succeeded", c.from.name, c.to.name)
		}
	}
	_, status := A1.exec(ctx, t, "tailscale", "status", "--json")
	if strings.Contains(status, B1.ip) {
		t.Errorf("tenant A's netmap includes tenant B's node %s", B1.ip)
	}

	// --- Grant opens exactly its port -------------------------------------------------------------------------
	granted := baseSnapshot(true)
	granted.Grants = []policy.Grant{{
		ID: "g1", Grantor: orgB, Grantee: orgA, Src: policy.DeviceSel(a1), Dst: b1,
		Ports: []policy.Port{"tcp:8080"}, StartsAt: time.Now().Add(-time.Minute), ExpiresAt: time.Now().Add(time.Hour),
	}}
	store.set(granted)
	if err := hub.RefreshAndWait(ctx, cellName); err != nil {
		t.Fatalf("grant policy refused: %v", err)
	}
	// Before the grant the two tenants' nodes cannot see each other, so the first connection also sets up a path.
	opened := time.Now()
	waitUntil(t, 30*time.Second, "granted port opens", func() bool { return A1.reach(ctx, t, B1.ip, 8080) })
	t.Logf("granted port reachable after %s (includes path setup between newly visible peers)", time.Since(opened))
	if A1.reach(ctx, t, B1.ip, 9090) {
		t.Error("a grant for tcp:8080 also opened tcp:9090")
	}
	if A2.reach(ctx, t, B1.ip, 8080) {
		t.Error("a grant for device a1 also admitted a2")
	}
	if B1.reach(ctx, t, A1.ip, 8080) {
		t.Error("a grant is one-directional; the grantor must not reach the grantee")
	}

	// --- Revocation latency -----------------------------------------------------------------------------------
	store.set(baseSnapshot(true))
	start := time.Now()
	if err := hub.RefreshAndWait(ctx, cellName); err != nil {
		t.Fatalf("revocation policy refused: %v", err)
	}
	waitUntil(t, 30*time.Second, "revoked port closes", func() bool { return !A1.reach(ctx, t, B1.ip, 8080) })
	latency := time.Since(start)
	t.Logf("revocation enforced on the wire in %s (SLO p99 < 5s; includes a probe timeout of up to 5s)", latency)
	if latency > 15*time.Second {
		t.Errorf("revocation took %s", latency)
	}
}

func waitUntil(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", d, what)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// writeTestPKI creates a throwaway CA and a server certificate for host in dir and returns the CA path.
func writeTestPKI(t *testing.T, dir, host string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "headmesh e2e CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name, typ string, b []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: b}), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("server.crt", "CERTIFICATE", der)
	write("server.key", "PRIVATE KEY", keyDER)
	return write("ca.pem", "CERTIFICATE", caDER)
}
