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

// RulePair is one pair of devices a previewed rule lets through.
type RulePair struct {
	SrcID, SrcName string
	DstID, DstName string
	// New are the rule's ports this pair can't reach yet; Already are those something else already allows.
	New, Already []string
}

// RulePreview is what creating a rule would change.
type RulePreview struct {
	// At is the instant previewed: now, or the rule's starts_at when that is later.
	At       time.Time
	Pairs    []RulePair
	NewPairs int // pairs that gain at least one port
	// Truncated is true when more than maxPreviewPairs pairs matched; NewPairs still counts them all.
	Truncated bool
}

const maxPreviewPairs = 500

// PreviewRule reports what CreateRule would change, without changing anything: every pair of the org's devices the
// rule lets through, and which of its ports are new for that pair. It validates exactly as CreateRule does, and
// refuses a rule the compiler would refuse.
func (s *Service) PreviewRule(ctx context.Context, p Principal, req RuleRequest) (*RulePreview, error) {
	if !p.Can(ScopeRulesRead) && !p.Can(ScopeRulesWrite) {
		return nil, ErrForbidden
	}
	ports, _, _, err := s.prepareRule(req)
	if err != nil {
		return nil, err
	}
	out := &RulePreview{At: s.now()}
	if req.StartsAt != nil && req.StartsAt.After(out.At) {
		out.At = *req.StartsAt
	}
	var org dbq.Org
	byShort := map[string]dbq.Device{}
	var rule policy.Rule
	err = s.Store.Tx(ctx, p.scope(), func(q *dbq.Queries, _ pgx.Tx) error {
		var err error
		if org, err = q.GetOrg(ctx, p.OrgID); err != nil {
			return fmt.Errorf("load org: %w", err)
		}
		if err := validateSelector(ctx, q, org, "src", "your org", req.Src); err != nil {
			return err
		}
		if err := validateSelector(ctx, q, org, "dst", "your org", req.Dst); err != nil {
			return err
		}
		devices, err := q.ListDevicesByOrg(ctx, p.OrgID)
		if err != nil {
			return fmt.Errorf("list devices: %w", err)
		}
		byID := map[string]dbq.Device{}
		for _, d := range devices {
			byShort[d.ShortID] = d
			byID[d.ID.String()] = d
		}
		rule = policy.Rule{ID: "preview", Org: org.ShortID, StartsAt: derefTime(req.StartsAt), ExpiresAt: derefTime(req.ExpiresAt)}
		if rule.Src, err = policySelector(req.Src, byID); err != nil {
			return err
		}
		if rule.Dst, err = policySelector(req.Dst, byID); err != nil {
			return err
		}
		for _, pt := range ports {
			rule.Ports = append(rule.Ports, policy.Port(pt))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var snap policy.Snapshot
	err = s.Store.Tx(ctx, systemScope, func(q *dbq.Queries, _ pgx.Tx) error {
		cell, err := q.GetCell(ctx, org.CellID)
		if err != nil {
			return fmt.Errorf("load cell: %w", err)
		}
		snap, err = store.LoadSnapshot(ctx, q, cell, out.At, 0)
		return err
	})
	if err != nil {
		return nil, err
	}
	deltas, err := policy.PreviewRule(snap, out.At, rule)
	if errors.Is(err, policy.ErrInvalid) {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	} else if err != nil {
		return nil, fmt.Errorf("preview: %w", err)
	}
	for _, d := range deltas {
		if len(d.New) > 0 {
			out.NewPairs++
		}
		if len(out.Pairs) == maxPreviewPairs {
			out.Truncated = true
			continue
		}
		src, dst := byShort[d.Src], byShort[d.Dst]
		pair := RulePair{SrcID: src.ID.String(), SrcName: src.GivenName, DstID: dst.ID.String(), DstName: dst.GivenName}
		for _, pt := range d.New {
			pair.New = append(pair.New, string(pt))
		}
		for _, pt := range d.Already {
			pair.Already = append(pair.Already, string(pt))
		}
		out.Pairs = append(out.Pairs, pair)
	}
	return out, nil
}

// policySelector turns a stored selector into the compiler's form; device selectors name devices by UUID.
func policySelector(sel store.SelectorJSON, byID map[string]dbq.Device) (policy.Selector, error) {
	switch sel.Kind {
	case "all":
		return policy.All(), nil
	case "users":
		return policy.Users(), nil
	case "role":
		return policy.Role(sel.Role), nil
	case "device":
		id, err := store.UUID(sel.Device)
		if err != nil {
			return policy.Selector{}, fmt.Errorf("%w: device", ErrInvalid)
		}
		d, ok := byID[id.String()]
		if !ok {
			return policy.Selector{}, fmt.Errorf("%w: device is not in your org", ErrInvalid)
		}
		return policy.DeviceSel(d.ShortID), nil
	default:
		return policy.Selector{}, fmt.Errorf("%w: unknown selector kind %q", ErrInvalid, sel.Kind)
	}
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}
