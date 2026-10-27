package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/headmesh-dev/headmesh/core/policy"
	"github.com/headmesh-dev/headmesh/internal/store/dbq"
)

// errRemovedDevice marks a selector that names a device which no longer exists. The rule or grant holding it can
// never take effect again, so it is skipped rather than rewritten into some other selector.
var errRemovedDevice = errors.New("selector references a removed device")

// SelectorJSON is how rule and grant selectors are stored in jsonb columns. Device selectors reference the
// device by UUID so renames and re-enrolments never break them.
type SelectorJSON struct {
	Kind   string `json:"kind"` // all | users | role | device
	Role   string `json:"role,omitempty"`
	Device string `json:"device,omitempty"`
}

// PlatformSelectorJSON is the stored form of a platform rule side.
type PlatformSelectorJSON struct {
	Scope string `json:"scope"` // platform | children
	Role  string `json:"role,omitempty"`
}

// OrgSettings is the typed view of orgs.settings.
type OrgSettings struct {
	SSHEnabled        bool `json:"ssh_enabled,omitempty"`
	AllowSubnetRoutes bool `json:"allow_subnet_routes,omitempty"`
	AllowExitNodes    bool `json:"allow_exit_nodes,omitempty"`
	MaxGrantTTLHours  int  `json:"max_grant_ttl_hours,omitempty"`
}

// LoadSnapshot reads one cell's tenancy state as policy input. It must run in a System-scoped transaction.
// Grants are limited to those active at now or starting before now+horizon, which is everything a policy
// timeline over that horizon can need.
func LoadSnapshot(ctx context.Context, q *dbq.Queries, cell dbq.Cell, now time.Time, horizon time.Duration) (policy.Snapshot, error) {
	s := policy.Snapshot{Cell: cell.Name}
	orgs, err := q.ListOrgsInCell(ctx, cell.ID)
	if err != nil {
		return s, fmt.Errorf("load orgs: %w", err)
	}
	orgShort := map[pgtype.UUID]string{}
	for _, o := range orgs {
		orgShort[o.ID] = o.ShortID
	}
	for _, o := range orgs {
		var settings OrgSettings
		if err := json.Unmarshal(o.Settings, &settings); err != nil {
			return s, fmt.Errorf("org %s settings: %w", o.ShortID, err)
		}
		po := policy.Org{
			Short: o.ShortID, Template: policy.Template(o.Template), Platform: o.IsPlatform,
			SSH: settings.SSHEnabled, Users: o.HeadscaleUsers,
		}
		if o.ParentOrgID.Valid {
			po.Parent = orgShort[o.ParentOrgID]
		}
		s.Orgs = append(s.Orgs, po)
	}
	devices, err := q.ListDevicesInCell(ctx, cell.ID)
	if err != nil {
		return s, fmt.Errorf("load devices: %w", err)
	}
	devShort := map[pgtype.UUID]string{}
	for _, d := range devices {
		devShort[d.ID] = d.ShortID
		state, err := deviceState(d.State)
		if err != nil {
			return s, err
		}
		s.Devices = append(s.Devices, policy.Device{
			Short: d.ShortID, Org: orgShort[d.OrgID], Roles: d.Roles, User: deref(d.HeadscaleUser), State: state,
		})
	}
	rules, err := q.ListRulesInCell(ctx, cell.ID)
	if err != nil {
		return s, fmt.Errorf("load rules: %w", err)
	}
	for _, r := range rules {
		src, err := decodeSelector(r.Src, devShort)
		if errors.Is(err, errRemovedDevice) {
			continue
		} else if err != nil {
			return s, fmt.Errorf("rule %s src: %w", r.ID, err)
		}
		dst, err := decodeSelector(r.Dst, devShort)
		if errors.Is(err, errRemovedDevice) {
			continue
		} else if err != nil {
			return s, fmt.Errorf("rule %s dst: %w", r.ID, err)
		}
		s.Rules = append(s.Rules, policy.Rule{
			ID: r.ID.String(), Org: orgShort[r.OrgID], Src: src, Dst: dst, Ports: toPorts(r.Ports),
			StartsAt: derefTime(r.StartsAt), ExpiresAt: derefTime(r.ExpiresAt),
		})
	}
	platformRules, err := q.ListPlatformRulesInCell(ctx, cell.ID)
	if err != nil {
		return s, fmt.Errorf("load platform rules: %w", err)
	}
	for _, pr := range platformRules {
		src, err := decodePlatformSelector(pr.Src)
		if err != nil {
			return s, fmt.Errorf("platform rule %s src: %w", pr.ID, err)
		}
		dst, err := decodePlatformSelector(pr.Dst)
		if err != nil {
			return s, fmt.Errorf("platform rule %s dst: %w", pr.ID, err)
		}
		s.PlatformRules = append(s.PlatformRules, policy.PlatformRule{
			ID: pr.ID.String(), Platform: orgShort[pr.OrgID], Src: src, Dst: dst, Ports: toPorts(pr.Ports),
		})
	}
	grants, err := q.ListGrantsInCell(ctx, dbq.ListGrantsInCellParams{CellID: cell.ID, ExpiresAt: now, StartsAt: now.Add(horizon)})
	if err != nil {
		return s, fmt.Errorf("load grants: %w", err)
	}
	for _, g := range grants {
		src, err := decodeSelector(g.Src, devShort)
		if errors.Is(err, errRemovedDevice) {
			continue
		} else if err != nil {
			return s, fmt.Errorf("grant %s src: %w", g.ID, err)
		}
		dst, ok := devShort[g.DstDeviceID]
		if !ok {
			// The destination was removed; the grant can no longer take effect.
			continue
		}
		s.Grants = append(s.Grants, policy.Grant{
			ID: g.ID.String(), Grantor: orgShort[g.GrantorOrgID], Grantee: orgShort[g.GranteeOrgID], Src: src, Dst: dst,
			Ports: toPorts(g.Ports), StartsAt: g.StartsAt, ExpiresAt: g.ExpiresAt, Revoked: g.RevokedAt != nil,
		})
	}
	return s, nil
}

func deviceState(s string) (policy.DeviceState, error) {
	switch s {
	case "active":
		return policy.DeviceActive, nil
	case "quarantined":
		return policy.DeviceQuarantined, nil
	case "removed":
		return policy.DeviceRemoved, nil
	default:
		return 0, fmt.Errorf("unknown device state %q", s)
	}
}

func decodeSelector(raw []byte, devShort map[pgtype.UUID]string) (policy.Selector, error) {
	var sj SelectorJSON
	if err := json.Unmarshal(raw, &sj); err != nil {
		return policy.Selector{}, fmt.Errorf("decode selector: %w", err)
	}
	switch sj.Kind {
	case "all":
		return policy.All(), nil
	case "users":
		return policy.Users(), nil
	case "role":
		return policy.Role(sj.Role), nil
	case "device":
		id, err := UUID(sj.Device)
		if err != nil {
			return policy.Selector{}, err
		}
		short, ok := devShort[id]
		if !ok {
			return policy.Selector{}, errRemovedDevice
		}
		return policy.DeviceSel(short), nil
	default:
		return policy.Selector{}, fmt.Errorf("unknown selector kind %q", sj.Kind)
	}
}

func decodePlatformSelector(raw []byte) (policy.PlatformSelector, error) {
	var ps PlatformSelectorJSON
	if err := json.Unmarshal(raw, &ps); err != nil {
		return policy.PlatformSelector{}, fmt.Errorf("decode platform selector: %w", err)
	}
	switch ps.Scope {
	case "platform":
		return policy.PlatformSelector{Scope: policy.ScopePlatform, Role: ps.Role}, nil
	case "children":
		return policy.PlatformSelector{Scope: policy.ScopeChildren, Role: ps.Role}, nil
	default:
		return policy.PlatformSelector{}, fmt.Errorf("unknown platform scope %q", ps.Scope)
	}
}

func toPorts(ps []string) []policy.Port {
	out := make([]policy.Port, len(ps))
	for i, p := range ps {
		out[i] = policy.Port(p)
	}
	return out
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}
