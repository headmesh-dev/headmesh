package store

import (
	"context"
	"fmt"
	"net/netip"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/headmesh-dev/headmesh/core/cellserver"
	"github.com/headmesh-dev/headmesh/core/policy"
	cellv1 "github.com/headmesh-dev/headmesh/gen/cell/v1"
	"github.com/headmesh-dev/headmesh/internal/store/dbq"
)

// CellStore implements cellserver.Store on Postgres. Every call runs in the system scope: the hub works on
// whole cells, never on behalf of a tenant.
type CellStore struct {
	s     *Store
	mu    sync.Mutex
	cells map[string]dbq.Cell
}

var _ cellserver.Store = (*CellStore)(nil)

// NewCellStore returns a CellStore.
func NewCellStore(s *Store) *CellStore { return &CellStore{s: s, cells: map[string]dbq.Cell{}} }

var systemScope = Scope{System: true}

func (c *CellStore) cell(ctx context.Context, q *dbq.Queries, name string) (dbq.Cell, error) {
	c.mu.Lock()
	cell, ok := c.cells[name]
	c.mu.Unlock()
	if ok {
		return cell, nil
	}
	cell, err := q.GetCellByName(ctx, name)
	if err != nil {
		return dbq.Cell{}, fmt.Errorf("unknown cell %q: %w", name, err)
	}
	c.mu.Lock()
	c.cells[name] = cell
	c.mu.Unlock()
	return cell, nil
}

// Snapshot implements cellserver.Store.
func (c *CellStore) Snapshot(ctx context.Context, name string, now time.Time, horizon time.Duration) (policy.Snapshot, error) {
	var snap policy.Snapshot
	err := c.s.Tx(ctx, systemScope, func(q *dbq.Queries, _ pgx.Tx) error {
		cell, err := c.cell(ctx, q, name)
		if err != nil {
			return err
		}
		snap, err = LoadSnapshot(ctx, q, cell, now, horizon)
		return err
	})
	return snap, err
}

// RecordHello implements cellserver.Store.
func (c *CellStore) RecordHello(ctx context.Context, name string, hello *cellv1.Hello) error {
	return c.s.Tx(ctx, systemScope, func(q *dbq.Queries, _ pgx.Tx) error {
		cell, err := c.cell(ctx, q, name)
		if err != nil {
			return err
		}
		v := hello.GetHeadscaleVersion()
		if err := q.TouchCell(ctx, dbq.TouchCellParams{ID: cell.ID, HeadscaleVersion: &v}); err != nil {
			return fmt.Errorf("touch cell: %w", err)
		}
		_, err = AppendAudit(ctx, q, AuditEvent{
			ActorType: "cell", ActorID: name, Action: "cell.connected",
			TargetType: "cell", TargetID: name, Data: map[string]any{
				"agent_version": hello.GetAgentVersion(), "headscale_version": v, "mode": hello.GetMode().String(),
				"noise_public_key_sha256": hello.GetNoisePublicKeySha256(), "active_revision": hello.GetActivePolicyRevision(),
			},
		})
		return err
	})
}

// RecordObserved implements cellserver.Store: it syncs addresses and liveness onto known devices.
func (c *CellStore) RecordObserved(ctx context.Context, name string, obs *cellv1.Observed) error {
	return c.s.Tx(ctx, systemScope, func(q *dbq.Queries, _ pgx.Tx) error {
		cell, err := c.cell(ctx, q, name)
		if err != nil {
			return err
		}
		for _, n := range obs.GetNodes() {
			id := int64(n.GetId()) //nolint:gosec // Headscale node ids are small positive integers
			p := dbq.SyncDeviceFromNodeParams{CellID: cell.ID, HeadscaleNodeID: &id, Online: n.GetOnline()}
			for _, raw := range n.GetIpAddresses() {
				addr, err := netip.ParseAddr(raw)
				if err != nil {
					continue
				}
				if addr.Is4() {
					p.Ipv4 = &addr
				} else {
					p.Ipv6 = &addr
				}
			}
			if ls := n.GetLastSeen(); ls != nil {
				t := ls.AsTime()
				p.LastSeenAt = &t
			}
			if err := q.SyncDeviceFromNode(ctx, p); err != nil {
				return fmt.Errorf("sync node %d: %w", n.GetId(), err)
			}
		}
		return nil
	})
}

// RecordTimeline implements cellserver.Store.
func (c *CellStore) RecordTimeline(ctx context.Context, name string, tl []*cellv1.PolicyRevision) error {
	return c.s.Tx(ctx, systemScope, func(q *dbq.Queries, _ pgx.Tx) error {
		cell, err := c.cell(ctx, q, name)
		if err != nil {
			return err
		}
		for _, r := range tl {
			if err := q.InsertPolicyRevision(ctx, dbq.InsertPolicyRevisionParams{
				CellID: cell.ID, Revision: int64(r.GetRevision()), //nolint:gosec // revisions are allocated from int64
				Sha256: r.GetSha256(), Body: r.GetHujson(), CompilerVersion: policy.CompilerVersion,
				EffectiveAt: r.GetEffectiveAt().AsTime(),
			}); err != nil {
				return fmt.Errorf("record revision %d: %w", r.GetRevision(), err)
			}
		}
		return nil
	})
}

// RecordApplied implements cellserver.Store. A guardrail refusal is also written to the audit log, because it
// means the control server asked a cell for something the cell considered unsafe.
func (c *CellStore) RecordApplied(ctx context.Context, name string, a *cellv1.PolicyApplied) error {
	return c.s.Tx(ctx, systemScope, func(q *dbq.Queries, _ pgx.Tx) error {
		cell, err := c.cell(ctx, q, name)
		if err != nil {
			return err
		}
		status, at := "applied", a.GetAppliedAt().AsTime()
		var errText *string
		if !a.GetOk() {
			status, errText = "failed", &a.Error
		}
		if err := q.MarkPolicyRevision(ctx, dbq.MarkPolicyRevisionParams{
			CellID: cell.ID, Revision: int64(a.GetRevision()), //nolint:gosec // revisions are allocated from int64
			Status: status, AppliedAt: &at, Error: errText,
		}); err != nil {
			return fmt.Errorf("mark revision: %w", err)
		}
		if a.GetRefusedByGuardrail() {
			_, err = AppendAudit(ctx, q, AuditEvent{
				ActorType: "cell", ActorID: name, Action: "policy.refused_by_cell",
				TargetType: "policy_revision", TargetID: fmt.Sprint(a.GetRevision()), Data: map[string]any{"error": a.GetError()},
			})
		}
		return err
	})
}

// NextRevision implements cellserver.Store.
func (c *CellStore) NextRevision(ctx context.Context, name string) (uint64, error) {
	var next int64
	err := c.s.Tx(ctx, systemScope, func(q *dbq.Queries, _ pgx.Tx) error {
		cell, err := c.cell(ctx, q, name)
		if err != nil {
			return err
		}
		next, err = q.NextPolicyRevision(ctx, cell.ID)
		return err
	})
	return uint64(next), err //nolint:gosec // never negative: coalesce(max, 0) + 1
}
