// Package store is Headmesh's Postgres access layer: tenant-scoped transactions, migrations, the tamper-evident
// audit log and the loader that turns database state into a policy.Snapshot.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/headmesh-dev/headmesh/db"
	"github.com/headmesh-dev/headmesh/internal/store/dbq"
)

// Store wraps a connection pool. The pool's role must not be a superuser and must not have BYPASSRLS, otherwise
// row-level security (the database half of tenant isolation) silently stops applying.
type Store struct {
	pool *pgxpool.Pool
}

// New returns a Store over pool.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Pool exposes the underlying pool (for River and health checks).
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Scope says which tenants a transaction may see. Exactly one of OrgIDs or System must be set.
type Scope struct {
	OrgIDs []pgtype.UUID
	// System grants a whole-cell view. Only background workers (policy compiler, reconciler, recovery sweep)
	// use it; request handlers never do.
	System bool
}

// ErrNoScope is returned when a transaction is started without a tenant scope.
var ErrNoScope = errors.New("store: transaction needs a tenant scope or the system scope")

// Tx runs fn in a transaction whose row-level-security context is set to scope. The context is transaction-local
// (SET LOCAL semantics), so it can never leak to another request through the pool.
func (s *Store) Tx(ctx context.Context, scope Scope, fn func(q *dbq.Queries, tx pgx.Tx) error) error {
	if !scope.System && len(scope.OrgIDs) == 0 {
		return ErrNoScope
	}
	ids := make([]string, 0, len(scope.OrgIDs))
	for _, id := range scope.OrgIDs {
		if !id.Valid {
			return fmt.Errorf("store: invalid org id in scope")
		}
		ids = append(ids, id.String())
	}
	system := ""
	if scope.System {
		system = "on"
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('headmesh.org_ids', $1, true), set_config('headmesh.system', $2, true)",
			strings.Join(ids, ","), system); err != nil {
			return fmt.Errorf("set tenant scope: %w", err)
		}
		return fn(dbq.New(tx), tx)
	})
	if err != nil {
		return fmt.Errorf("store tx: %w", err)
	}
	return nil
}

// Migrate applies all pending migrations. It must run as the schema owner, not as the application role.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	goose.SetBaseFS(db.Migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("goose dialect: %w", err)
	}
	conn := stdlib.OpenDBFromPool(pool)
	defer func(c *sql.DB) { _ = c.Close() }(conn)
	if err := goose.UpContext(ctx, conn, "migrations"); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// UUID parses a canonical UUID string into the pgx type.
func UUID(s string) (pgtype.UUID, error) {
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		return pgtype.UUID{}, fmt.Errorf("parse uuid %q: %w", s, err)
	}
	return u, nil
}
