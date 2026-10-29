package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/headmesh-dev/headmesh/core/policy"
	"github.com/headmesh-dev/headmesh/internal/store"
	"github.com/headmesh-dev/headmesh/internal/store/dbq"
)

// RuleRequest is access inside the principal's org: devices matching Src may reach devices matching Dst on Ports.
// Without StartsAt the rule starts now; without ExpiresAt it stands until deleted.
type RuleRequest struct {
	Src, Dst  store.SelectorJSON
	Ports     []string
	StartsAt  *time.Time
	ExpiresAt *time.Time
}

// Rule is the API view of an intra-org rule.
type Rule struct {
	ID        string
	Src, Dst  store.SelectorJSON
	Ports     []string
	StartsAt  *time.Time
	ExpiresAt *time.Time
	Version   int32
	CreatedAt time.Time
	// Enforced is true when the cell confirmed the resulting policy is live.
	Enforced bool
}

// CreateRule adds a rule to the principal's org and pushes the new policy to the cell.
func (s *Service) CreateRule(ctx context.Context, p Principal, req RuleRequest) (*Rule, error) {
	if !p.Can(ScopeRulesWrite) {
		return nil, ErrForbidden
	}
	ports, src, dst, err := s.prepareRule(req)
	if err != nil {
		return nil, err
	}
	var r dbq.Rule
	var cellName string
	err = s.Store.Tx(ctx, p.scope(), func(q *dbq.Queries, _ pgx.Tx) error {
		org, err := q.GetOrg(ctx, p.OrgID)
		if err != nil {
			return fmt.Errorf("load org: %w", err)
		}
		if err := validateSelector(ctx, q, org, "src", "your org", req.Src); err != nil {
			return err
		}
		if err := validateSelector(ctx, q, org, "dst", "your org", req.Dst); err != nil {
			return err
		}
		cell, err := q.GetCell(ctx, org.CellID)
		if err != nil {
			return fmt.Errorf("load cell: %w", err)
		}
		cellName = cell.Name
		r, err = q.CreateRule(ctx, dbq.CreateRuleParams{
			OrgID: p.OrgID, Src: src, Dst: dst, Ports: ports, StartsAt: req.StartsAt, ExpiresAt: req.ExpiresAt,
		})
		if err != nil {
			return fmt.Errorf("create rule: %w", err)
		}
		_, err = store.AppendAudit(ctx, q, store.AuditEvent{
			OrgID: p.OrgID, ActorType: "api_key", ActorID: p.KeyID.String(),
			Action: "rule.created", TargetType: "rule", TargetID: r.ID.String(), Data: map[string]any{
				"src": req.Src, "dst": req.Dst, "ports": ports, "starts_at": req.StartsAt, "expires_at": req.ExpiresAt,
			},
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	out, err := toRule(r)
	if err != nil {
		return nil, err
	}
	out.Enforced = s.Hub.RefreshAndWait(ctx, cellName) == nil
	return out, nil
}

// prepareRule checks a rule's ports and times and encodes its selectors. The selectors themselves are checked
// against the org inside the write's transaction.
func (s *Service) prepareRule(req RuleRequest) (ports []string, src, dst []byte, err error) {
	ports = make([]string, 0, len(req.Ports))
	for _, raw := range req.Ports {
		pt, err := policy.ParsePort(raw)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("%w: port %q (for example tcp:22, udp:5000-5100 or * for any)", ErrInvalid, raw)
		}
		ports = append(ports, string(pt))
	}
	if len(ports) == 0 {
		return nil, nil, nil, fmt.Errorf("%w: at least one port is required", ErrInvalid)
	}
	if req.ExpiresAt != nil {
		starts := s.now()
		if req.StartsAt != nil {
			starts = *req.StartsAt
		}
		if !req.ExpiresAt.After(starts) || !req.ExpiresAt.After(s.now()) {
			return nil, nil, nil, fmt.Errorf("%w: expires_at must be in the future and after starts_at", ErrInvalid)
		}
	}
	if src, err = json.Marshal(req.Src); err != nil {
		return nil, nil, nil, fmt.Errorf("encode selector: %w", err)
	}
	if dst, err = json.Marshal(req.Dst); err != nil {
		return nil, nil, nil, fmt.Errorf("encode selector: %w", err)
	}
	return ports, src, dst, nil
}

// UpdateRule replaces a rule's selectors, ports and times. ifVersion is the version the caller read (from the rule's
// ETag); a rule changed since then is not overwritten (ErrPreconditionFailed). Zero skips the check.
func (s *Service) UpdateRule(ctx context.Context, p Principal, ruleID string, req RuleRequest, ifVersion int32) (*Rule, error) {
	if !p.Can(ScopeRulesWrite) {
		return nil, ErrForbidden
	}
	id, err := store.UUID(ruleID)
	if err != nil {
		return nil, fmt.Errorf("%w: rule id", ErrInvalid)
	}
	ports, src, dst, err := s.prepareRule(req)
	if err != nil {
		return nil, err
	}
	var r dbq.Rule
	var cellName string
	err = s.Store.Tx(ctx, p.scope(), func(q *dbq.Queries, _ pgx.Tx) error {
		old, err := q.GetRule(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && old.OrgID != p.OrgID) {
			return ErrNotFound
		} else if err != nil {
			return fmt.Errorf("load rule: %w", err)
		}
		if ifVersion != 0 && old.Version != ifVersion {
			return fmt.Errorf("%w: the rule is at version %d, not %d", ErrPreconditionFailed, old.Version, ifVersion)
		}
		org, err := q.GetOrg(ctx, p.OrgID)
		if err != nil {
			return fmt.Errorf("load org: %w", err)
		}
		if err := validateSelector(ctx, q, org, "src", "your org", req.Src); err != nil {
			return err
		}
		if err := validateSelector(ctx, q, org, "dst", "your org", req.Dst); err != nil {
			return err
		}
		cell, err := q.GetCell(ctx, org.CellID)
		if err != nil {
			return fmt.Errorf("load cell: %w", err)
		}
		cellName = cell.Name
		r, err = q.UpdateRule(ctx, dbq.UpdateRuleParams{
			ID: id, OrgID: p.OrgID, Src: src, Dst: dst, Ports: ports, StartsAt: req.StartsAt, ExpiresAt: req.ExpiresAt,
			Version: old.Version,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: the rule changed while it was being updated", ErrPreconditionFailed)
		} else if err != nil {
			return fmt.Errorf("update rule: %w", err)
		}
		before, err := toRule(old)
		if err != nil {
			return err
		}
		_, err = store.AppendAudit(ctx, q, store.AuditEvent{
			OrgID: p.OrgID, ActorType: "api_key", ActorID: p.KeyID.String(),
			Action: "rule.updated", TargetType: "rule", TargetID: r.ID.String(), Data: map[string]any{
				"version": r.Version,
				"before":  map[string]any{"src": before.Src, "dst": before.Dst, "ports": old.Ports, "starts_at": old.StartsAt, "expires_at": old.ExpiresAt},
				"after":   map[string]any{"src": req.Src, "dst": req.Dst, "ports": ports, "starts_at": req.StartsAt, "expires_at": req.ExpiresAt},
			},
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	out, err := toRule(r)
	if err != nil {
		return nil, err
	}
	out.Enforced = s.Hub.RefreshAndWait(ctx, cellName) == nil
	return out, nil
}

// ListRules returns the principal's org's rules, oldest first.
func (s *Service) ListRules(ctx context.Context, p Principal) ([]*Rule, error) {
	if !p.Can(ScopeRulesRead) && !p.Can(ScopeRulesWrite) {
		return nil, ErrForbidden
	}
	var out []*Rule
	err := s.Store.Tx(ctx, p.scope(), func(q *dbq.Queries, _ pgx.Tx) error {
		rs, err := q.ListRulesByOrg(ctx, p.OrgID)
		if err != nil {
			return fmt.Errorf("list rules: %w", err)
		}
		for _, r := range rs {
			v, err := toRule(r)
			if err != nil {
				return err
			}
			out = append(out, v)
		}
		return nil
	})
	return out, err
}

// GetRule returns one of the principal's org's rules.
func (s *Service) GetRule(ctx context.Context, p Principal, ruleID string) (*Rule, error) {
	if !p.Can(ScopeRulesRead) && !p.Can(ScopeRulesWrite) {
		return nil, ErrForbidden
	}
	id, err := store.UUID(ruleID)
	if err != nil {
		return nil, fmt.Errorf("%w: rule id", ErrInvalid)
	}
	var out *Rule
	err = s.Store.Tx(ctx, p.scope(), func(q *dbq.Queries, _ pgx.Tx) error {
		r, err := q.GetRule(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && r.OrgID != p.OrgID) {
			return ErrNotFound
		} else if err != nil {
			return fmt.Errorf("load rule: %w", err)
		}
		out, err = toRule(r)
		return err
	})
	return out, err
}

// DeleteRule removes a rule and waits until the cell enforces the policy without it.
func (s *Service) DeleteRule(ctx context.Context, p Principal, ruleID string) (*Rule, error) {
	if !p.Can(ScopeRulesWrite) {
		return nil, ErrForbidden
	}
	id, err := store.UUID(ruleID)
	if err != nil {
		return nil, fmt.Errorf("%w: rule id", ErrInvalid)
	}
	var out *Rule
	var cellName string
	err = s.Store.Tx(ctx, p.scope(), func(q *dbq.Queries, _ pgx.Tx) error {
		r, err := q.GetRule(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && r.OrgID != p.OrgID) {
			return ErrNotFound
		} else if err != nil {
			return fmt.Errorf("load rule: %w", err)
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
		if err := q.DeleteRule(ctx, dbq.DeleteRuleParams{ID: id, OrgID: p.OrgID}); err != nil {
			return fmt.Errorf("delete rule: %w", err)
		}
		if out, err = toRule(r); err != nil {
			return err
		}
		_, err = store.AppendAudit(ctx, q, store.AuditEvent{
			OrgID: p.OrgID, ActorType: "api_key", ActorID: p.KeyID.String(),
			Action: "rule.deleted", TargetType: "rule", TargetID: r.ID.String(), Data: map[string]any{
				"src": out.Src, "dst": out.Dst, "ports": r.Ports,
			},
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	out.Enforced = s.Hub.RefreshAndWait(ctx, cellName) == nil
	return out, nil
}

func toRule(r dbq.Rule) (*Rule, error) {
	out := &Rule{
		ID: r.ID.String(), Ports: r.Ports, StartsAt: r.StartsAt, ExpiresAt: r.ExpiresAt, Version: r.Version,
		CreatedAt: r.CreatedAt,
	}
	if err := json.Unmarshal(r.Src, &out.Src); err != nil {
		return nil, fmt.Errorf("decode rule %s src: %w", out.ID, err)
	}
	if err := json.Unmarshal(r.Dst, &out.Dst); err != nil {
		return nil, fmt.Errorf("decode rule %s dst: %w", out.ID, err)
	}
	return out, nil
}
