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
	v := &verifier{ix: ix, s: s, at: at, doc: doc, active: map[grantKey][]Grant{}}
	for _, g := range s.Grants {
		if g.ActiveAt(at) {
			k := grantKey{grantee: g.Grantee, grantor: g.Grantor, dst: DeviceTag(g.Dst)}
			v.active[k] = append(v.active[k], g)
		}
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
	active map[grantKey][]Grant
	errs   []error
}

type grantKey struct{ grantee, grantor, dst string }

func (v *verifier) fail(format string, args ...any) {
	v.errs = append(v.errs, violationf(format, args...))
}

func (v *verifier) checkTagOwners() {
	for t := range v.doc.TagOwners {
		if !strings.HasPrefix(t, tagPrefix) {
			v.fail("tagOwners declares foreign tag %q", t)
			continue
		}
		if t == QuarantineTag {
			v.fail("tagOwners declares %s; quarantined nodes must never be referenced", t)
			continue
		}
		if _, ok := v.ix.principalOrg(t); !ok {
			v.fail("tagOwners declares unknown tag %q", t)
		}
	}
}

// principal validates p and returns its org.
func (v *verifier) principal(where, p string) (string, bool) {
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
	where := fmt.Sprintf("grants[%d]", i)
	if len(g.Src) == 0 || len(g.Dst) == 0 || len(g.IP) == 0 {
		v.fail("%s: src, dst and ip must all be non-empty", where)
		return
	}
	ranges := make([]portRange, 0, len(g.IP))
	for _, raw := range g.IP {
		pr, err := Port(raw).parsed()
		if err != nil {
			v.fail("%s: %v", where, err)
			return
		}
		ranges = append(ranges, pr)
	}
	dstOrgs := make([]string, len(g.Dst))
	dstOK := make([]bool, len(g.Dst))
	for j, dp := range g.Dst {
		dstOrgs[j], dstOK[j] = v.principal(where+".dst", dp)
	}
	for _, sp := range g.Src {
		so, ok := v.principal(where+".src", sp)
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
	for _, g := range v.active[grantKey{grantee: so, grantor: do, dst: dp}] {
		if slices.Contains(v.selectorNames(g.Grantee, g.Src), sp) && portsCover(g.Ports, want) {
			return true
		}
	}
	for _, pr := range v.s.PlatformRules {
		if v.sideMatches(pr.Platform, pr.Src, so, sp) && v.sideMatches(pr.Platform, pr.Dst, do, dp) &&
			portsCover(pr.Ports, want) {
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
		return slices.Contains(v.selectorNames(org, All()), principal)
	}
	return principal == RoleTag(org, sel.Role)
}

// selectorNames is the full set of principal names a selector may legitimately be rendered as.
func (v *verifier) selectorNames(org string, sel Selector) []string {
	switch sel.Kind {
	case SelectAll:
		out := []string{OrgTag(org)}
		for _, u := range v.ix.orgs[org].Users {
			out = append(out, userPrincipal(u))
		}
		return out
	case SelectUsers:
		var out []string
		for _, u := range v.ix.orgs[org].Users {
			out = append(out, userPrincipal(u))
		}
		return out
	case SelectRole:
		return []string{RoleTag(org, sel.Role)}
	case SelectDevice:
		return []string{DeviceTag(sel.Device)}
	default:
		return nil
	}
}

func portsCover(ports []Port, want portRange) bool {
	for _, p := range ports {
		pr, err := p.parsed()
		if err == nil && pr.covers(want) {
			return true
		}
	}
	return false
}

func (v *verifier) checkSSH(i int, r SSHRule) {
	where := fmt.Sprintf("ssh[%d]", i)
	if r.Action != "accept" {
		v.fail("%s: action %q; Headmesh only emits accept", where, r.Action)
	}
	if slices.Contains(r.Users, "root") {
		v.fail("%s: root logins are never emitted", where)
	}
	orgs := map[string]bool{}
	for _, p := range append(slices.Clone(r.Src), r.Dst...) {
		if org, ok := v.principal(where, p); ok {
			orgs[org] = true
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
	where := fmt.Sprintf("tests[%d]", i)
	v.principal(where+".src", t.Src)
	for _, e := range append(slices.Clone(t.Accept), t.Deny...) {
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
