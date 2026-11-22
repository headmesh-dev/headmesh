package recovery

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/headmesh-dev/headmesh/internal/store"
	"github.com/headmesh-dev/headmesh/internal/store/dbq"
)

// VerifyAudit checks every organization and the tenant-less chain on the restored database.
func VerifyAudit(ctx context.Context, db *pgx.Conn) (orgs, events int, err error) {
	err = pgx.BeginTxFunc(ctx, db, pgx.TxOptions{IsoLevel: pgx.RepeatableRead}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('headmesh.system', 'on', true)"); err != nil {
			return fmt.Errorf("set recovery audit scope: %w", err)
		}
		rows, err := tx.Query(ctx, `SELECT id FROM orgs
			UNION SELECT org_id FROM audit_events WHERE org_id IS NOT NULL
			UNION SELECT chain_key FROM audit_chain_heads WHERE chain_key <> '00000000-0000-0000-0000-000000000000'
			ORDER BY 1`)
		if err != nil {
			return fmt.Errorf("list restored organizations: %w", err)
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[pgtype.UUID])
		if err != nil {
			return fmt.Errorf("read restored organizations: %w", err)
		}
		orgs = len(ids)
		q := dbq.New(tx)
		for _, id := range append(ids, pgtype.UUID{}) {
			n, err := store.VerifyAuditChain(ctx, q, id)
			if err != nil {
				if !id.Valid {
					return fmt.Errorf("verify tenant-less audit chain: %w", err)
				}
				return fmt.Errorf("verify audit chain for organization %s: %w", id, err)
			}
			events += n
		}
		return nil
	})
	if err != nil {
		return 0, 0, fmt.Errorf("verify restored audit chains: %w", err)
	}
	return orgs, events, nil
}
