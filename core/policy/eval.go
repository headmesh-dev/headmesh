package policy

import "slices"

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

func intersects(a, b []string) bool {
	for _, x := range a {
		if slices.Contains(b, x) {
			return true
		}
	}
	return false
}
