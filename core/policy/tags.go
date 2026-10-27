package policy

import "strings"

// Tag prefixes. Every Headmesh-managed tag starts with "tag:hm-", which lets Verify recognise (and refuse) any
// foreign principal in a compiled document.
const (
	tagPrefix     = "tag:hm-"
	orgTagPrefix  = "tag:hm-o-"
	roleTagPrefix = "tag:hm-r-"
	nodeTagPrefix = "tag:hm-n-"

	// QuarantineTag is applied to nodes that registered outside Headmesh in an enforced cell. No rule ever
	// references it, so quarantined nodes can reach nothing and nothing can reach them.
	QuarantineTag = "tag:hm-quarantine"
)

// OrgTag is carried by every tagged device of an org.
func OrgTag(org string) string { return orgTagPrefix + org }

// RoleTag is carried by every tagged device of org with role.
func RoleTag(org, role string) string { return roleTagPrefix + org + "-" + role }

// DeviceTag is unique to one device.
func DeviceTag(device string) string { return nodeTagPrefix + device }

// DeviceTags returns the full tag set Headmesh assigns to a tagged device, sorted. Quarantined devices get only
// QuarantineTag; removed devices get none.
func DeviceTags(d Device) []string {
	switch d.State {
	case DeviceRemoved:
		return nil
	case DeviceQuarantined:
		return []string{QuarantineTag}
	case DeviceActive:
	}
	if d.User != "" {
		return nil
	}
	tags := []string{DeviceTag(d.Short), OrgTag(d.Org)}
	for _, r := range d.Roles {
		tags = append(tags, RoleTag(d.Org, r))
	}
	return sortedUnique(tags)
}

// userPrincipal is how Headscale policies name a user's personal nodes.
func userPrincipal(u string) string { return u + "@" }

// tagOrg extracts the org short from an org, role or device tag. Device tags need the index to resolve.
func (ix *index) principalOrg(p string) (string, bool) {
	switch {
	case strings.HasPrefix(p, orgTagPrefix):
		org := strings.TrimPrefix(p, orgTagPrefix)
		_, ok := ix.orgs[org]
		return org, ok
	case strings.HasPrefix(p, roleTagPrefix):
		rest := strings.TrimPrefix(p, roleTagPrefix)
		if len(rest) < 12 || rest[10] != '-' {
			return "", false
		}
		org := rest[:10]
		_, ok := ix.orgs[org]
		return org, ok
	case strings.HasPrefix(p, nodeTagPrefix):
		d, ok := ix.devices[strings.TrimPrefix(p, nodeTagPrefix)]
		return d.Org, ok
	case strings.HasSuffix(p, "@"):
		org, ok := ix.users[strings.TrimSuffix(p, "@")]
		return org, ok
	default:
		return "", false
	}
}
