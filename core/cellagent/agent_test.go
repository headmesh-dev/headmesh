package cellagent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	cellv1 "github.com/headmesh-dev/headmesh/gen/cell/v1"
	headscalev1 "github.com/headmesh-dev/headmesh/gen/headscale/v1"
	"github.com/headmesh-dev/headmesh/internal/headscale"
)

// fakeHeadscale records calls and can be told to reject CheckPolicy.
type fakeHeadscale struct {
	mu          sync.Mutex
	policy      string
	setCalls    int
	checkErr    error
	deleted     []uint64
	tagged      map[uint64][]string
	createdKeys []headscale.PreAuthKeyRequest
}

func newFake() *fakeHeadscale { return &fakeHeadscale{tagged: map[uint64][]string{}} }

func (f *fakeHeadscale) ListNodes(context.Context) ([]*headscalev1.Node, error) {
	return []*headscalev1.Node{{Id: 1, Name: "gpu1", Tags: []string{"tag:hm-o-aaaaaaaaaa"}}}, nil
}

func (f *fakeHeadscale) CreatePreAuthKey(_ context.Context, r headscale.PreAuthKeyRequest) (*headscalev1.PreAuthKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createdKeys = append(f.createdKeys, r)
	return &headscalev1.PreAuthKey{Id: uint64(len(f.createdKeys)), Key: "hskey-secret", Expiration: timestamppb.New(r.Expiration)}, nil
}

func (f *fakeHeadscale) SetTags(_ context.Context, id uint64, tags []string) (*headscalev1.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tagged[id] = tags
	return &headscalev1.Node{Id: id, Tags: tags}, nil
}

func (f *fakeHeadscale) RenameNode(_ context.Context, id uint64, name string) (*headscalev1.Node, error) {
	return &headscalev1.Node{Id: id, GivenName: name}, nil
}

func (f *fakeHeadscale) DeleteNode(_ context.Context, id uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, id)
	return nil
}

func (f *fakeHeadscale) ExpireNode(_ context.Context, id uint64, _ bool) (*headscalev1.Node, error) {
	return &headscalev1.Node{Id: id}, nil
}

func (f *fakeHeadscale) SetApprovedRoutes(_ context.Context, id uint64, routes []string) (*headscalev1.Node, error) {
	return &headscalev1.Node{Id: id, ApprovedRoutes: routes}, nil
}

func (f *fakeHeadscale) GetPolicy(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.policy, nil
}

func (f *fakeHeadscale) SetPolicy(_ context.Context, p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.policy = p
	f.setCalls++
	return nil
}

func (f *fakeHeadscale) CheckPolicy(context.Context, string) error { return f.checkErr }
func (f *fakeHeadscale) Health(context.Context) error              { return nil }

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type harness struct {
	agent *Agent
	hs    *fakeHeadscale
	clk   *clock
	key   ed25519.PrivateKey
	dir   string
}

func newHarness(t *testing.T, mutate func(*Config)) *harness {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	dir := t.TempDir()
	cfg := Config{
		CellName: "test", Mode: cellv1.Mode_MODE_ENFORCE, StateDir: dir, ServerKeys: []ed25519.PublicKey{pub},
		ProtectedNodes: []uint64{100},
		Invariants: []Invariant{{
			Name: "control reaches hosts", Src: []string{"tag:hm-r-xaaaaaaaab-control"},
			Dst: []string{"tag:hm-r-paaaaaaaab-gpu-host"}, Proto: "tcp", Port: 22,
		}},
		Now: clk.Now,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	hs := newFake()
	a, err := New(cfg, hs)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.AcceptServerKey(pub); err != nil {
		t.Fatal(err)
	}
	return &harness{agent: a, hs: hs, clk: clk, key: priv, dir: dir}
}

const goodPolicy = `{"tagOwners": {"tag:hm-r-xaaaaaaaab-control": [], "tag:hm-r-paaaaaaaab-gpu-host": []},
 "grants": [{"src": ["tag:hm-r-xaaaaaaaab-control"], "dst": ["tag:hm-r-paaaaaaaab-gpu-host"], "ip": ["tcp:22"]}]}`

const breaksInvariant = `{"tagOwners": {}, "grants": []}`

func rev(n uint64, body string, at time.Time) *cellv1.PolicyRevision {
	sum := sha256.Sum256([]byte(body))
	return &cellv1.PolicyRevision{Revision: n, Sha256: hex.EncodeToString(sum[:]), Hujson: body, EffectiveAt: timestamppb.New(at)}
}

func (h *harness) op(t *testing.T, id string, mutate func(*cellv1.Op)) *cellv1.Op {
	t.Helper()
	op := &cellv1.Op{Id: id, NotAfter: timestamppb.New(h.clk.Now().Add(time.Minute))}
	mutate(op)
	if err := SignOp(h.key, op); err != nil {
		t.Fatal(err)
	}
	return op
}

func TestTimelineAppliesOnScheduleAndSkipsSuperseded(t *testing.T) {
	h := newHarness(t, nil)
	now := h.clk.Now()
	second := strings.Replace(goodPolicy, `"tcp:22"`, `"tcp:22", "tcp:2222"`, 1)
	third := strings.Replace(goodPolicy, `"tcp:22"`, `"tcp:22", "tcp:3333"`, 1)
	if err := h.agent.SetTimeline([]*cellv1.PolicyRevision{
		rev(1, goodPolicy, now), rev(2, second, now.Add(time.Minute)), rev(3, third, now.Add(2*time.Minute)),
	}); err != nil {
		t.Fatal(err)
	}
	if r := h.agent.Tick(context.Background()); r == nil || !r.GetOk() || r.GetRevision() != 1 {
		t.Fatalf("revision 1 should apply now: %v", r)
	}
	if r := h.agent.Tick(context.Background()); r != nil {
		t.Fatalf("nothing is due yet, got %v", r)
	}
	// The agent was "offline" past both boundaries: only the latest due revision is applied.
	h.clk.Add(5 * time.Minute)
	if r := h.agent.Tick(context.Background()); r == nil || r.GetRevision() != 3 {
		t.Fatalf("revision 3 should apply, got %v", r)
	}
	if h.hs.setCalls != 2 {
		t.Fatalf("SetPolicy called %d times, want 2", h.hs.setCalls)
	}
	if st := h.agent.Status(); st.ActiveRevision != 3 || st.PendingCount != 0 {
		t.Fatalf("status = %+v", st)
	}
}

func TestPolicyGuardrails(t *testing.T) {
	cases := map[string]struct {
		body    string
		sha     func(string) string
		checker error
		mode    cellv1.Mode
		frozen  bool
	}{
		"invariant broken":            {body: breaksInvariant},
		"hash mismatch":               {body: goodPolicy, sha: func(string) string { return strings.Repeat("0", 64) }},
		"no grants means allow-all":   {body: `{"tagOwners": {}}`},
		"headscale rejects":           {body: goodPolicy, checker: errors.New("policy test failed")},
		"observe mode":                {body: goodPolicy, mode: cellv1.Mode_MODE_OBSERVE},
		"frozen by operator":          {body: goodPolicy, frozen: true},
		"legacy acls section present": {body: `{"acls": [], "grants": []}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, func(c *Config) {
				if tc.mode != cellv1.Mode_MODE_UNSPECIFIED {
					c.Mode = tc.mode
				}
			})
			h.hs.checkErr = tc.checker
			if tc.frozen {
				if err := h.agent.Freeze(); err != nil {
					t.Fatal(err)
				}
			}
			r := rev(1, tc.body, h.clk.Now())
			if tc.sha != nil {
				r.Sha256 = tc.sha(tc.body)
			}
			if err := h.agent.SetTimeline([]*cellv1.PolicyRevision{r}); err != nil {
				t.Fatal(err)
			}
			res := h.agent.Tick(context.Background())
			if res == nil || res.GetOk() || !res.GetRefusedByGuardrail() {
				t.Fatalf("want a guardrail refusal, got %v", res)
			}
			if h.hs.setCalls != 0 {
				t.Fatal("a refused policy must never reach Headscale")
			}
		})
	}
}

func TestRollbackRestoresLastKnownGood(t *testing.T) {
	h := newHarness(t, nil)
	if err := h.agent.SetTimeline([]*cellv1.PolicyRevision{rev(1, goodPolicy, h.clk.Now())}); err != nil {
		t.Fatal(err)
	}
	if r := h.agent.Tick(context.Background()); !r.GetOk() {
		t.Fatalf("apply: %v", r)
	}
	h.hs.policy = "something an operator broke by hand"
	if err := h.agent.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	if h.hs.policy != goodPolicy {
		t.Fatal("rollback did not restore the last-known-good policy")
	}
	info, err := os.Stat(filepath.Join(h.dir, "last-known-good.hujson"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("last-known-good must be stored owner-only (%v, %v)", info, err)
	}
}

func TestOperationsAreSignedSingleUseAndUnexpired(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	del := func(op *cellv1.Op) { op.Kind = &cellv1.Op_DeleteNode{DeleteNode: &cellv1.DeleteNode{NodeId: 7}} }

	if r := h.agent.HandleOp(ctx, h.op(t, "op-1", del)); !r.GetOk() {
		t.Fatalf("valid op failed: %v", r)
	}
	if r := h.agent.HandleOp(ctx, h.op(t, "op-1", del)); r.GetOk() || !r.GetRefusedByGuardrail() {
		t.Fatalf("replayed op must be refused: %v", r)
	}
	forged := h.op(t, "op-2", del)
	forged.GetDeleteNode().NodeId = 8 // tamper after signing
	if r := h.agent.HandleOp(ctx, forged); r.GetOk() {
		t.Fatal("an op modified after signing must be refused")
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	wrongKey := &cellv1.Op{Id: "op-3", NotAfter: timestamppb.New(h.clk.Now().Add(time.Minute))}
	del(wrongKey)
	if err := SignOp(other, wrongKey); err != nil {
		t.Fatal(err)
	}
	if r := h.agent.HandleOp(ctx, wrongKey); r.GetOk() {
		t.Fatal("an op signed by an unpinned key must be refused")
	}
	expired := h.op(t, "op-4", del)
	h.clk.Add(2 * time.Minute)
	if r := h.agent.HandleOp(ctx, expired); r.GetOk() {
		t.Fatal("an expired op must be refused")
	}
	if len(h.hs.deleted) != 1 {
		t.Fatalf("exactly one delete should have reached Headscale, got %v", h.hs.deleted)
	}

	// Replay protection survives a restart.
	again, err := New(h.agent.cfg, h.hs)
	if err != nil {
		t.Fatal(err)
	}
	if err := again.AcceptServerKey(h.agent.cfg.ServerKeys[0]); err != nil {
		t.Fatal(err)
	}
	h.clk.Add(-2 * time.Minute)
	if r := again.HandleOp(ctx, h.op(t, "op-1", del)); r.GetOk() {
		t.Fatal("an op executed before a restart must not run again")
	}
}

func TestUnpinnedServerKeyIsRejected(t *testing.T) {
	h := newHarness(t, nil)
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	if err := h.agent.AcceptServerKey(pub); !errors.Is(err, ErrRefused) {
		t.Fatalf("unpinned key: %v", err)
	}
}

func TestOperationGuardrails(t *testing.T) {
	ctx := context.Background()
	future := func(h *harness, d time.Duration) *timestamppb.Timestamp { return timestamppb.New(h.clk.Now().Add(d)) }
	cases := map[string]func(h *harness, op *cellv1.Op){
		"delete protected node": func(_ *harness, op *cellv1.Op) {
			op.Kind = &cellv1.Op_DeleteNode{DeleteNode: &cellv1.DeleteNode{NodeId: 100}}
		},
		"retag protected node": func(_ *harness, op *cellv1.Op) {
			op.Kind = &cellv1.Op_SetTags{SetTags: &cellv1.SetTags{NodeId: 100, Tags: []string{"tag:hm-o-aaaaaaaaaa"}}}
		},
		"foreign tag": func(_ *harness, op *cellv1.Op) {
			op.Kind = &cellv1.Op_SetTags{SetTags: &cellv1.SetTags{NodeId: 5, Tags: []string{"tag:admin"}}}
		},
		"untagged node": func(_ *harness, op *cellv1.Op) {
			op.Kind = &cellv1.Op_SetTags{SetTags: &cellv1.SetTags{NodeId: 5}}
		},
		"exit node route": func(_ *harness, op *cellv1.Op) {
			op.Kind = &cellv1.Op_SetApprovedRoutes{SetApprovedRoutes: &cellv1.SetApprovedRoutes{NodeId: 5, Routes: []string{"0.0.0.0/0"}}}
		},
		"untagged pre-auth key": func(h *harness, op *cellv1.Op) {
			op.Kind = &cellv1.Op_CreatePreAuthKey{CreatePreAuthKey: &cellv1.CreatePreAuthKey{Expiration: future(h, 5*time.Minute)}}
		},
		"pre-auth key with foreign tag": func(h *harness, op *cellv1.Op) {
			op.Kind = &cellv1.Op_CreatePreAuthKey{CreatePreAuthKey: &cellv1.CreatePreAuthKey{AclTags: []string{"tag:server"}, Expiration: future(h, 5*time.Minute)}}
		},
		"long-lived pre-auth key": func(h *harness, op *cellv1.Op) {
			op.Kind = &cellv1.Op_CreatePreAuthKey{CreatePreAuthKey: &cellv1.CreatePreAuthKey{AclTags: []string{"tag:hm-o-aaaaaaaaaa"}, Expiration: future(h, 90*24*time.Hour)}}
		},
		"expire protected node": func(_ *harness, op *cellv1.Op) {
			op.Kind = &cellv1.Op_ExpireNode{ExpireNode: &cellv1.ExpireNode{NodeId: 100}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)
			op := h.op(t, "op", func(op *cellv1.Op) { mutate(h, op) })
			if r := h.agent.HandleOp(ctx, op); r.GetOk() || !r.GetRefusedByGuardrail() {
				t.Fatalf("want guardrail refusal, got %v", r)
			}
		})
	}
}

func TestDeleteRateLimit(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.MaxDeletesPerMinute = 3 })
	ctx := context.Background()
	refused := 0
	for i := range 5 {
		op := h.op(t, fmt.Sprintf("del-%d", i), func(op *cellv1.Op) {
			op.Kind = &cellv1.Op_DeleteNode{DeleteNode: &cellv1.DeleteNode{NodeId: uint64(10 + i)}}
		})
		if r := h.agent.HandleOp(ctx, op); !r.GetOk() {
			refused++
		}
	}
	if refused != 2 || len(h.hs.deleted) != 3 {
		t.Fatalf("want 3 deletes then 2 refusals, got %d deletes, %d refused", len(h.hs.deleted), refused)
	}
	h.clk.Add(61 * time.Second)
	op := h.op(t, "del-later", func(op *cellv1.Op) {
		op.Kind = &cellv1.Op_DeleteNode{DeleteNode: &cellv1.DeleteNode{NodeId: 99}}
	})
	if r := h.agent.HandleOp(ctx, op); !r.GetOk() {
		t.Fatalf("the window should have reset: %v", r)
	}
}

func TestPreAuthKeyIsReturnedOnlyInTheResult(t *testing.T) {
	h := newHarness(t, nil)
	op := h.op(t, "key-1", func(op *cellv1.Op) {
		op.Kind = &cellv1.Op_CreatePreAuthKey{CreatePreAuthKey: &cellv1.CreatePreAuthKey{
			AclTags: []string{"tag:hm-o-aaaaaaaaaa", "tag:hm-n-baaaaaaaaa"}, Expiration: timestamppb.New(h.clk.Now().Add(5 * time.Minute)),
		}}
	})
	r := h.agent.HandleOp(context.Background(), op)
	if !r.GetOk() || r.GetPreAuthKey().GetKey() != "hskey-secret" {
		t.Fatalf("key not returned: %v", r)
	}
	entries, err := os.ReadDir(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(h.dir, e.Name()))
		if strings.Contains(string(b), "hskey-secret") {
			t.Fatalf("pre-auth key value leaked into state file %s", e.Name())
		}
	}
}
