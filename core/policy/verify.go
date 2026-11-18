package policy

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ErrViolation wraps every isolation or well-formedness violation Verify finds.
var ErrViolation = errors.New("policy violation")

func violationf(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrViolation}, args...)...)
}

// Verify checks a policy document against the snapshot it should have been compiled from:
//
//   - only Headmesh principals of known orgs and devices appear (no "*", autogroups, groups, hosts or IPs);
//   - every referenced tag is declared in tagOwners and nothing else is;
//   - every cross-org edge is justified, port by port, by a grant active at instant at or by a platform rule;
//   - "*" ports never cross orgs, SSH rules never cross orgs, and tests only name known principals.
//
// Verify re-derives justification from the snapshot rather than from the compiler's intermediate state, so it
// catches compiler bugs as well as hand-edited documents.
func Verify(s Snapshot, at time.Time, doc Document) error {
	ix, err := s.index()
	if err != nil {
		return err
	}
	return verifyIndexed(ix, s, at, doc)
}

func verifyIndexed(ix *index, s Snapshot, at time.Time, doc Document) error {
	v := &verifier{ix: ix, s: s, at: at, doc: doc, active: map[grantKey][]activeGrant{}}
	for _, g := range s.Grants {
		if g.ActiveAt(at) {
			k := grantKey{grantee: g.Grantee, grantor: g.Grantor, dst: g.Dst}
			v.active[k] = append(v.active[k], activeGrant{grant: g, ports: parsePorts(g.Ports)})
		}
	}
	v.platformPorts = make([][]portRange, len(s.PlatformRules))
	for i, pr := range s.PlatformRules {
		v.platformPorts[i] = parsePorts(pr.Ports)
	}
	v.checkTagOwners()
	for i, g := range doc.Grants {
		v.checkGrantRule(i, g)
	}
	for i, r := range doc.SSH {
		v.checkSSH(i, r)
	}
	for i, t := range doc.Tests {
		v.checkTest(i, t)
	}
	return errors.Join(v.errs...)
}

type verifier struct {
	ix     *index
	s      Snapshot
	at     time.Time
	doc    Document
	active map[grantKey][]activeGrant
	// platformPorts holds the parsed ports of each of s.PlatformRules, by index.
	platformPorts [][]portRange
	errs          []error
	// Scratch space reused across grant rules.
	ranges  []portRange
	dstOrgs []string
	dstOK   []bool
}

// grantKey finds the active grants that could justify an edge; dst is the destination device's Short.
type grantKey struct{ grantee, grantor, dst string }

// activeGrant is a grant active at the verified instant, with its ports parsed once.
type activeGrant struct {
	grant Grant
	ports []portRange
}

// location is a place in the document, such as grants[3].dst. It is only formatted when a check fails.
type location struct {
	list  string
	i     int
	field string
}

func (l location) String() string { return l.list + "[" + strconv.Itoa(l.i) + "]" + l.field }

func (l location) with(field string) location {
	l.field = field
	return l
}

func (v *verifier) fail(format string, args ...any) {
	v.errs = append(v.errs, violationf(format, args...))
}

func (v *verifier) checkTagOwners() {
	// Headscale refuses keys and nodes whose tags the live policy does not declare.
	for _, d := range v.s.Devices {
		if d.State == DeviceRemoved {
			continue
		}
		for _, t := range DeviceTags(d) {
			if _, ok := v.doc.TagOwners[t]; !ok {
				v.fail("tag %q of live device %s is not declared in tagOwners", t, d.Short)
			}
		}
	}
	for t := range v.doc.TagOwners {
		if !strings.HasPrefix(t, tagPrefix) {
			v.fail("tagOwners declares foreign tag %q", t)
			continue
		}
		if t == QuarantineTag {
			continue // declared so quarantined nodes keep a valid tag; principal() forbids it in any rule
		}
		if _, ok := v.ix.principalOrg(t); !ok {
			v.fail("tagOwners declares unknown tag %q", t)
		}
	}
}

// principal validates p and returns its org.
func (v *verifier) principal(where location, p string) (string, bool) {
	if strings.HasPrefix(p, "tag:") {
		if _, declared := v.doc.TagOwners[p]; !declared {
			v.fail("%s: tag %q is not declared in tagOwners", where, p)
		}
	}
	org, ok := v.ix.principalOrg(p)
	if !ok {
		v.fail("%s: principal %q is not a known Headmesh org, role, device or user", where, p)
	}
	return org, ok
}

func (v *verifier) checkGrantRule(i int, g GrantRule) {
	where := location{list: "grants", i: i}
	if len(g.Src) == 0 || len(g.Dst) == 0 || len(g.IP) == 0 {
		v.fail("%s: src, dst and ip must all be non-empty", where)
		return
	}
	ranges := v.ranges[:0]
	for _, raw := range g.IP {
		pr, err := Port(raw).parsed()
		if err != nil {
			v.fail("%s: %v", where, err)
			return
		}
		ranges = append(ranges, pr)
	}
	v.ranges = ranges
	dstOrgs, dstOK := v.dstOrgs[:0], v.dstOK[:0]
	for _, dp := range g.Dst {
		org, ok := v.principal(where.with(".dst"), dp)
		dstOrgs, dstOK = append(dstOrgs, org), append(dstOK, ok)
	}
	v.dstOrgs, v.dstOK = dstOrgs, dstOK
	for _, sp := range g.Src {
		so, ok := v.principal(where.with(".src"), sp)
		if !ok {
			continue
		}
		for di, dp := range g.Dst {
			do := dstOrgs[di]
			if !dstOK[di] || so == do {
				continue
			}
			for j, pr := range ranges {
				if g.IP[j] == string(PortAny) {
					v.fail("%s: %q → %q crosses orgs with \"*\" ports", where, sp, dp)
					continue
				}
				if !v.justified(so, do, sp, dp, pr) {
					v.fail("%s: %q → %q on %s crosses from org %s to org %s without an active grant or platform rule",
						where, sp, dp, g.IP[j], so, do)
				}
			}
		}
	}
}

func (v *verifier) justified(so, do, sp, dp string, want portRange) bool {
	// Grants always target one device, so only a device tag destination can be covered by one.
	if dst, ok := strings.CutPrefix(dp, nodeTagPrefix); ok {
		for _, ag := range v.active[grantKey{grantee: so, grantor: do, dst: dst}] {
			if v.selects(ag.grant.Grantee, ag.grant.Src, sp) && rangesCover(ag.ports, want) {
				return true
			}
		}
	}
	for i, pr := range v.s.PlatformRules {
		if v.sideMatches(pr.Platform, pr.Src, so, sp) && v.sideMatches(pr.Platform, pr.Dst, do, dp) &&
			rangesCover(v.platformPorts[i], want) {
			return true
		}
	}
	return false
}

func (v *verifier) sideMatches(platform string, sel PlatformSelector, org, principal string) bool {
	switch sel.Scope {
	case ScopePlatform:
		if org != platform {
			return false
		}
	case ScopeChildren:
		if !v.ix.childSet[platform][org] {
			return false
		}
	default:
		return false
	}
	if sel.Role == "" {
		return v.selects(org, All(), principal)
	}
	return isRoleTag(principal, org, sel.Role)
}

// selects reports whether p is one of the principal names sel may legitimately be rendered as for org: the
// org tag or a user principal for All, a user principal for Users, the role tag for Role and the device tag
// for Device. It compares in place rather than building each name.
func (v *verifier) selects(org string, sel Selector, p string) bool {
	switch sel.Kind {
	case SelectAll:
		return hasName(p, orgTagPrefix, org) || v.isUserOf(org, p)
	case SelectUsers:
		return v.isUserOf(org, p)
	case SelectRole:
		return isRoleTag(p, org, sel.Role)
	case SelectDevice:
		return hasName(p, nodeTagPrefix, sel.Device)
	default:
		return false
	}
}

// isUserOf reports whether p is the user principal of one of org's Headscale users.
func (v *verifier) isUserOf(org, p string) bool {
	u, ok := strings.CutSuffix(p, "@")
	return ok && slices.Contains(v.ix.orgs[org].Users, u)
}

// hasName reports whether p == prefix + name.
func hasName(p, prefix, name string) bool {
	rest, ok := strings.CutPrefix(p, prefix)
	return ok && rest == name
}

// isRoleTag reports whether p == RoleTag(org, role).
func isRoleTag(p, org, role string) bool {
	rest, ok := strings.CutPrefix(p, roleTagPrefix)
	if !ok {
		return false
	}
	if rest, ok = strings.CutPrefix(rest, org); !ok {
		return false
	}
	return hasName(rest, "-", role)
}

// parsePorts parses ports, leaving out any that don't parse; they can't cover anything.
func parsePorts(ports []Port) []portRange {
	out := make([]portRange, 0, len(ports))
	for _, p := range ports {
		if pr, err := p.parsed(); err == nil {
			out = append(out, pr)
		}
	}
	return out
}

func rangesCover(ranges []portRange, want portRange) bool {
	for _, r := range ranges {
		if r.covers(want) {
			return true
		}
	}
	return false
}

func (v *verifier) checkSSH(i int, r SSHRule) {
	where := location{list: "ssh", i: i}
	if r.Action != "accept" {
		v.fail("%s: action %q; Headmesh only emits accept", where, r.Action)
	}
	if slices.Contains(r.Users, "root") {
		v.fail("%s: root logins are never emitted", where)
	}
	orgs := map[string]bool{}
	for _, side := range [][]string{r.Src, r.Dst} {
		for _, p := range side {
			if org, ok := v.principal(where, p); ok {
				orgs[org] = true
			}
		}
	}
	for _, p := range r.Dst {
		if !strings.HasPrefix(p, "tag:") {
			v.fail("%s: destination %q must be a tag", where, p)
		}
	}
	if len(orgs) > 1 {
		v.fail("%s: SSH rule spans %d orgs", where, len(orgs))
	}
}

func (v *verifier) checkTest(i int, t PolicyTest) {
	where := location{list: "tests", i: i}
	v.principal(where.with(".src"), t.Src)
	for _, entries := range [][]string{t.Accept, t.Deny} {
		for _, e := range entries {
			// Tags contain a colon themselves ("tag:hm-o-x:22"), so the port follows the last colon.
			idx := strings.LastIndex(e, ":")
			if idx <= 0 {
				v.fail("%s: entry %q has no port", where, e)
				continue
			}
			p, port := e[:idx], e[idx+1:]
			if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
				v.fail("%s: entry %q has an invalid port", where, e)
			}
			v.principal(where, p)
		}
	}
}
