package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/headmesh-dev/headmesh/internal/store"
	"github.com/headmesh-dev/headmesh/internal/store/dbq"
)

// BootstrapRequest creates a cell (if missing), an org in it and an admin API key. It is how an operator sets up a
// fresh installation from the command line before any user accounts exist.
type BootstrapRequest struct {
	CellName, ServerURL, BaseDomain, CellMode string
	OrgSlug, OrgName, Template                string
	Platform                                  bool
	ParentSlug                                string
	HeadscaleUsers                            []string
}

// BootstrapResult reports what was created. AdminKey is shown once.
type BootstrapResult struct {
	CellID, OrgID, OrgShort string
	AdminKey                *APIKey
}

// Bootstrap runs in the system scope; it is only reachable from the server binary, never over HTTP.
func (s *Service) Bootstrap(ctx context.Context, req BootstrapRequest) (*BootstrapResult, error) {
	if req.Template == "" {
		req.Template = "team"
	}
	if req.CellMode == "" {
		req.CellMode = "observe"
	}
	short, err := newShortID()
	if err != nil {
		return nil, err
	}
	var cell dbq.Cell
	var org dbq.Org
	err = s.Store.Tx(ctx, systemScope, func(q *dbq.Queries, _ pgx.Tx) error {
		var err error
		cell, err = q.GetCellByName(ctx, req.CellName)
		if errors.Is(err, pgx.ErrNoRows) {
			cell, err = q.CreateCell(ctx, dbq.CreateCellParams{
				Name: req.CellName, ServerUrl: req.ServerURL,
				BaseDomain: req.BaseDomain, Mode: req.CellMode,
			})
		}
		if err != nil {
			return fmt.Errorf("cell: %w", err)
		}
		var parent pgtype.UUID
		if req.ParentSlug != "" {
			p, err := q.GetOrgBySlug(ctx, req.ParentSlug)
			if err != nil {
				return fmt.Errorf("parent org %q: %w", req.ParentSlug, err)
			}
			parent = p.ID
		}
		org, err = q.CreateOrg(ctx, dbq.CreateOrgParams{
			ShortID: short, Slug: req.OrgSlug, Name: req.OrgName, CellID: cell.ID,
			ParentOrgID: parent, IsPlatform: req.Platform, Template: req.Template, HeadscaleUsers: nonNil(req.HeadscaleUsers),
		})
		if err != nil {
			return fmt.Errorf("org: %w", err)
		}
		_, err = store.AppendAudit(ctx, q, store.AuditEvent{
			OrgID: org.ID, ActorType: "system", ActorID: "bootstrap",
			Action: "org.created", TargetType: "org", TargetID: org.ID.String(),
			Data: map[string]any{"slug": req.OrgSlug, "cell": req.CellName, "platform": req.Platform},
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	key, err := s.issueKey(ctx, systemScope, org.ID, pgtype.UUID{}, "bootstrap admin", []string{ScopeAdmin}, 0)
	if err != nil {
		return nil, err
	}
	return &BootstrapResult{CellID: cell.ID.String(), OrgID: org.ID.String(), OrgShort: org.ShortID, AdminKey: key}, nil
}
