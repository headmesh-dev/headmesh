package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/headmesh-dev/headmesh/core/policy"
	"github.com/headmesh-dev/headmesh/internal/store"
	"github.com/headmesh-dev/headmesh/internal/store/dbq"
)

// GrantRequest gives devices of another org time-limited access to one device of the principal's org.
type GrantRequest struct {
	GranteeOrg  string // short id of the org receiving access
	Src         store.SelectorJSON
	DstDeviceID string
	// DstService names one of the grantor's services instead of DstDeviceID; Ports then default to the service's
	// ports and may only narrow them. The grant records the device and ports as they are now.
	DstService  string
	Ports       []string
	StartsAt    *time.Time
	ExpiresAt   time.Time
	Reason      string
	ExternalRef string // idempotency key, for example a rental id
}

// Grant is the API view of a grant.
type Grant struct {
	ID          string
	GrantorOrg  string
	GranteeOrg  string
	DstDeviceID string
	// DstService names one of the grantor's services instead of DstDeviceID; Ports then default to the service's
	// ports and may only narrow them. The grant records the device and ports as they are now.
	DstService  string
	Ports       []string
	StartsAt    time.Time
	ExpiresAt   time.Time
	Revoked     bool
	ExternalRef string
	// Enforced is true when the cell confirmed the resulting policy is live.
	Enforced bool
}

func (s *Service) maxGrantTTL() time.Duration {
	if s.MaxGrantTTL > 0 {
		return s.MaxGrantTTL
	}
	return 30 * 24 * time.Hour
}

// CreateGrant creates (or, with the same external ref, returns) a grant and pushes the new policy to the cell.
func (s *Service) CreateGrant(ctx context.Context, p Principal, req GrantRequest) (*Grant, error) {
	if !p.Can(ScopeGrantsWrite) {
		return nil, ErrForbidden
	}
	if req.DstService != "" {
		if req.DstDeviceID != "" {
			return nil, fmt.Errorf("%w: give dst_device_id or dst_service, not both", ErrInvalid)
		}
		if err := s.resolveGrantService(ctx, p, &req); err != nil {
			return nil, err
		}
	} else if req.DstDeviceID == "" {
		return nil, fmt.Errorf("%w: give dst_device_id or dst_service", ErrInvalid)
	}
	ports := make([]string, 0, len(req.Ports))
	for _, raw := range req.Ports {
		pt, err := policy.ParsePort(raw)
		if err != nil || pt == policy.PortAny {
			return nil, fmt.Errorf("%w: port %q (grants need explicit ports such as tcp:22)", ErrInvalid, raw)
		}
		ports = append(ports, string(pt))
	}
	if len(ports) == 0 {
		return nil, fmt.Errorf("%w: at least one port is required", ErrInvalid)
	}
	now := s.now()
	starts := now
	if req.StartsAt != nil {
		starts = *req.StartsAt
	}
	if !req.ExpiresAt.After(starts) || req.ExpiresAt.Sub(now) > s.maxGrantTTL() {
		return nil, fmt.Errorf("%w: expires_at must be after starts_at and within %s", ErrInvalid, s.maxGrantTTL())
	}
	dstID, err := store.UUID(req.DstDeviceID)
	if err != nil {
		return nil, fmt.Errorf("%w: dst_device_id", ErrInvalid)
	}

	// Resolve the grantee and validate the source selector in the system scope (the grantor cannot see the
	// grantee's rows), then write in the grantor's scope.
	var grantee dbq.Org
	var cell dbq.Cell
	err = s.Store.Tx(ctx, systemScope, func(q *dbq.Queries, _ pgx.Tx) error {
		var err error
		if grantee, err = q.GetOrgByShortID(ctx, req.GranteeOrg); errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: grantee org", ErrNotFound)
		} else if err != nil {
			return fmt.Errorf("load grantee: %w", err)
		}
		grantor, err := q.GetOrg(ctx, p.OrgID)
		if err != nil {
			return fmt.Errorf("load grantor: %w", err)
		}
		if grantee.ID == grantor.ID {
			return fmt.Errorf("%w: use a rule for access within your own org", ErrInvalid)
		}
		if grantee.CellID != grantor.CellID {
			return fmt.Errorf("%w: grantee is in another cell", ErrInvalid)
		}
		if cell, err = q.GetCell(ctx, grantor.CellID); err != nil {
			return fmt.Errorf("load cell: %w", err)
		}
		return validateGranteeSelector(ctx, q, grantee, req.Src)
	})
	if err != nil {
		return nil, err
	}
	src, err := json.Marshal(req.Src)
	if err != nil {
		return nil, fmt.Errorf("encode selector: %w", err)
	}
	var g dbq.Grant
	err = s.Store.Tx(ctx, p.scope(), func(q *dbq.Queries, _ pgx.Tx) error {
		dst, err := q.GetDevice(ctx, dstID)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: destination device is not in your org", ErrNotFound)
		} else if err != nil {
			return fmt.Errorf("load device: %w", err)
		}
		if dst.State != "active" || dst.HeadscaleUser != nil {
			return fmt.Errorf("%w: destination must be an active tagged device", ErrInvalid)
		}
		g, err = q.CreateGrant(ctx, dbq.CreateGrantParams{
			GrantorOrgID: p.OrgID, GranteeOrgID: grantee.ID, Src: src, DstDeviceID: dst.ID, Ports: ports,
			StartsAt: starts, ExpiresAt: req.ExpiresAt, Reason: strPtr(req.Reason), ExternalRef: strPtr(req.ExternalRef),
		})
		if errors.Is(err, pgx.ErrNoRows) && req.ExternalRef != "" {
			g, err = q.GetGrantByExternalRef(ctx, dbq.GetGrantByExternalRefParams{GrantorOrgID: p.OrgID, ExternalRef: &req.ExternalRef})
			if err != nil {
				return fmt.Errorf("load existing grant: %w", err)
			}
			return nil // idempotent retry: no new audit entry
		}
		if err != nil {
			return fmt.Errorf("create grant: %w", err)
		}
		_, err = store.AppendAudit(ctx, q, store.AuditEvent{
			OrgID: p.OrgID, ActorType: "api_key", ActorID: p.KeyID.String(),
			Action: "grant.created", TargetType: "grant", TargetID: g.ID.String(), Data: map[string]any{
				"grantee_org": req.GranteeOrg, "dst_device": req.DstDeviceID, "dst_service": req.DstService, "ports": ports,
				"starts_at": starts, "expires_at": req.ExpiresAt, "external_ref": req.ExternalRef,
			},
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	out := toGrant(g, p.OrgShort, req.GranteeOrg)
	out.Enforced = s.Hub.RefreshAndWait(ctx, cell.Name) == nil
	return out, nil
}

// resolveGrantService turns req.DstService into the service's device and ports. Ports the caller lists must be
// ports of the service: a grant can open part of a service, never more.
func (s *Service) resolveGrantService(ctx context.Context, p Principal, req *GrantRequest) error {
	return s.Store.Tx(ctx, p.scope(), func(q *dbq.Queries, _ pgx.Tx) error {
		svc, err := q.GetServiceByName(ctx, dbq.GetServiceByNameParams{OrgID: p.OrgID, Name: req.DstService})
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: your org has no service named %q", ErrNotFound, req.DstService)
		} else if err != nil {
			return fmt.Errorf("load service: %w", err)
		}
		req.DstDeviceID = svc.DeviceID.String()
		if len(req.Ports) == 0 {
			req.Ports = svc.Ports
			return nil
		}
		for _, raw := range req.Ports {
			pt, err := policy.ParsePort(raw)
			if err != nil || !slices.Contains(svc.Ports, string(pt)) {
				return fmt.Errorf("%w: port %q isn't one of service %s's ports (%s)", ErrInvalid, raw, svc.Name,
					strings.Join(svc.Ports, ", "))
			}
		}
		return nil
	})
}

func validateGranteeSelector(ctx context.Context, q *dbq.Queries, grantee dbq.Org, sel store.SelectorJSON) error {
	return validateSelector(ctx, q, grantee, "src", "the grantee org", sel)
}

// validateSelector checks that sel names devices of org. side ("src", "dst") and whose ("your org") only shape
// the error messages.
func validateSelector(ctx context.Context, q *dbq.Queries, org dbq.Org, side, whose string, sel store.SelectorJSON) error {
	switch sel.Kind {
	case "all":
		return nil
	case "users":
		if len(org.HeadscaleUsers) == 0 {
			return fmt.Errorf("%w: %s has no personal devices", ErrInvalid, whose)
		}
		return nil
	case "role":
		if err := policy.ValidateRole(sel.Role); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		return nil
	case "device":
		id, err := store.UUID(sel.Device)
		if err != nil {
			return fmt.Errorf("%w: %s device", ErrInvalid, side)
		}
		d, err := q.GetDevice(ctx, id)
		if err != nil || d.OrgID != org.ID || d.State == "removed" {
			return fmt.Errorf("%w: %s device is not a device of %s", ErrInvalid, side, whose)
		}
		return nil
	default:
		return fmt.Errorf("%w: %s.kind must be all, users, role or device", ErrInvalid, side)
	}
}

// RevokeGrant revokes a grant the principal's org issued and waits until the cell enforces it.
func (s *Service) RevokeGrant(ctx context.Context, p Principal, grantID string) (*Grant, error) {
	if !p.Can(ScopeGrantsWrite) {
		return nil, ErrForbidden
	}
	id, err := store.UUID(grantID)
	if err != nil {
		return nil, fmt.Errorf("%w: grant id", ErrInvalid)
	}
	var g dbq.Grant
	var cellName string
	err = s.Store.Tx(ctx, p.scope(), func(q *dbq.Queries, _ pgx.Tx) error {
		existing, err := q.GetGrant(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return fmt.Errorf("load grant: %w", err)
		}
		if existing.GrantorOrgID != p.OrgID {
			return fmt.Errorf("%w: only the granting org can revoke", ErrForbidden)
		}
		org, err := q.GetOrg(ctx, p.OrgID)
		if err != nil {
			return fmt.Errorf("load org: %w", err)
		}
		cell, err := q.GetCell(ctx, org.CellID)
		if err != nil {
			return fmt.Errorf("load cell: %w", err)
		}
		cellName = cell.Name
		g, err = q.RevokeGrant(ctx, dbq.RevokeGrantParams{ID: id})
		if errors.Is(err, pgx.ErrNoRows) {
			g = existing // already revoked: idempotent
			return nil
		} else if err != nil {
			return fmt.Errorf("revoke grant: %w", err)
		}
		_, err = store.AppendAudit(ctx, q, store.AuditEvent{
			OrgID: p.OrgID, ActorType: "api_key", ActorID: p.KeyID.String(),
			Action: "grant.revoked", TargetType: "grant", TargetID: g.ID.String(),
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	out := toGrant(g, p.OrgShort, "")
	out.Enforced = s.Hub.RefreshAndWait(ctx, cellName) == nil
	return out, nil
}

// ListGrants returns active grants the principal's org issued or received.
func (s *Service) ListGrants(ctx context.Context, p Principal) ([]*Grant, error) {
	if !p.Can(ScopeGrantsRead) && !p.Can(ScopeGrantsWrite) {
		return nil, ErrForbidden
	}
	var out []*Grant
	err := s.Store.Tx(ctx, p.scope(), func(q *dbq.Queries, _ pgx.Tx) error {
		gs, err := q.ListGrantsForOrg(ctx, p.OrgID)
		if err != nil {
			return fmt.Errorf("list grants: %w", err)
		}
		for _, g := range gs {
			out = append(out, toGrant(g, "", ""))
		}
		return nil
	})
	return out, err
}

func toGrant(g dbq.Grant, grantor, grantee string) *Grant {
	out := &Grant{
		ID: g.ID.String(), GrantorOrg: grantor, GranteeOrg: grantee, DstDeviceID: uuidString(g.DstDeviceID),
		Ports: g.Ports, StartsAt: g.StartsAt, ExpiresAt: g.ExpiresAt, Revoked: g.RevokedAt != nil,
	}
	if g.ExternalRef != nil {
		out.ExternalRef = *g.ExternalRef
	}
	return out
}
