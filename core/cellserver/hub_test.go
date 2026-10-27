package cellserver_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectinprocess"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/headmesh-dev/headmesh/core/cellagent"
	"github.com/headmesh-dev/headmesh/core/cellserver"
	"github.com/headmesh-dev/headmesh/core/policy"
	cellv1 "github.com/headmesh-dev/headmesh/gen/cell/v1"
	"github.com/headmesh-dev/headmesh/gen/cell/v1/cellv1connect"
	headscalev1 "github.com/headmesh-dev/headmesh/gen/headscale/v1"
	"github.com/headmesh-dev/headmesh/internal/headscale"
)

// memStore serves a mutable snapshot and records what the agent reports.
type memStore struct {
	mu       sync.Mutex
	snap     policy.Snapshot
	rev      uint64
	applied  []*cellv1.PolicyApplied
	observed int
}

func (m *memStore) Snapshot(context.Context, string, time.Time, time.Duration) (policy.Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snap, nil
}

func (m *memStore) RecordHello(context.Context, string, *cellv1.Hello) error { return nil }

func (m *memStore) RecordObserved(context.Context, string, *cellv1.Observed) error {
	m.mu.Lock()
	m.observed++
	m.mu.Unlock()
	return nil
}

func (m *memStore) RecordApplied(_ context.Context, _ string, a *cellv1.PolicyApplied) error {
	m.mu.Lock()
	m.applied = append(m.applied, a)
	m.mu.Unlock()
	return nil
}

func (m *memStore) RecordTimeline(context.Context, string, []*cellv1.PolicyRevision) error {
	return nil
}

func (m *memStore) NextRevision(context.Context, string) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rev += 100
	return m.rev, nil
}

// fakeHS is a minimal Headscale for the agent side.
type fakeHS struct {
	mu     sync.Mutex
	policy string
}

func (f *fakeHS) ListNodes(context.Context) ([]*headscalev1.Node, error) { return nil, nil }
func (f *fakeHS) CreatePreAuthKey(_ context.Context, r headscale.PreAuthKeyRequest) (*headscalev1.PreAuthKey, error) {
	return &headscalev1.PreAuthKey{Id: 9, Key: "minted-" + strings.Join(r.Tags, ","), Expiration: timestamppb.New(r.Expiration)}, nil
}

func (f *fakeHS) SetTags(context.Context, uint64, []string) (*headscalev1.Node, error) {
	return &headscalev1.Node{}, nil
}

func (f *fakeHS) RenameNode(context.Context, uint64, string) (*headscalev1.Node, error) {
	return &headscalev1.Node{}, nil
}
func (f *fakeHS) DeleteNode(context.Context, uint64) error { return nil }
func (f *fakeHS) ExpireNode(context.Context, uint64, bool) (*headscalev1.Node, error) {
	return &headscalev1.Node{}, nil
}

func (f *fakeHS) SetApprovedRoutes(context.Context, uint64, []string) (*headscalev1.Node, error) {
	return &headscalev1.Node{}, nil
}

func (f *fakeHS) GetPolicy(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.policy, nil
}

func (f *fakeHS) SetPolicy(_ context.Context, p string) error {
	f.mu.Lock()
	f.policy = p
	f.mu.Unlock()
	return nil
}
func (f *fakeHS) CheckPolicy(context.Context, string) error { return nil }
func (f *fakeHS) Health(context.Context) error              { return nil }

func (f *fakeHS) current() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.policy
}

const (
	platform = "xaaaaaaaab"
	owner    = "paaaaaaaab"
	gateway  = "daaaaaaaab"
	gpu      = "daaaaaaaac"
)

func snapshot(withGrant bool, now time.Time) policy.Snapshot {
	s := policy.Snapshot{
		Cell: "hs1",
		Orgs: []policy.Org{
			{Short: platform, Template: policy.TemplateLocked, Platform: true},
			{Short: owner, Template: policy.TemplateLocked, Parent: platform},
		},
		Devices: []policy.Device{
			{Short: gateway, Org: platform, Roles: []string{"gateway"}, State: policy.DeviceActive},
			{Short: gpu, Org: owner, Roles: []string{"gpu-host"}, State: policy.DeviceActive},
		},
		PlatformRules: []policy.PlatformRule{{
			ID: "ssh", Platform: platform,
			Src: policy.PlatformSelector{Scope: policy.ScopePlatform, Role: "gateway"},
			Dst: policy.PlatformSelector{Scope: policy.ScopeChildren, Role: "gpu-host"}, Ports: []policy.Port{"tcp:22"},
		}},
	}
	if withGrant {
		s.Grants = []policy.Grant{{
			ID: "rental", Grantor: owner, Grantee: platform, Src: policy.Role("gateway"), Dst: gpu,
			Ports: []policy.Port{"tcp:10443"}, StartsAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
		}}
	}
	return s
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestHubAndAgentEndToEnd(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	store := &memStore{snap: snapshot(true, now)}
	hub, err := cellserver.NewHub(cellserver.Config{
		Signer: priv, Store: store, Logger: slog.New(slog.DiscardHandler),
		Authenticate: func(context.Context) (string, error) { return "hs1", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	server := connect.NewServer()
	cellv1connect.RegisterCellServiceHandler(server, hub)
	client := cellv1connect.NewCellServiceClient(connect.NewClient(connectinprocess.New(server)))

	hs := &fakeHS{}
	agent, err := cellagent.New(cellagent.Config{
		CellName: "hs1", Mode: cellv1.Mode_MODE_ENFORCE, StateDir: t.TempDir(), ServerKeys: []ed25519.PublicKey{pub},
		Invariants: []cellagent.Invariant{{
			Name: "gateway ssh", Src: []string{policy.RoleTag(platform, "gateway")},
			Dst: []string{policy.RoleTag(owner, "gpu-host")}, Proto: "tcp", Port: 22,
		}},
	}, hs)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = agent.Run(ctx, client, cellagent.HelloInfo{AgentVersion: "test"}, cellagent.RunOptions{
			TickInterval: 10 * time.Millisecond, ObserveInterval: time.Hour, Logger: slog.New(slog.DiscardHandler),
		})
	}()

	// 1. On connect the hub pushes the compiled policy and the agent applies it.
	waitFor(t, "initial policy", func() bool { return strings.Contains(hs.current(), `"tcp:10443"`) })

	// 2. Operations round-trip with signatures; the agent mints a tagged key.
	opCtx, opCancel := context.WithTimeout(ctx, 5*time.Second)
	defer opCancel()
	res, err := hub.Do(opCtx, "hs1", &cellv1.Op{Kind: &cellv1.Op_CreatePreAuthKey{CreatePreAuthKey: &cellv1.CreatePreAuthKey{
		AclTags: []string{policy.OrgTag(owner)}, Expiration: timestamppb.New(time.Now().Add(5 * time.Minute)),
	}}})
	if err != nil || !res.GetOk() || !strings.HasPrefix(res.GetPreAuthKey().GetKey(), "minted-") {
		t.Fatalf("pre-auth key op: res=%v err=%v", res, err)
	}

	// 3. Revoking the grant and refreshing removes it from the live policy.
	store.mu.Lock()
	store.snap = snapshot(false, now)
	store.mu.Unlock()
	if err := hub.Refresh(ctx, "hs1"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "revocation", func() bool {
		p := hs.current()
		return p != "" && !strings.Contains(p, `"tcp:10443"`) && strings.Contains(p, `"tcp:22"`)
	})

	// 4. A refused op is reported, not executed.
	res, err = hub.Do(opCtx, "hs1", &cellv1.Op{Kind: &cellv1.Op_SetApprovedRoutes{SetApprovedRoutes: &cellv1.SetApprovedRoutes{
		NodeId: 3, Routes: []string{"0.0.0.0/0"},
	}}})
	if err != nil || res.GetOk() || !res.GetRefusedByGuardrail() {
		t.Fatalf("exit route must be refused by the cell: res=%v err=%v", res, err)
	}
	if !hub.Connected("hs1") || hub.Connected("other") {
		t.Fatal("connection tracking is wrong")
	}
}

func TestHubRejectsCellImpersonation(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	hub, err := cellserver.NewHub(cellserver.Config{
		Signer: priv, Store: &memStore{}, Logger: slog.New(slog.DiscardHandler),
		Authenticate: func(context.Context) (string, error) { return "hs1", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	server := connect.NewServer()
	cellv1connect.RegisterCellServiceHandler(server, hub)
	client := cellv1connect.NewCellServiceClient(connect.NewClient(connectinprocess.New(server)))
	stream, err := client.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	if err := stream.Send(&cellv1.SyncRequest{Msg: &cellv1.SyncRequest_Hello{Hello: &cellv1.Hello{CellName: "xcelsior"}}}); err != nil {
		t.Fatal(err)
	}
	_, err = stream.Receive()
	var cerr *connect.Error
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodePermissionDenied {
		t.Fatalf("a cell speaking for another cell must be refused, got %v", err)
	}
	if hub.Connected("xcelsior") {
		t.Fatal("an impersonating session must never be registered")
	}
	if _, err := hub.Do(context.Background(), "xcelsior", &cellv1.Op{}); !errors.Is(err, cellserver.ErrCellOffline) {
		t.Fatalf("ops to an offline cell must fail fast, got %v", err)
	}
}
