// Package policy compiles Headmesh tenancy state into a Headscale (v0.29+) policy document.
//
// The compiler is a pure function: the same Snapshot and instant always produce byte-identical output. Tenant
// isolation is enforced twice — once by construction here, and once by Verify, an independent checker that
// re-derives every cross-organisation edge from the rendered document and rejects anything not justified by an
// active grant or a platform rule. Nothing is sent to a cell unless Verify passes.
package policy

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"
)

var (
	shortIDPattern = regexp.MustCompile(`^[a-z2-7]{10}$`)
	rolePattern    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
	userPattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
)

// Template seeds an organisation's intra-org access before any explicit rules.
type Template string

const (
	// TemplateTeam lets every device in the org reach every other device in the org on any port.
	TemplateTeam Template = "team"
	// TemplateLocked allows nothing until rules are added. Used for platform child orgs.
	TemplateLocked Template = "locked"
)

// Org is one isolated tenant network inside a cell.
type Org struct {
	Short    string   // 10-char base32 id used in tags; immutable
	Template Template // intra-org baseline
	Parent   string   // Short of the platform org that owns this child org, if any
	Platform bool     // true when this org may own child orgs and define PlatformRules
	SSH      bool     // emit an intra-org Tailscale SSH rule for non-root users
	// Users are Headscale users whose personal (untagged) nodes belong to this org. Only used for cells that
	// were adopted with existing user-owned nodes; Headmesh-enrolled nodes are always tagged.
	Users []string
}

// DeviceState mirrors the lifecycle of a device record.
type DeviceState int

const (
	DeviceActive DeviceState = iota + 1
	DeviceQuarantined
	DeviceRemoved
)

// Device is one node. Tagged devices carry org, role and device tags; user-owned devices (adopted cells only)
// are addressed through their Headscale user.
type Device struct {
	Short string
	Org   string
	Roles []string
	User  string // non-empty for user-owned nodes in adopted cells
	State DeviceState
}

// SelectorKind picks which devices of an org a rule side refers to.
type SelectorKind int

const (
	SelectAll SelectorKind = iota + 1
	SelectRole
	SelectDevice
	// SelectUsers selects only the org's user-owned (personal) nodes. Adopted cells use it for rules such as
	// "the operator's own devices may reach everything".
	SelectUsers
)

// Selector refers to devices within a single org.
type Selector struct {
	Kind   SelectorKind
	Role   string // for SelectRole
	Device string // device Short, for SelectDevice
}

// All selects every device of the org.
func All() Selector { return Selector{Kind: SelectAll} }

// Role selects devices carrying role r.
func Role(r string) Selector { return Selector{Kind: SelectRole, Role: r} }

// Users selects the org's user-owned nodes.
func Users() Selector { return Selector{Kind: SelectUsers} }

// DeviceSel selects exactly one device.
func DeviceSel(short string) Selector { return Selector{Kind: SelectDevice, Device: short} }

// Rule is intra-org access. A zero StartsAt/ExpiresAt means unbounded on that side.
type Rule struct {
	ID        string
	Org       string
	Src, Dst  Selector
	Ports     []Port
	StartsAt  time.Time
	ExpiresAt time.Time
}

// ActiveAt reports whether the rule is in force at t.
func (r Rule) ActiveAt(t time.Time) bool {
	if !r.StartsAt.IsZero() && t.Before(r.StartsAt) {
		return false
	}
	return r.ExpiresAt.IsZero() || t.Before(r.ExpiresAt)
}

// Grant is time-limited cross-org access: devices of Grantee (Src) may reach exactly one device of Grantor (Dst)
// on Ports. ExpiresAt is mandatory.
type Grant struct {
	ID        string
	Grantor   string
	Grantee   string
	Src       Selector // within Grantee
	Dst       string   // device Short within Grantor
	Ports     []Port
	StartsAt  time.Time
	ExpiresAt time.Time
	Revoked   bool
}

// ActiveAt reports whether the grant is in force at t.
func (g Grant) ActiveAt(t time.Time) bool {
	return !g.Revoked && !t.Before(g.StartsAt) && t.Before(g.ExpiresAt)
}

// Scope says whether a platform selector refers to the platform's own devices or to its child orgs' devices.
type Scope int

const (
	ScopePlatform Scope = iota + 1
	ScopeChildren
)

// PlatformSelector selects devices of a platform org or of all its child orgs, optionally by role.
type PlatformSelector struct {
	Scope Scope
	Role  string // empty means every device in scope
}

// PlatformRule is standing access between a platform and its child orgs (for example gateway nodes → every GPU
// host on tcp:22, or every GPU host → the platform's SPIRE server). Exactly one side is ScopeChildren, so child
// orgs can never reach each other through a platform rule.
type PlatformRule struct {
	ID       string
	Platform string
	Src, Dst PlatformSelector
	Ports    []Port
}

// Snapshot is a consistent view of one cell's tenancy state.
type Snapshot struct {
	Cell          string
	Orgs          []Org
	Devices       []Device
	Rules         []Rule
	Grants        []Grant
	PlatformRules []PlatformRule
}

// ErrInvalid wraps every validation failure.
var ErrInvalid = errors.New("invalid policy input")

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, args...)...)
}

// index is the validated, lookup-friendly form of a Snapshot.
type index struct {
	orgs    map[string]Org
	devices map[string]Device
	// children maps a platform Short to its child org Shorts (sorted); childSet is the same as a set.
	children map[string][]string
	childSet map[string]map[string]bool
	// users maps a Headscale user to the org its personal nodes belong to.
	users map[string]string
}

// Validate checks referential integrity and naming rules. Compile calls it; callers may use it to reject writes
// early.
func (s Snapshot) Validate() error {
	_, err := s.index()
	return err
}

func (s Snapshot) index() (*index, error) {
	ix := &index{
		orgs:     make(map[string]Org, len(s.Orgs)),
		devices:  make(map[string]Device, len(s.Devices)),
		children: map[string][]string{},
		childSet: map[string]map[string]bool{},
	}
	users := map[string]string{}
	ix.users = users
	for _, o := range s.Orgs {
		if !shortIDPattern.MatchString(o.Short) {
			return nil, invalidf("org short id %q", o.Short)
		}
		if _, dup := ix.orgs[o.Short]; dup {
			return nil, invalidf("duplicate org %s", o.Short)
		}
		if o.Template != TemplateTeam && o.Template != TemplateLocked {
			return nil, invalidf("org %s: unknown template %q", o.Short, o.Template)
		}
		for _, u := range o.Users {
			if !userPattern.MatchString(u) {
				return nil, invalidf("org %s: headscale user %q", o.Short, u)
			}
			if prev, taken := users[u]; taken {
				return nil, invalidf("headscale user %q belongs to both %s and %s", u, prev, o.Short)
			}
			users[u] = o.Short
		}
		ix.orgs[o.Short] = o
	}
	for _, o := range s.Orgs {
		if o.Parent == "" {
			continue
		}
		p, ok := ix.orgs[o.Parent]
		if !ok || !p.Platform {
			return nil, invalidf("org %s: parent %q is not a platform org in this cell", o.Short, o.Parent)
		}
		if o.Platform {
			return nil, invalidf("org %s: a child org cannot itself be a platform", o.Short)
		}
		ix.children[o.Parent] = append(ix.children[o.Parent], o.Short)
		if ix.childSet[o.Parent] == nil {
			ix.childSet[o.Parent] = map[string]bool{}
		}
		ix.childSet[o.Parent][o.Short] = true
	}
	for k := range ix.children {
		slices.Sort(ix.children[k])
	}
	for _, d := range s.Devices {
		if !shortIDPattern.MatchString(d.Short) {
			return nil, invalidf("device short id %q", d.Short)
		}
		if _, dup := ix.devices[d.Short]; dup {
			return nil, invalidf("duplicate device %s", d.Short)
		}
		o, ok := ix.orgs[d.Org]
		if !ok {
			return nil, invalidf("device %s: unknown org %q", d.Short, d.Org)
		}
		if d.State < DeviceActive || d.State > DeviceRemoved {
			return nil, invalidf("device %s: unknown state %d", d.Short, d.State)
		}
		if d.User != "" && !slices.Contains(o.Users, d.User) {
			return nil, invalidf("device %s: user %q is not a user of org %s", d.Short, d.User, d.Org)
		}
		if d.User != "" && len(d.Roles) > 0 {
			return nil, invalidf("device %s: user-owned devices cannot carry roles (roles are tags)", d.Short)
		}
		for _, r := range d.Roles {
			if !rolePattern.MatchString(r) {
				return nil, invalidf("device %s: role %q", d.Short, r)
			}
		}
		ix.devices[d.Short] = d
	}
	for _, r := range s.Rules {
		if _, ok := ix.orgs[r.Org]; !ok {
			return nil, invalidf("rule %s: unknown org %q", r.ID, r.Org)
		}
		if err := ix.checkSelector(r.Org, r.Src); err != nil {
			return nil, invalidf("rule %s src: %v", r.ID, err)
		}
		if err := ix.checkSelector(r.Org, r.Dst); err != nil {
			return nil, invalidf("rule %s dst: %v", r.ID, err)
		}
		if err := checkPorts(r.Ports, true); err != nil {
			return nil, invalidf("rule %s: %v", r.ID, err)
		}
		if !r.StartsAt.IsZero() && !r.ExpiresAt.IsZero() && !r.ExpiresAt.After(r.StartsAt) {
			return nil, invalidf("rule %s: expires_at must be after starts_at", r.ID)
		}
	}
	for _, g := range s.Grants {
		if err := ix.checkGrant(g); err != nil {
			return nil, err
		}
	}
	for _, pr := range s.PlatformRules {
		if err := ix.checkPlatformRule(pr); err != nil {
			return nil, err
		}
	}
	return ix, nil
}

func (ix *index) checkSelector(org string, sel Selector) error {
	switch sel.Kind {
	case SelectAll:
		return nil
	case SelectUsers:
		if len(ix.orgs[org].Users) == 0 {
			return fmt.Errorf("org %s has no Headscale users", org)
		}
		return nil
	case SelectRole:
		if !rolePattern.MatchString(sel.Role) {
			return fmt.Errorf("role %q", sel.Role)
		}
		return nil
	case SelectDevice:
		d, ok := ix.devices[sel.Device]
		if !ok {
			return fmt.Errorf("unknown device %q", sel.Device)
		}
		if d.Org != org {
			return fmt.Errorf("device %s belongs to org %s, not %s", d.Short, d.Org, org)
		}
		if d.User != "" {
			return fmt.Errorf("device %s is user-owned and cannot be selected individually", d.Short)
		}
		return nil
	default:
		return fmt.Errorf("unknown selector kind %d", sel.Kind)
	}
}

func (ix *index) checkGrant(g Grant) error {
	if _, ok := ix.orgs[g.Grantor]; !ok {
		return invalidf("grant %s: unknown grantor %q", g.ID, g.Grantor)
	}
	if _, ok := ix.orgs[g.Grantee]; !ok {
		return invalidf("grant %s: unknown grantee %q", g.ID, g.Grantee)
	}
	if g.Grantor == g.Grantee {
		return invalidf("grant %s: grantor and grantee are the same org; use a rule", g.ID)
	}
	if err := ix.checkSelector(g.Grantee, g.Src); err != nil {
		return invalidf("grant %s src: %v", g.ID, err)
	}
	d, ok := ix.devices[g.Dst]
	if !ok || d.Org != g.Grantor {
		return invalidf("grant %s: destination %q is not a device of %s", g.ID, g.Dst, g.Grantor)
	}
	if d.User != "" {
		return invalidf("grant %s: destination %s is user-owned; grants target tagged devices only", g.ID, g.Dst)
	}
	if err := checkPorts(g.Ports, false); err != nil {
		return invalidf("grant %s: %v", g.ID, err)
	}
	if g.ExpiresAt.IsZero() || !g.ExpiresAt.After(g.StartsAt) {
		return invalidf("grant %s: expires_at is required and must be after starts_at", g.ID)
	}
	return nil
}

func (ix *index) checkPlatformRule(pr PlatformRule) error {
	p, ok := ix.orgs[pr.Platform]
	if !ok || !p.Platform {
		return invalidf("platform rule %s: %q is not a platform org", pr.ID, pr.Platform)
	}
	for _, side := range []PlatformSelector{pr.Src, pr.Dst} {
		if side.Scope != ScopePlatform && side.Scope != ScopeChildren {
			return invalidf("platform rule %s: unknown scope %d", pr.ID, side.Scope)
		}
		if side.Role != "" && !rolePattern.MatchString(side.Role) {
			return invalidf("platform rule %s: role %q", pr.ID, side.Role)
		}
	}
	if (pr.Src.Scope == ScopeChildren) == (pr.Dst.Scope == ScopeChildren) {
		return invalidf("platform rule %s: exactly one side must be the platform's children", pr.ID)
	}
	if err := checkPorts(pr.Ports, false); err != nil {
		return invalidf("platform rule %s: %v", pr.ID, err)
	}
	return nil
}

// ValidateRole reports whether r is a valid role name (it becomes part of a Headscale tag).
func ValidateRole(r string) error {
	if !rolePattern.MatchString(r) {
		return fmt.Errorf("role %q: want lowercase letters, digits and dashes, starting with a letter, at most 31 characters", r)
	}
	return nil
}
