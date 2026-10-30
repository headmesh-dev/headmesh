package policy

import (
	"cmp"
	"slices"
)

// Principals returns the policy principals a live device is known by. Inactive devices have none.
func Principals(d Device) []string {
	if d.State != DeviceActive {
		return nil
	}
	if d.User != "" {
		return []string{userPrincipal(d.User)}
	}
	return DeviceTags(d)
}

// Allows reports whether doc lets src open a connection to dst on proto/port. It implements exactly the subset
// of Tailscale grant semantics Headmesh emits — tag and user principals with explicit or "*" ports — and is
// used by the access-explain API and by property tests that check compiled policies against the model.
func (doc Document) Allows(src, dst Device, proto string, port int) bool {
	return doc.AllowsPrincipals(Principals(src), Principals(dst), proto, port)
}

// AllowsPrincipals reports whether a node known by principals sp may reach a node known by principals dp on
// proto/port. Cell agents use it to check local reachability invariants without a device model.
func (doc Document) AllowsPrincipals(sp, dp []string, proto string, port int) bool {
	if len(sp) == 0 || len(dp) == 0 {
		return false
	}
	for _, g := range doc.Grants {
		if !intersects(g.Src, sp) || !intersects(g.Dst, dp) {
			continue
		}
		for _, raw := range g.IP {
			if pr, err := Port(raw).parsed(); err == nil && pr.matches(proto, port) {
				return true
			}
		}
	}
	return false
}

// AllowsPort reports whether doc lets src reach dst on every connection port spec p names: one port, a range, or "*".
// It counts the union of matching grants, so tcp:8000-8100 is allowed when one grant opens 8000-8049 and another
// 8050-8100. "*" is allowed only by a matching "*" grant, since it covers protocols Headmesh never lists.
func (doc Document) AllowsPort(src, dst Device, p Port) bool {
	want, err := p.parsed()
	if err != nil {
		return false
	}
	sp, dp := Principals(src), Principals(dst)
	if len(sp) == 0 || len(dp) == 0 {
		return false
	}
	var spans []portRange
	for _, g := range doc.Grants {
		if !intersects(g.Src, sp) || !intersects(g.Dst, dp) {
			continue
		}
		for _, raw := range g.IP {
			have, err := Port(raw).parsed()
			if err != nil {
				continue
			}
			if have.covers(want) {
				return true
			}
			if want.proto != "" && have.proto == want.proto {
				spans = append(spans, have)
			}
		}
	}
	// No single grant covers it: sweep the union of the matching spans across [lo, hi].
	slices.SortFunc(spans, func(a, b portRange) int { return cmp.Compare(a.lo, b.lo) })
	next := want.lo
	for _, r := range spans {
		if r.lo > next {
			break
		}
		if r.hi >= next {
			next = r.hi + 1
		}
		if next > want.hi {
			return true
		}
	}
	return false
}

func intersects(a, b []string) bool {
	for _, x := range a {
		if slices.Contains(b, x) {
			return true
		}
	}
	return false
}
