package service

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/headmesh-dev/headmesh/internal/store"
	"github.com/headmesh-dev/headmesh/internal/store/dbq"
)

// SweepExpiredGrants writes one grant.expired audit entry for every grant whose expiry has passed and has not
// been audited, recording how late the sweep noticed it. Access itself already ended on time — cells apply
// precomputed policy revisions at each expiry — so this is bookkeeping, not enforcement. It returns how many
// grants it audited.
func (s *Service) SweepExpiredGrants(ctx context.Context) (int, error) {
	total := 0
	for {
		n := 0
		now := s.now()
		err := s.Store.Tx(ctx, systemScope, func(q *dbq.Queries, _ pgx.Tx) error {
			grants, err := q.ClaimExpiredGrants(ctx, dbq.ClaimExpiredGrantsParams{ExpiresAt: now, Limit: 500})
			if err != nil {
				return fmt.Errorf("claim expired grants: %w", err)
			}
			for _, g := range grants {
				_, err := store.AppendAudit(ctx, q, store.AuditEvent{
					OrgID: g.GrantorOrgID, OccurredAt: now, ActorType: "system", ActorID: "expiry",
					Action: "grant.expired", TargetType: "grant", TargetID: g.ID.String(),
					Data: map[string]any{
						"expires_at": g.ExpiresAt, "noticed_late_seconds": int64(now.Sub(g.ExpiresAt).Seconds()),
						"external_ref": derefOr(g.ExternalRef, ""),
					},
				})
				if err != nil {
					return err
				}
				if err := q.MarkGrantExpiryAudited(ctx, dbq.MarkGrantExpiryAuditedParams{ID: g.ID, ExpiryAuditedAt: &now}); err != nil {
					return fmt.Errorf("mark grant audited: %w", err)
				}
			}
			n = len(grants)
			return nil
		})
		if err != nil {
			return total, err
		}
		total += n
		if n < 500 {
			return total, nil
		}
	}
}

// RecoveryHub is the part of the hub the startup gate uses.
type RecoveryHub interface {
	Connected(cell string) bool
	RefreshAndWait(ctx context.Context, cell string) error
}

// AwaitCellsCurrent blocks until every enforce-mode cell is connected and has applied a policy compiled now, so
// grants that expired while the control plane was down are closed before the API accepts new work (plan §13.4).
// Observe-mode cells never block. It returns an error if ctx ends first.
func (s *Service) AwaitCellsCurrent(ctx context.Context, hub RecoveryHub, log *slog.Logger) error {
	var cells []dbq.Cell
	err := s.Store.Tx(ctx, systemScope, func(q *dbq.Queries, _ pgx.Tx) error {
		var err error
		cells, err = q.ListCells(ctx)
		if err != nil {
			return fmt.Errorf("list cells: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, c := range cells {
		if c.Mode != "enforce" {
			continue
		}
		for {
			if hub.Connected(c.Name) {
				err := hub.RefreshAndWait(ctx, c.Name)
				if err == nil {
					log.Info("recovery: cell is current", "cell", c.Name)
					break
				}
				log.Warn("recovery: cell not current yet", "cell", c.Name, "error", err)
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("recovery: cell %s did not confirm a current policy: %w", c.Name, ctx.Err())
			case <-time.After(time.Second):
			}
		}
	}
	return nil
}

func derefOr(s *string, def string) string {
	if s == nil {
		return def
	}
	return *s
}
