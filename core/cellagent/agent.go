// Package cellagent is the logic of headmesh-cellagent, the process that runs beside each Headscale.
//
// The agent applies desired state from the Headmesh server to its local Headscale, but it never trusts that
// state blindly. Every change passes local guardrails that protect a production tailnet even from a buggy or
// compromised control server:
//
//   - operations must carry a valid Ed25519 signature from a pinned server key, must not be expired and are
//     executed at most once;
//   - observe mode and an operator "freeze" refuse every change;
//   - protected nodes can never be deleted, retagged, renamed, expired or given routes;
//   - deletes and retags are rate limited; exit-node routes are never approved;
//   - a policy is applied only if it parses, keeps every local reachability invariant, and passes Headscale's own
//     CheckPolicy (which also runs the policy's isolation tests);
//   - the last policy that applied cleanly is kept on disk for one-command rollback.
//
// Future policy revisions (scheduled grant expiries) are applied on time by Tick even while the server is
// unreachable.
package cellagent

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/headmesh-dev/headmesh/core/policy"
	cellv1 "github.com/headmesh-dev/headmesh/gen/cell/v1"
	headscalev1 "github.com/headmesh-dev/headmesh/gen/headscale/v1"
	"github.com/headmesh-dev/headmesh/internal/headscale"
)

// Invariant is reachability that must survive every policy change on this cell, for example "control nodes
// reach GPU hosts on tcp:22". Principals use Headscale policy syntax (tags or user@).
type Invariant struct {
	Name  string   `json:"name"`
	Src   []string `json:"src"`
	Dst   []string `json:"dst"`
	Proto string   `json:"proto"`
	Port  int      `json:"port"`
}

// Config controls an Agent. Cell-local safety settings live here, on the cell host, not on the server.
type Config struct {
	CellName string
	Mode     cellv1.Mode
	// StateDir holds last-known-good policy, the freeze flag and the record of executed operations.
	StateDir string
	// ServerKeys are the Ed25519 public keys allowed to sign operations. The key announced in Welcome must be
	// one of them, so a compromised transport cannot substitute its own.
	ServerKeys     []ed25519.PublicKey
	Invariants     []Invariant
	ProtectedNodes []uint64
	// MaxDeletesPerMinute and MaxRetagsPerHour bound blast radius. Zero means the defaults (5 and 50).
	MaxDeletesPerMinute int
	MaxRetagsPerHour    int
	// MaxPreAuthKeyTTL bounds pre-auth key lifetimes. Zero means 24h.
	MaxPreAuthKeyTTL time.Duration
	Now              func() time.Time
}

// Agent applies desired state to one Headscale under local guardrails. It is safe for concurrent use.
type Agent struct {
	cfg Config
	hs  headscale.API

	mu         sync.Mutex
	signingKey ed25519.PublicKey
	timeline   []*cellv1.PolicyRevision
	active     *cellv1.PolicyRevision
	seenOps    map[string]time.Time
	deletes    []time.Time
	retags     []time.Time
}

// ErrRefused marks a change a guardrail rejected. Results carry refused_by_guardrail so the server can alert.
var ErrRefused = errors.New("refused by cell guardrail")

func refusedf(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrRefused}, args...)...)
}

// New returns an Agent. It restores the record of executed operations from StateDir so a restart cannot
// replay an operation.
func New(cfg Config, hs headscale.API) (*Agent, error) {
	if cfg.CellName == "" || cfg.StateDir == "" {
		return nil, errors.New("cellagent: CellName and StateDir are required")
	}
	if cfg.Mode != cellv1.Mode_MODE_OBSERVE && cfg.Mode != cellv1.Mode_MODE_ENFORCE {
		return nil, errors.New("cellagent: mode must be observe or enforce")
	}
	if len(cfg.ServerKeys) == 0 {
		return nil, errors.New("cellagent: at least one pinned server key is required")
	}
	if cfg.MaxDeletesPerMinute == 0 {
		cfg.MaxDeletesPerMinute = 5
	}
	if cfg.MaxRetagsPerHour == 0 {
		cfg.MaxRetagsPerHour = 50
	}
	if cfg.MaxPreAuthKeyTTL == 0 {
		cfg.MaxPreAuthKeyTTL = 24 * time.Hour
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("cellagent: state dir: %w", err)
	}
	a := &Agent{cfg: cfg, hs: hs, seenOps: map[string]time.Time{}}
	if err := a.loadSeenOps(); err != nil {
		return nil, err
	}
	return a, nil
}

// AcceptServerKey records the op-signing key the server announced, refusing keys that are not pinned.
func (a *Agent) AcceptServerKey(key []byte) error {
	for _, k := range a.cfg.ServerKeys {
		if slices.Equal(k, key) {
			a.mu.Lock()
			a.signingKey = slices.Clone(k)
			a.mu.Unlock()
			return nil
		}
	}
	return refusedf("server announced an op-signing key that is not pinned on this cell")
}

// --- Policy timeline ---------------------------------------------------------------------------------------

// SetTimeline replaces the desired policy timeline. Revisions must be strictly increasing in both revision
// number and effective time.
func (a *Agent) SetTimeline(tl []*cellv1.PolicyRevision) error {
	for i := 1; i < len(tl); i++ {
		if tl[i].GetRevision() <= tl[i-1].GetRevision() ||
			!tl[i].GetEffectiveAt().AsTime().After(tl[i-1].GetEffectiveAt().AsTime()) {
			return errors.New("cellagent: timeline must be strictly ordered by revision and effective time")
		}
	}
	a.mu.Lock()
	a.timeline = tl
	a.mu.Unlock()
	return nil
}

// Tick applies the newest revision that is due and newer than the active one. Revisions that became due and
// were superseded while the agent was busy or offline are skipped: only the latest due state matters.
func (a *Agent) Tick(ctx context.Context) *cellv1.PolicyApplied {
	now := a.cfg.Now()
	a.mu.Lock()
	var due *cellv1.PolicyRevision
	for _, r := range a.timeline {
		if !r.GetEffectiveAt().AsTime().After(now) {
			due = r
		}
	}
	if due == nil || (a.active != nil && due.GetRevision() <= a.active.GetRevision()) {
		a.mu.Unlock()
		return nil
	}
	a.mu.Unlock()
	return a.apply(ctx, due)
}

func (a *Agent) apply(ctx context.Context, rev *cellv1.PolicyRevision) *cellv1.PolicyApplied {
	res := &cellv1.PolicyApplied{Revision: rev.GetRevision(), Sha256: rev.GetSha256(), AppliedAt: timestamppb.New(a.cfg.Now())}
	err := a.checkPolicy(ctx, rev)
	if err == nil {
		err = a.hs.SetPolicy(ctx, rev.GetHujson())
	}
	if err != nil {
		res.Error = err.Error()
		res.RefusedByGuardrail = errors.Is(err, ErrRefused)
		return res
	}
	if err := a.writeFile("last-known-good.hujson", []byte(rev.GetHujson())); err != nil {
		// The policy is live; failing to persist the rollback copy is reported but not fatal.
		res.Error = "applied, but saving last-known-good failed: " + err.Error()
	}
	a.mu.Lock()
	a.active = rev
	a.mu.Unlock()
	res.Ok = true
	return res
}

// checkPolicy runs every guardrail a policy must pass before it reaches Headscale.
func (a *Agent) checkPolicy(ctx context.Context, rev *cellv1.PolicyRevision) error {
	if err := a.mutable(); err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(rev.GetHujson()))
	if hex.EncodeToString(sum[:]) != rev.GetSha256() {
		return refusedf("revision %d: body does not match its sha256", rev.GetRevision())
	}
	doc, err := policy.ParseDocument([]byte(rev.GetHujson()))
	if err != nil {
		return refusedf("revision %d: %v", rev.GetRevision(), err)
	}
	for _, inv := range a.cfg.Invariants {
		if !doc.AllowsPrincipals(inv.Src, inv.Dst, inv.Proto, inv.Port) {
			return refusedf("revision %d breaks invariant %q (%v -> %v %s:%d)", rev.GetRevision(), inv.Name, inv.Src, inv.Dst, inv.Proto, inv.Port)
		}
	}
	if err := a.hs.CheckPolicy(ctx, rev.GetHujson()); err != nil {
		return refusedf("revision %d failed Headscale CheckPolicy (including its isolation tests): %v", rev.GetRevision(), err)
	}
	return nil
}

// Rollback re-applies the last policy that applied cleanly. It is the operator's undo button and works without
// the server.
func (a *Agent) Rollback(ctx context.Context) error {
	body, err := os.ReadFile(filepath.Join(a.cfg.StateDir, "last-known-good.hujson"))
	if err != nil {
		return fmt.Errorf("cellagent: no last-known-good policy: %w", err)
	}
	if err := a.hs.CheckPolicy(ctx, string(body)); err != nil {
		return fmt.Errorf("cellagent: last-known-good no longer passes CheckPolicy: %w", err)
	}
	if err := a.hs.SetPolicy(ctx, string(body)); err != nil {
		return fmt.Errorf("cellagent: rollback: %w", err)
	}
	return nil
}

// --- Freeze and mode ---------------------------------------------------------------------------------------

// Freeze stops all changes until Unfreeze. It is a file in StateDir so it survives restarts and can be set by
// an operator with `headmesh-cellagent freeze` even while the agent runs.
func (a *Agent) Freeze() error {
	return a.writeFile("frozen", []byte(a.cfg.Now().UTC().Format(time.RFC3339)))
}

// Unfreeze lifts a freeze.
func (a *Agent) Unfreeze() error {
	err := os.Remove(filepath.Join(a.cfg.StateDir, "frozen"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("cellagent: unfreeze: %w", err)
	}
	return nil
}

// Frozen reports whether the freeze flag is set.
func (a *Agent) Frozen() bool {
	_, err := os.Stat(filepath.Join(a.cfg.StateDir, "frozen"))
	return err == nil
}

func (a *Agent) mutable() error {
	if a.cfg.Mode != cellv1.Mode_MODE_ENFORCE {
		return refusedf("cell is in observe mode")
	}
	if a.Frozen() {
		return refusedf("cell is frozen by an operator")
	}
	return nil
}

// --- Operations --------------------------------------------------------------------------------------------

// SignOp signs op for the server side; the agent verifies with the matching public key.
func SignOp(key ed25519.PrivateKey, op *cellv1.Op) error {
	msg, err := opSigningBytes(op)
	if err != nil {
		return err
	}
	op.Signature = ed25519.Sign(key, msg)
	return nil
}

func opSigningBytes(op *cellv1.Op) ([]byte, error) {
	clone, ok := proto.Clone(op).(*cellv1.Op)
	if !ok {
		return nil, errors.New("cellagent: clone op")
	}
	clone.Signature = nil
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(clone)
	if err != nil {
		return nil, fmt.Errorf("cellagent: encode op: %w", err)
	}
	return b, nil
}

// HandleOp verifies and executes one operation and returns its result. It never panics on bad input.
func (a *Agent) HandleOp(ctx context.Context, op *cellv1.Op) *cellv1.OpResult {
	res := &cellv1.OpResult{OpId: op.GetId()}
	fail := func(err error) *cellv1.OpResult {
		res.Error = err.Error()
		res.RefusedByGuardrail = errors.Is(err, ErrRefused)
		return res
	}
	if err := a.verifyOp(op); err != nil {
		return fail(err)
	}
	if get := op.GetGetNode(); get != nil {
		// Reads are allowed in observe mode and while frozen, and are not recorded as executed.
		node, err := a.getNode(ctx, get.GetNodeId())
		if err != nil {
			return fail(err)
		}
		res.Ok, res.Result = true, &cellv1.OpResult_Node{Node: ToNode(node)}
		return res
	}
	if err := a.mutable(); err != nil {
		return fail(err)
	}
	if err := a.markSeen(op.GetId()); err != nil {
		return fail(err)
	}
	switch k := op.GetKind().(type) {
	case *cellv1.Op_CreatePreAuthKey:
		key, err := a.createPreAuthKey(ctx, k.CreatePreAuthKey)
		if err != nil {
			return fail(err)
		}
		res.Result = &cellv1.OpResult_PreAuthKey{PreAuthKey: &cellv1.PreAuthKeyResult{
			Id: key.GetId(), Key: key.GetKey(), Expiration: key.GetExpiration(),
		}}
	case *cellv1.Op_SetTags:
		node, err := a.setTags(ctx, k.SetTags)
		if err != nil {
			return fail(err)
		}
		res.Result = &cellv1.OpResult_Node{Node: ToNode(node)}
	case *cellv1.Op_RenameNode:
		if err := a.unprotected(k.RenameNode.GetNodeId(), "rename"); err != nil {
			return fail(err)
		}
		node, err := a.hs.RenameNode(ctx, k.RenameNode.GetNodeId(), k.RenameNode.GetNewName())
		if err != nil {
			return fail(err)
		}
		res.Result = &cellv1.OpResult_Node{Node: ToNode(node)}
	case *cellv1.Op_DeleteNode:
		if err := a.deleteNode(ctx, k.DeleteNode.GetNodeId()); err != nil {
			return fail(err)
		}
	case *cellv1.Op_ExpireNode:
		if !k.ExpireNode.GetDisableExpiry() {
			if err := a.unprotected(k.ExpireNode.GetNodeId(), "expire"); err != nil {
				return fail(err)
			}
		}
		node, err := a.hs.ExpireNode(ctx, k.ExpireNode.GetNodeId(), k.ExpireNode.GetDisableExpiry())
		if err != nil {
			return fail(err)
		}
		res.Result = &cellv1.OpResult_Node{Node: ToNode(node)}
	case *cellv1.Op_SetApprovedRoutes:
		node, err := a.setRoutes(ctx, k.SetApprovedRoutes)
		if err != nil {
			return fail(err)
		}
		res.Result = &cellv1.OpResult_Node{Node: ToNode(node)}
	default:
		return fail(refusedf("unknown operation kind"))
	}
	res.Ok = true
	return res
}

func (a *Agent) getNode(ctx context.Context, id uint64) (*headscalev1.Node, error) {
	nodes, err := a.hs.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	for _, n := range nodes {
		if n.GetId() == id {
			return n, nil
		}
	}
	return nil, fmt.Errorf("node %d not found", id)
}

func (a *Agent) verifyOp(op *cellv1.Op) error {
	if op.GetId() == "" {
		return refusedf("operation without id")
	}
	a.mu.Lock()
	key := a.signingKey
	a.mu.Unlock()
	if key == nil {
		return refusedf("no accepted server key yet")
	}
	msg, err := opSigningBytes(op)
	if err != nil {
		return err
	}
	if !ed25519.Verify(key, msg, op.GetSignature()) {
		return refusedf("operation %s has an invalid signature", op.GetId())
	}
	if op.GetNotAfter() == nil || a.cfg.Now().After(op.GetNotAfter().AsTime()) {
		return refusedf("operation %s has expired", op.GetId())
	}
	return nil
}

func (a *Agent) unprotected(node uint64, action string) error {
	if slices.Contains(a.cfg.ProtectedNodes, node) {
		return refusedf("node %d is protected on this cell and cannot be %sd", node, strings.TrimSuffix(action, "e"))
	}
	return nil
}

func headmeshTag(t string) bool { return strings.HasPrefix(t, "tag:hm-") }

func (a *Agent) createPreAuthKey(ctx context.Context, req *cellv1.CreatePreAuthKey) (*headscalev1.PreAuthKey, error) {
	if len(req.GetAclTags()) == 0 {
		return nil, refusedf("pre-auth keys must be tagged")
	}
	for _, t := range req.GetAclTags() {
		if !headmeshTag(t) || t == policy.QuarantineTag {
			return nil, refusedf("pre-auth key tag %q is not a Headmesh tag", t)
		}
	}
	exp := req.GetExpiration().AsTime()
	if !exp.After(a.cfg.Now()) || exp.Sub(a.cfg.Now()) > a.cfg.MaxPreAuthKeyTTL {
		return nil, refusedf("pre-auth key expiry must be within %s", a.cfg.MaxPreAuthKeyTTL)
	}
	return a.hs.CreatePreAuthKey(ctx, headscale.PreAuthKeyRequest{
		Tags: req.GetAclTags(), Reusable: req.GetReusable(), Ephemeral: req.GetEphemeral(), Expiration: exp,
	})
}

func (a *Agent) setTags(ctx context.Context, req *cellv1.SetTags) (*headscalev1.Node, error) {
	if err := a.unprotected(req.GetNodeId(), "retag"); err != nil {
		return nil, err
	}
	if len(req.GetTags()) == 0 {
		return nil, refusedf("a node cannot be left without tags")
	}
	for _, t := range req.GetTags() {
		if !headmeshTag(t) {
			return nil, refusedf("tag %q is not a Headmesh tag", t)
		}
	}
	if err := a.rateLimit(&a.retags, time.Hour, a.cfg.MaxRetagsPerHour, "retags"); err != nil {
		return nil, err
	}
	return a.hs.SetTags(ctx, req.GetNodeId(), req.GetTags())
}

func (a *Agent) deleteNode(ctx context.Context, node uint64) error {
	if err := a.unprotected(node, "delete"); err != nil {
		return err
	}
	if err := a.rateLimit(&a.deletes, time.Minute, a.cfg.MaxDeletesPerMinute, "deletes"); err != nil {
		return err
	}
	return a.hs.DeleteNode(ctx, node)
}

func (a *Agent) setRoutes(ctx context.Context, req *cellv1.SetApprovedRoutes) (*headscalev1.Node, error) {
	if err := a.unprotected(req.GetNodeId(), "reroute"); err != nil {
		return nil, err
	}
	for _, r := range req.GetRoutes() {
		if r == "0.0.0.0/0" || r == "::/0" {
			return nil, refusedf("exit-node routes are never approved by Headmesh")
		}
	}
	return a.hs.SetApprovedRoutes(ctx, req.GetNodeId(), req.GetRoutes())
}

func (a *Agent) rateLimit(window *[]time.Time, per time.Duration, maxN int, what string) error {
	now := a.cfg.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	kept := (*window)[:0]
	for _, t := range *window {
		if now.Sub(t) < per {
			kept = append(kept, t)
		}
	}
	*window = kept
	if len(kept) >= maxN {
		return refusedf("too many %s (limit %d per %s); freeze-and-review threshold reached", what, maxN, per)
	}
	*window = append(*window, now)
	return nil
}

// markSeen records op id durably before execution, so a crash mid-operation can never lead to a replay.
func (a *Agent) markSeen(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, dup := a.seenOps[id]; dup {
		return refusedf("operation %s was already executed", id)
	}
	now := a.cfg.Now()
	for k, t := range a.seenOps {
		if now.Sub(t) > 7*24*time.Hour {
			delete(a.seenOps, k)
		}
	}
	a.seenOps[id] = now
	b, err := json.Marshal(a.seenOps)
	if err != nil {
		return fmt.Errorf("cellagent: encode seen ops: %w", err)
	}
	return a.writeFileLocked("seen-ops.json", b)
}

func (a *Agent) loadSeenOps() error {
	b, err := os.ReadFile(filepath.Join(a.cfg.StateDir, "seen-ops.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cellagent: read seen ops: %w", err)
	}
	if err := json.Unmarshal(b, &a.seenOps); err != nil {
		return fmt.Errorf("cellagent: decode seen ops: %w", err)
	}
	return nil
}

// --- Observation -------------------------------------------------------------------------------------------

// Observe returns a full snapshot of Headscale's nodes and the active policy digest.
func (a *Agent) Observe(ctx context.Context) *cellv1.Observed {
	obs := &cellv1.Observed{FullSnapshot: true, HeadscaleHealthy: true}
	if err := a.hs.Health(ctx); err != nil {
		obs.HeadscaleHealthy = false
		obs.Detail = err.Error()
		return obs
	}
	nodes, err := a.hs.ListNodes(ctx)
	if err != nil {
		obs.HeadscaleHealthy = false
		obs.Detail = err.Error()
		return obs
	}
	for _, n := range nodes {
		obs.Nodes = append(obs.Nodes, ToNode(n))
	}
	if body, err := a.hs.GetPolicy(ctx); err == nil {
		sum := sha256.Sum256([]byte(body))
		obs.ActivePolicySha256 = hex.EncodeToString(sum[:])
	}
	return obs
}

// ToNode converts a Headscale node to the cell protocol's node.
func ToNode(n *headscalev1.Node) *cellv1.Node {
	if n == nil {
		return nil
	}
	out := &cellv1.Node{
		Id: n.GetId(), Name: n.GetName(), GivenName: n.GetGivenName(), IpAddresses: n.GetIpAddresses(),
		Tags: n.GetTags(), Online: n.GetOnline(), LastSeen: n.GetLastSeen(), Expiry: n.GetExpiry(),
		ApprovedRoutes: n.GetApprovedRoutes(), AvailableRoutes: n.GetAvailableRoutes(),
	}
	if u := n.GetUser(); u != nil {
		out.User = u.GetName()
	}
	if k := n.GetPreAuthKey(); k != nil {
		out.PreAuthKeyId = k.GetId()
	}
	return out
}

// Status is the local, operator-facing summary printed by `headmesh-cellagent status`.
type Status struct {
	Cell           string    `json:"cell"`
	Mode           string    `json:"mode"`
	Frozen         bool      `json:"frozen"`
	ActiveRevision uint64    `json:"active_revision"`
	ActiveSHA256   string    `json:"active_sha256"`
	PendingCount   int       `json:"pending_revisions"`
	NextChangeAt   time.Time `json:"next_change_at,omitzero"`
}

// Status reports the agent's state.
func (a *Agent) Status() Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := Status{Cell: a.cfg.CellName, Mode: a.cfg.Mode.String(), Frozen: a.Frozen()}
	if a.active != nil {
		s.ActiveRevision, s.ActiveSHA256 = a.active.GetRevision(), a.active.GetSha256()
	}
	now := a.cfg.Now()
	for _, r := range a.timeline {
		if r.GetEffectiveAt().AsTime().After(now) {
			if s.PendingCount == 0 {
				s.NextChangeAt = r.GetEffectiveAt().AsTime()
			}
			s.PendingCount++
		}
	}
	return s
}

// --- Files -------------------------------------------------------------------------------------------------

func (a *Agent) writeFile(name string, b []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.writeFileLocked(name, b)
}

// writeFileLocked writes atomically (temp file, fsync, rename) with owner-only permissions.
func (a *Agent) writeFileLocked(name string, b []byte) error {
	f, err := os.CreateTemp(a.cfg.StateDir, "."+name+".tmp-*")
	if err != nil {
		return fmt.Errorf("cellagent: write %s: %w", name, err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return fmt.Errorf("cellagent: write %s: %w", name, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("cellagent: sync %s: %w", name, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("cellagent: close %s: %w", name, err)
	}
	if err := os.Rename(tmp, filepath.Join(a.cfg.StateDir, name)); err != nil {
		return fmt.Errorf("cellagent: rename %s: %w", name, err)
	}
	return nil
}
