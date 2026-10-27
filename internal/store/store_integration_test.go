//go:build integration

package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/headmesh-dev/headmesh/core/policy"
	"github.com/headmesh-dev/headmesh/internal/store"
	"github.com/headmesh-dev/headmesh/internal/store/dbq"
)

const postgresImage = "postgres:18.4-alpine"

// env holds a migrated database plus two pools: owner (superuser, used only for setup and tampering) and app
// (a plain role without BYPASSRLS, which is how production connects).
type env struct {
	owner *pgxpool.Pool
	app   *store.Store
}

func setup(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	ctr, err := tcpostgres.Run(ctx, postgresImage,
		tcpostgres.WithDatabase("headmesh"), tcpostgres.WithUsername("owner"), tcpostgres.WithPassword("owner"),
		tcpostgres.BasicWaitStrategies())
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = testcontainers.TerminateContainer(ctr) })
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	owner, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	if err := store.Migrate(ctx, owner); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, stmt := range []string{
		`CREATE ROLE headmesh_app LOGIN PASSWORD 'app' NOSUPERUSER NOBYPASSRLS`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO headmesh_app`,
		`REVOKE UPDATE, DELETE ON audit_events FROM headmesh_app`,
	} {
		if _, err := owner.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.User, cfg.ConnConfig.Password = "headmesh_app", "app"
	app, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	return &env{owner: owner, app: store.New(app)}
}

var system = store.Scope{System: true}

type seeded struct {
	cell                     dbq.Cell
	platform, owner1, owner2 dbq.Org
	gateway, gpu1, gpu2      dbq.Device
}

func seed(t *testing.T, e *env) seeded {
	t.Helper()
	ctx := context.Background()
	var s seeded
	err := e.app.Tx(ctx, system, func(q *dbq.Queries, _ pgx.Tx) error {
		var err error
		if s.cell, err = q.CreateCell(ctx, dbq.CreateCellParams{Name: "xcelsior", ServerUrl: "https://hs.example.test", BaseDomain: "mesh.example.test", Mode: "enforce"}); err != nil {
			return err
		}
		org := func(short, slug string, parent pgtype.UUID, platform bool) (dbq.Org, error) {
			return q.CreateOrg(ctx, dbq.CreateOrgParams{
				ShortID: short, Slug: slug, Name: slug, CellID: s.cell.ID,
				ParentOrgID: parent, IsPlatform: platform, Template: "locked", HeadscaleUsers: []string{},
			})
		}
		if s.platform, err = org("xaaaaaaaab", "xcelsior", pgtype.UUID{}, true); err != nil {
			return err
		}
		if s.owner1, err = org("paaaaaaaab", "owner-1", s.platform.ID, false); err != nil {
			return err
		}
		if s.owner2, err = org("paaaaaaaac", "owner-2", s.platform.ID, false); err != nil {
			return err
		}
		dev := func(o dbq.Org, short, name, kind string, roles []string) (dbq.Device, error) {
			return q.CreateDevice(ctx, dbq.CreateDeviceParams{
				OrgID: o.ID, CellID: s.cell.ID, ShortID: short, Name: name,
				GivenName: name + "-" + o.Slug, Kind: kind, Roles: roles,
			})
		}
		if s.gateway, err = dev(s.platform, "daaaaaaaab", "gateway", "server", []string{"gateway"}); err != nil {
			return err
		}
		if s.gpu1, err = dev(s.owner1, "daaaaaaaac", "gpu1", "gpu", []string{"gpu-host"}); err != nil {
			return err
		}
		s.gpu2, err = dev(s.owner2, "daaaaaaaad", "gpu2", "gpu", []string{"gpu-host"})
		return err
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return s
}

func TestMigrationsRoundTrip(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	var n int
	if err := e.owner.QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tablename = 'grants'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("grants table missing after migration (n=%d, err=%v)", n, err)
	}
	// Every tenant-owned table must have RLS enabled and forced.
	rows, err := e.owner.Query(ctx, `SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind = 'r' AND EXISTS (
		  SELECT 1 FROM information_schema.columns col WHERE col.table_name = c.relname AND col.column_name IN ('org_id', 'grantor_org_id'))
		AND NOT (c.relrowsecurity AND c.relforcerowsecurity)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		_ = rows.Scan(&name)
		t.Errorf("tenant table %s does not force row-level security", name)
	}
}

func TestRowLevelSecurityIsolatesTenants(t *testing.T) {
	e := setup(t)
	s := seed(t, e)
	ctx := context.Background()

	// Owner 1 sees only its own device.
	err := e.app.Tx(ctx, store.Scope{OrgIDs: []pgtype.UUID{s.owner1.ID}}, func(q *dbq.Queries, _ pgx.Tx) error {
		devs, err := q.ListDevicesInCell(ctx, s.cell.ID)
		if err != nil {
			return err
		}
		if len(devs) != 1 || devs[0].ID != s.gpu1.ID {
			return fmt.Errorf("owner 1 sees %d devices, want only gpu1", len(devs))
		}
		if _, err := q.GetDevice(ctx, s.gpu2.ID); !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("owner 1 can read owner 2's device: %w", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Writing into another tenant is rejected by the WITH CHECK clause.
	err = e.app.Tx(ctx, store.Scope{OrgIDs: []pgtype.UUID{s.owner1.ID}}, func(q *dbq.Queries, _ pgx.Tx) error {
		_, err := q.CreateDevice(ctx, dbq.CreateDeviceParams{
			OrgID: s.owner2.ID, CellID: s.cell.ID, ShortID: "daaaaaaaae",
			Name: "intruder", GivenName: "intruder-owner-2", Kind: "server", Roles: []string{},
		})
		return err
	})
	if err == nil {
		t.Fatal("inserting a device into another org must fail")
	}

	// No scope at all is refused before touching the database.
	if err := e.app.Tx(ctx, store.Scope{}, func(*dbq.Queries, pgx.Tx) error { return nil }); !errors.Is(err, store.ErrNoScope) {
		t.Fatalf("missing scope: got %v", err)
	}

	// The scope does not leak to the next transaction on the same pooled connection.
	err = e.app.Tx(ctx, store.Scope{OrgIDs: []pgtype.UUID{s.owner2.ID}}, func(q *dbq.Queries, tx pgx.Tx) error {
		var v string
		if err := tx.QueryRow(ctx, `SELECT coalesce(current_setting('headmesh.system', true), '')`).Scan(&v); err != nil {
			return err
		}
		if v == "on" {
			return errors.New("system scope leaked into a tenant transaction")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAuditChainIsAppendOnlyAndTamperEvident(t *testing.T) {
	e := setup(t)
	s := seed(t, e)
	ctx := context.Background()
	scope := store.Scope{OrgIDs: []pgtype.UUID{s.owner1.ID}}

	// Concurrent appends must serialise into one unbroken chain.
	var wg sync.WaitGroup
	var failures atomic.Int32
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := e.app.Tx(ctx, scope, func(q *dbq.Queries, _ pgx.Tx) error {
				_, err := store.AppendAudit(ctx, q, store.AuditEvent{
					OrgID: s.owner1.ID, ActorType: "system", ActorID: "test",
					Action: "grant.created", TargetType: "grant", TargetID: fmt.Sprint(i), Data: map[string]any{"n": i, "z": "last", "a": "first"},
				})
				return err
			})
			if err != nil {
				failures.Add(1)
				t.Errorf("append %d: %v", i, err)
			}
		}()
	}
	wg.Wait()
	if failures.Load() > 0 {
		t.FailNow()
	}
	err := e.app.Tx(ctx, scope, func(q *dbq.Queries, _ pgx.Tx) error {
		n, err := store.VerifyAuditChain(ctx, q, s.owner1.ID)
		if err != nil {
			return err
		}
		if n != 20 {
			return fmt.Errorf("verified %d events, want 20", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}

	// The application role cannot rewrite history.
	err = e.app.Tx(ctx, scope, func(_ *dbq.Queries, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE audit_events SET action = 'grant.revoked'`)
		return err
	})
	if err == nil {
		t.Fatal("the app role must not be able to update audit events")
	}

	// Even a superuser who bypasses the trigger is caught by the chain.
	for _, stmt := range []string{
		`ALTER TABLE audit_events DISABLE TRIGGER audit_events_no_update`,
		`UPDATE audit_events SET data = '{"n": 999}' WHERE id = (SELECT min(id) + 3 FROM audit_events)`,
		`ALTER TABLE audit_events ENABLE TRIGGER audit_events_no_update`,
	} {
		if _, err := e.owner.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	err = e.app.Tx(ctx, scope, func(q *dbq.Queries, _ pgx.Tx) error {
		_, err := store.VerifyAuditChain(ctx, q, s.owner1.ID)
		return err
	})
	if !errors.Is(err, store.ErrChainBroken) {
		t.Fatalf("tampering must break the chain, got %v", err)
	}
}

func TestEnrollmentTokenCannotBeOverspent(t *testing.T) {
	e := setup(t)
	s := seed(t, e)
	ctx := context.Background()
	hash := sha256.Sum256([]byte("hmj_test-token"))
	scope := store.Scope{OrgIDs: []pgtype.UUID{s.owner1.ID}}
	err := e.app.Tx(ctx, scope, func(q *dbq.Queries, _ pgx.Tx) error {
		_, err := q.CreateEnrollmentToken(ctx, dbq.CreateEnrollmentTokenParams{
			OrgID: s.owner1.ID, TokenHash: hash[:],
			Prefix: "hmj_test", Kind: "device", Roles: []string{"gpu-host"}, DeviceKind: "gpu", MaxUses: 1,
			ExpiresAt: time.Now().Add(time.Hour),
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = e.app.Tx(ctx, scope, func(q *dbq.Queries, _ pgx.Tx) error {
				if _, err := q.ConsumeEnrollmentToken(ctx, hash[:]); err == nil {
					wins.Add(1)
				}
				return nil
			})
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("a single-use token was consumed %d times", wins.Load())
	}
}

func TestSnapshotCompilesAndGrantsAreIdempotent(t *testing.T) {
	e := setup(t)
	s := seed(t, e)
	ctx := context.Background()
	now := time.Now()
	src, _ := json.Marshal(store.SelectorJSON{Kind: "role", Role: "gateway"})
	ref := "attempt-42"
	create := func() (dbq.Grant, error) {
		var g dbq.Grant
		err := e.app.Tx(ctx, store.Scope{OrgIDs: []pgtype.UUID{s.owner1.ID}}, func(q *dbq.Queries, _ pgx.Tx) error {
			var err error
			g, err = q.CreateGrant(ctx, dbq.CreateGrantParams{
				GrantorOrgID: s.owner1.ID, GranteeOrgID: s.platform.ID, Src: src,
				DstDeviceID: s.gpu1.ID, Ports: []string{"tcp:10443"}, StartsAt: now.Add(-time.Minute),
				ExpiresAt: now.Add(time.Hour), ExternalRef: &ref,
			})
			return err
		})
		return g, err
	}
	if _, err := create(); err != nil {
		t.Fatalf("create grant: %v", err)
	}
	if _, err := create(); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("a retried grant with the same external_ref must be a no-op, got %v", err)
	}
	platformSSH := func(scope string, role string) []byte {
		b, _ := json.Marshal(store.PlatformSelectorJSON{Scope: scope, Role: role})
		return b
	}
	err := e.app.Tx(ctx, system, func(q *dbq.Queries, _ pgx.Tx) error {
		if _, err := q.CreatePlatformRule(ctx, dbq.CreatePlatformRuleParams{
			OrgID: s.platform.ID,
			Src:   platformSSH("platform", "gateway"), Dst: platformSSH("children", "gpu-host"), Ports: []string{"tcp:22"},
		}); err != nil {
			return err
		}
		snap, err := store.LoadSnapshot(ctx, q, s.cell, now, 24*time.Hour)
		if err != nil {
			return err
		}
		c, err := policy.Compile(snap, now, policy.Options{})
		if err != nil {
			return err
		}
		dev := map[string]policy.Device{}
		for _, d := range snap.Devices {
			dev[d.Short] = d
		}
		gw, g1, g2 := dev[s.gateway.ShortID], dev[s.gpu1.ShortID], dev[s.gpu2.ShortID]
		switch {
		case !c.Document.Allows(gw, g1, "tcp", 10443):
			return errors.New("the active grant is missing from the compiled policy")
		case c.Document.Allows(gw, g2, "tcp", 10443):
			return errors.New("the grant leaked to another owner's host")
		case !c.Document.Allows(gw, g2, "tcp", 22):
			return errors.New("the platform SSH rule is missing")
		case c.Document.Allows(g1, g2, "tcp", 22):
			return errors.New("provider hosts can reach each other")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
