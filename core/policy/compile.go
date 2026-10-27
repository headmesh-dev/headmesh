package policy

import (
	"fmt"
	"hash/fnv"
	"slices"
	"strings"
	"time"
)

// Options tune compilation. The zero value is valid and production-safe.
type Options struct {
	// TagOwners is the owner list emitted for every Headmesh tag. Headmesh assigns tags through the Headscale API,
	// so no user needs to own them; the default (empty) means "only the API may assign".
	TagOwners []string
	// Canaries is the number of other orgs each org's deny test samples. Zero means the default (5); negative
	// disables policy tests entirely.
	Canaries int
	// CanaryPort is the port the canary tests probe. Defaults to tcp 22.
	CanaryPort int
}

const defaultCanaries = 5

// Compile renders the policy for snapshot s as it should be enforced at instant at. It validates s, builds the
// document, then runs Verify on its own output before returning, so a compiler bug can never produce a policy
// that crosses tenants.
func Compile(s Snapshot, at time.Time, opt Options) (*Compiled, error) {
	ix, err := s.index()
	if err != nil {
		return nil, err
	}
	b := &builder{ix: ix, s: s, at: at, opt: opt, live: newLiveIndex(s), tags: map[string]bool{}}
	doc := b.build()
	bytes, body, sum, err := render(s.Cell, doc)
	if err != nil {
		return nil, err
	}
	// Re-parse the rendered JSON (the bytes differ only by a leading comment header) so Verify checks exactly
	// what a cell would receive.
	parsed, err := parseJSON(body)
	if err != nil {
		return nil, fmt.Errorf("compiler produced an unparseable policy: %w", err)
	}
	if err := verifyIndexed(ix, s, at, parsed); err != nil {
		return nil, fmt.Errorf("compiler produced a policy that fails verification: %w", err)
	}
	return &Compiled{
		HuJSON:   bytes,
		SHA256:   sum,
		Document: parsed,
		Stats: Stats{
			Orgs: len(s.Orgs), Devices: len(s.Devices), Rules: len(s.Rules), Grants: len(s.Grants),
			PlatformRules: len(s.PlatformRules), Tags: len(parsed.TagOwners), Tests: len(parsed.Tests),
		},
	}, nil
}

// liveIndex records which principals currently match at least one active device, so the compiler only emits
// tags that exist and never scans the device list per selector.
type liveIndex struct {
	tagged map[string]int             // org -> number of active tagged devices
	roles  map[string]map[string]bool // org -> role -> some active tagged device has it
	users  map[string][]string        // org -> Headscale users with an active personal node (org's order)
}

func newLiveIndex(s Snapshot) *liveIndex {
	li := &liveIndex{tagged: map[string]int{}, roles: map[string]map[string]bool{}, users: map[string][]string{}}
	liveUser := map[string]bool{}
	for _, d := range s.Devices {
		if d.State != DeviceActive {
			continue
		}
		if d.User != "" {
			liveUser[d.User] = true
			continue
		}
		li.tagged[d.Org]++
		if li.roles[d.Org] == nil {
			li.roles[d.Org] = map[string]bool{}
		}
		for _, r := range d.Roles {
			li.roles[d.Org][r] = true
		}
	}
	for _, o := range s.Orgs {
		for _, u := range o.Users {
			if liveUser[u] {
				li.users[o.Short] = append(li.users[o.Short], u)
			}
		}
	}
	return li
}

type builder struct {
	ix   *index
	s    Snapshot
	at   time.Time
	opt  Options
	live *liveIndex
	doc  Document
	tags map[string]bool
}

func (b *builder) build() Document {
	b.doc = Document{TagOwners: map[string][]string{}, Grants: []GrantRule{}}
	for _, o := range b.s.Orgs {
		if o.Template == TemplateTeam {
			all := b.orgPrincipals(o.Short)
			b.addGrant(all, all, []Port{PortAny})
		}
		if o.SSH {
			dst := b.taggedOrgPrincipal(o.Short)
			if src := b.orgPrincipals(o.Short); len(src) > 0 && len(dst) > 0 {
				b.doc.SSH = append(b.doc.SSH, SSHRule{
					Action: "accept", Src: src, Dst: dst, Users: []string{"autogroup:nonroot"},
				})
			}
		}
	}
	for _, r := range b.s.Rules {
		if !r.ActiveAt(b.at) {
			continue
		}
		b.addGrant(b.resolve(r.Org, r.Src), b.resolve(r.Org, r.Dst), r.Ports)
	}
	for _, g := range b.s.Grants {
		if !g.ActiveAt(b.at) {
			continue
		}
		b.addGrant(b.resolve(g.Grantee, g.Src), b.resolve(g.Grantor, DeviceSel(g.Dst)), g.Ports)
	}
	for _, pr := range b.s.PlatformRules {
		b.addGrant(b.resolvePlatform(pr.Platform, pr.Src), b.resolvePlatform(pr.Platform, pr.Dst), pr.Ports)
	}
	b.addTests()
	b.collectTags()
	owners := b.opt.TagOwners
	if owners == nil {
		owners = []string{}
	}
	for t := range b.tags {
		b.doc.TagOwners[t] = slices.Clone(owners)
	}
	return b.doc
}

func (b *builder) addGrant(src, dst []string, ports []Port) {
	if len(src) == 0 || len(dst) == 0 {
		return
	}
	ip := make([]string, len(ports))
	for i, p := range ports {
		ip[i] = string(p)
	}
	b.doc.Grants = append(b.doc.Grants, GrantRule{Src: slices.Clone(src), Dst: slices.Clone(dst), IP: ip})
}

// collectTags declares every tag the finished document references — in grants, SSH rules and tests — because
// Headscale rejects policies that name undeclared tags.
func (b *builder) collectTags() {
	note := func(ps []string) {
		for _, p := range ps {
			if strings.HasPrefix(p, "tag:") {
				b.tags[p] = true
			}
		}
	}
	for _, g := range b.doc.Grants {
		note(g.Src)
		note(g.Dst)
	}
	for _, r := range b.doc.SSH {
		note(r.Src)
		note(r.Dst)
	}
	for _, t := range b.doc.Tests {
		note([]string{t.Src})
		for _, e := range append(slices.Clone(t.Accept), t.Deny...) {
			if i := strings.LastIndex(e, ":"); i > 0 {
				note([]string{e[:i]})
			}
		}
	}
}

func (b *builder) taggedOrgPrincipal(org string) []string {
	if b.live.tagged[org] > 0 {
		return []string{OrgTag(org)}
	}
	return nil
}

// orgPrincipals returns principals covering every live device of org: its org tag (if any tagged device is
// live) and the user principal of each Headscale user with a live personal node.
func (b *builder) orgPrincipals(org string) []string {
	out := b.taggedOrgPrincipal(org)
	for _, u := range b.live.users[org] {
		out = append(out, userPrincipal(u))
	}
	return out
}

// resolve turns an intra-org selector into principals, dropping principals that match no live device so the
// emitted policy only references tags that exist.
func (b *builder) resolve(org string, sel Selector) []string {
	switch sel.Kind {
	case SelectAll:
		return b.orgPrincipals(org)
	case SelectUsers:
		out := make([]string, 0, len(b.live.users[org]))
		for _, u := range b.live.users[org] {
			out = append(out, userPrincipal(u))
		}
		return out
	case SelectRole:
		if b.live.roles[org][sel.Role] {
			return []string{RoleTag(org, sel.Role)}
		}
		return nil
	case SelectDevice:
		d := b.ix.devices[sel.Device]
		if d.State == DeviceActive && d.User == "" {
			return []string{DeviceTag(d.Short)}
		}
		return nil
	default:
		return nil
	}
}

func (b *builder) resolvePlatform(platform string, sel PlatformSelector) []string {
	orgs := []string{platform}
	if sel.Scope == ScopeChildren {
		orgs = b.ix.children[platform]
	}
	var out []string
	for _, o := range orgs {
		if sel.Role == "" {
			out = append(out, b.orgPrincipals(o)...)
		} else {
			out = append(out, b.resolve(o, Role(sel.Role))...)
		}
	}
	return out
}

// addTests emits isolation canaries: for each org with live tagged devices, a deny test against a
// deterministic sample of unrelated orgs, and (for team orgs) an accept test against itself. Headscale refuses
// to load a policy whose tests fail, so the control server enforces isolation a second time.
//
// The sample walks the sorted list of live orgs from a per-org starting offset with a per-org stride, both
// derived from a hash of the cell and org. That keeps the choice stable and spread out while costing O(k) per
// org instead of hashing every pair.
func (b *builder) addTests() {
	k := b.opt.Canaries
	if k < 0 {
		return
	}
	if k == 0 {
		k = defaultCanaries
	}
	port := b.opt.CanaryPort
	if port == 0 {
		port = 22
	}
	related := b.relatedOrgs()
	var live []string
	for _, o := range b.s.Orgs {
		if b.live.tagged[o.Short] > 0 {
			live = append(live, o.Short)
		}
	}
	slices.Sort(live)
	n := len(live)
	for _, org := range live {
		t := PolicyTest{Src: OrgTag(org)}
		if n > 1 {
			h := fnv.New64a()
			_, _ = h.Write([]byte(b.s.Cell + "|" + org))
			sum := h.Sum64()
			start := int(sum % uint64(n))                        //nolint:gosec // result is < n, which is an int
			stride := coprimeStride(n, int((sum>>32)%uint64(n))) //nolint:gosec // result is < n, which is an int
			for i, picked := 0, 0; i < n && picked < k; i++ {
				other := live[(start+i*stride)%n]
				if other == org || related[org][other] {
					continue
				}
				t.Deny = append(t.Deny, fmt.Sprintf("%s:%d", OrgTag(other), port))
				picked++
			}
		}
		if b.ix.orgs[org].Template == TemplateTeam && b.live.tagged[org] >= 2 {
			t.Accept = []string{fmt.Sprintf("%s:%d", OrgTag(org), port)}
		}
		if len(t.Deny) > 0 || len(t.Accept) > 0 {
			b.doc.Tests = append(b.doc.Tests, t)
		}
	}
}

// coprimeStride returns a stride in [1, n) coprime with n (so the walk visits every org), starting from seed.
func coprimeStride(n, seed int) int {
	if n <= 2 {
		return 1
	}
	for s := seed; s < seed+n; s++ {
		c := s%(n-1) + 1
		if gcd(c, n) == 1 {
			return c
		}
	}
	return 1
}

func gcd(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// relatedOrgs marks org pairs that may legitimately reach each other (in either direction) at b.at, so canary
// deny tests never contradict a real grant or platform rule.
func (b *builder) relatedOrgs() map[string]map[string]bool {
	rel := map[string]map[string]bool{}
	mark := func(x, y string) {
		for _, p := range [][2]string{{x, y}, {y, x}} {
			if rel[p[0]] == nil {
				rel[p[0]] = map[string]bool{}
			}
			rel[p[0]][p[1]] = true
		}
	}
	for _, g := range b.s.Grants {
		if g.ActiveAt(b.at) {
			mark(g.Grantor, g.Grantee)
		}
	}
	seen := map[string]bool{}
	for _, pr := range b.s.PlatformRules {
		if seen[pr.Platform] {
			continue
		}
		seen[pr.Platform] = true
		for _, c := range b.ix.children[pr.Platform] {
			mark(pr.Platform, c)
		}
	}
	return rel
}
