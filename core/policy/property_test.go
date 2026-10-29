package policy

import (
	"errors"
	"slices"
	"testing"
	"time"

	"pgregory.net/rapid"
)

var (
	roleChoices  = []string{"gpu", "db", "web"}
	portChoices  = []string{"*", "tcp:22", "tcp:80", "udp:53", "tcp:8000-8100"}
	grantChoices = []string{"tcp:22", "tcp:80", "udp:53", "tcp:8000-8100"}
	probes       = []struct {
		proto string
		port  int
	}{{"tcp", 22}, {"tcp", 80}, {"udp", 53}, {"tcp", 8050}, {"tcp", 443}, {"sctp", 1}}
)

// genSnapshot draws a random but valid cell: a few orgs (optionally one platform with children), devices with
// random roles and states, intra-org rules, cross-org grants and platform rules, all around instant t0.
func genSnapshot(t *rapid.T) Snapshot {
	nOrgs := rapid.IntRange(1, 5).Draw(t, "orgs")
	s := Snapshot{Cell: "prop"}
	hasPlatform := rapid.Bool().Draw(t, "platform")
	for i := range nOrgs {
		o := Org{
			Short:    sid('o', i),
			Template: rapid.SampledFrom([]Template{TemplateTeam, TemplateLocked}).Draw(t, "template"),
			SSH:      rapid.Bool().Draw(t, "ssh"),
		}
		if hasPlatform && i == 0 {
			o.Platform = true
		} else if hasPlatform && rapid.Bool().Draw(t, "child") {
			o.Parent = sid('o', 0)
		}
		if rapid.IntRange(0, 3).Draw(t, "hasUser") == 0 {
			o.Users = []string{"u" + string(base32Alphabet[i])}
		}
		s.Orgs = append(s.Orgs, o)
	}
	dev := 0
	tagged := map[string][]string{} // org -> tagged device shorts
	for _, o := range s.Orgs {
		for range rapid.IntRange(0, 4).Draw(t, "devices") {
			d := Device{
				Short:      sid('d', dev),
				Org:        o.Short,
				State:      rapid.SampledFrom([]DeviceState{DeviceActive, DeviceActive, DeviceActive, DeviceQuarantined, DeviceRemoved}).Draw(t, "state"),
				Registered: rapid.IntRange(0, 3).Draw(t, "registered") > 0,
			}
			dev++
			if len(o.Users) > 0 && rapid.IntRange(0, 2).Draw(t, "userOwned") == 0 {
				d.User = o.Users[0]
			} else {
				for _, r := range roleChoices {
					if rapid.Bool().Draw(t, "role") {
						d.Roles = append(d.Roles, r)
					}
				}
				tagged[o.Short] = append(tagged[o.Short], d.Short)
			}
			s.Devices = append(s.Devices, d)
		}
	}
	selector := func(org, label string) Selector {
		switch rapid.IntRange(0, 3).Draw(t, label) {
		case 0:
			return All()
		case 3:
			for _, o := range s.Orgs {
				if o.Short == org && len(o.Users) > 0 {
					return Users()
				}
			}
			return All()
		case 1:
			return Role(rapid.SampledFrom(roleChoices).Draw(t, label+"Role"))
		default:
			if len(tagged[org]) == 0 {
				return All()
			}
			return DeviceSel(rapid.SampledFrom(tagged[org]).Draw(t, label+"Device"))
		}
	}
	drawPorts := func(choices []string, label string) []Port {
		n := rapid.IntRange(1, 2).Draw(t, label+"N")
		var out []Port
		for range n {
			out = append(out, Port(rapid.SampledFrom(choices).Draw(t, label)))
		}
		return out
	}
	window := func(label string) (time.Time, time.Time) {
		start := t0.Add(time.Duration(rapid.IntRange(-120, 120).Draw(t, label+"Start")) * time.Minute)
		return start, start.Add(time.Duration(rapid.IntRange(1, 180).Draw(t, label+"Dur")) * time.Minute)
	}
	for i, o := range s.Orgs {
		for j := range rapid.IntRange(0, 3).Draw(t, "rules") {
			r := Rule{
				ID: sid('r', i*8+j), Org: o.Short, Src: selector(o.Short, "rsrc"), Dst: selector(o.Short, "rdst"),
				Ports: drawPorts(portChoices, "rport"),
			}
			if rapid.Bool().Draw(t, "ruleTimed") {
				r.StartsAt, r.ExpiresAt = window("rule")
			}
			s.Rules = append(s.Rules, r)
		}
	}
	if len(s.Orgs) > 1 {
		for k := range rapid.IntRange(0, 4).Draw(t, "grants") {
			grantor := rapid.SampledFrom(s.Orgs).Draw(t, "grantor").Short
			grantee := rapid.SampledFrom(s.Orgs).Draw(t, "grantee").Short
			if grantor == grantee || len(tagged[grantor]) == 0 {
				continue
			}
			start, end := window("grant")
			s.Grants = append(s.Grants, Grant{
				ID: sid('g', k), Grantor: grantor, Grantee: grantee, Src: selector(grantee, "gsrc"),
				Dst: rapid.SampledFrom(tagged[grantor]).Draw(t, "gdst"), Ports: drawPorts(grantChoices, "gport"),
				StartsAt: start, ExpiresAt: end, Revoked: rapid.IntRange(0, 4).Draw(t, "revoked") == 0,
			})
		}
	}
	if hasPlatform {
		for k := range rapid.IntRange(0, 2).Draw(t, "platformRules") {
			toChildren := rapid.Bool().Draw(t, "toChildren")
			plat := PlatformSelector{Scope: ScopePlatform}
			kids := PlatformSelector{Scope: ScopeChildren}
			if rapid.Bool().Draw(t, "platRole") {
				plat.Role = rapid.SampledFrom(roleChoices).Draw(t, "platRoleName")
			}
			if rapid.Bool().Draw(t, "kidRole") {
				kids.Role = rapid.SampledFrom(roleChoices).Draw(t, "kidRoleName")
			}
			pr := PlatformRule{ID: sid('p', k), Platform: sid('o', 0), Ports: drawPorts(grantChoices, "pport")}
			if toChildren {
				pr.Src, pr.Dst = plat, kids
			} else {
				pr.Src, pr.Dst = kids, plat
			}
			s.PlatformRules = append(s.PlatformRules, pr)
		}
	}
	return s
}

// modelAllows is the reference semantics, derived only from the Snapshot. The compiled policy must agree with
// it exactly for every device pair and probe.
func modelAllows(s Snapshot, at time.Time, a, b Device, proto string, port int) bool {
	if a.State != DeviceActive || b.State != DeviceActive {
		return false
	}
	orgs := map[string]Org{}
	children := map[string][]string{}
	for _, o := range s.Orgs {
		orgs[o.Short] = o
		if o.Parent != "" {
			children[o.Parent] = append(children[o.Parent], o.Short)
		}
	}
	inSel := func(org string, sel Selector, d Device) bool {
		if d.Org != org {
			return false
		}
		switch sel.Kind {
		case SelectAll:
			return true
		case SelectUsers:
			return d.User != ""
		case SelectRole:
			return d.User == "" && slices.Contains(d.Roles, sel.Role)
		case SelectDevice:
			return d.User == "" && d.Short == sel.Device
		default:
			return false
		}
	}
	inSide := func(platform string, side PlatformSelector, d Device) bool {
		if side.Scope == ScopePlatform && d.Org != platform {
			return false
		}
		if side.Scope == ScopeChildren && !slices.Contains(children[platform], d.Org) {
			return false
		}
		return side.Role == "" || (d.User == "" && slices.Contains(d.Roles, side.Role))
	}
	match := func(ps []Port) bool {
		for _, p := range ps {
			if pr, err := p.parsed(); err == nil && pr.matches(proto, port) {
				return true
			}
		}
		return false
	}
	if a.Org == b.Org {
		if orgs[a.Org].Template == TemplateTeam {
			return true
		}
		for _, r := range s.Rules {
			if r.Org == a.Org && r.ActiveAt(at) && inSel(r.Org, r.Src, a) && inSel(r.Org, r.Dst, b) && match(r.Ports) {
				return true
			}
		}
	}
	for _, g := range s.Grants {
		if g.ActiveAt(at) && a.Org == g.Grantee && b.Org == g.Grantor && b.Short == g.Dst && b.User == "" &&
			inSel(g.Grantee, g.Src, a) && match(g.Ports) {
			return true
		}
	}
	for _, pr := range s.PlatformRules {
		if inSide(pr.Platform, pr.Src, a) && inSide(pr.Platform, pr.Dst, b) && match(pr.Ports) {
			return true
		}
	}
	return false
}

// TestCompiledPolicyMatchesModel is the central isolation property: for random cells, the compiled policy
// allows a connection if and only if the model does, Verify accepts it, and output is reproducible.
func TestCompiledPolicyMatchesModel(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := genSnapshot(t)
		at := t0.Add(time.Duration(rapid.IntRange(-60, 60).Draw(t, "at")) * time.Minute)
		c, err := Compile(s, at, Options{})
		if err != nil {
			t.Fatalf("Compile: %v", err)
		}
		for _, a := range s.Devices {
			for _, b := range s.Devices {
				if a.Short == b.Short {
					continue
				}
				for _, p := range probes {
					got := c.Document.Allows(a, b, p.proto, p.port)
					want := modelAllows(s, at, a, b, p.proto, p.port)
					if got != want {
						t.Fatalf("%s(%s) -> %s(%s) %s:%d: policy=%v model=%v\n%s",
							a.Short, a.Org, b.Short, b.Org, p.proto, p.port, got, want, c.HuJSON)
					}
				}
			}
		}
		again, err := Compile(s, at, Options{})
		if err != nil || again.SHA256 != c.SHA256 {
			t.Fatalf("recompiling the same snapshot changed the output (%v)", err)
		}
	})
}

// TestVerifyCatchesInjectedCrossTenantEdges checks that any unjustified cross-org edge added to a valid policy
// is rejected.
func TestVerifyCatchesInjectedCrossTenantEdges(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := genSnapshot(t)
		c, err := Compile(s, t0, Options{})
		if err != nil {
			t.Fatalf("Compile: %v", err)
		}
		var live []Device
		for _, d := range s.Devices {
			if d.State == DeviceActive && d.User == "" {
				live = append(live, d)
			}
		}
		if len(live) < 2 {
			return
		}
		a := rapid.SampledFrom(live).Draw(t, "a")
		b := rapid.SampledFrom(live).Draw(t, "b")
		port := rapid.SampledFrom([]string{"tcp:3389", "udp:4000"}).Draw(t, "port")
		if a.Org == b.Org {
			return
		}
		doc := c.Document
		doc.TagOwners[DeviceTag(a.Short)] = []string{}
		doc.TagOwners[DeviceTag(b.Short)] = []string{}
		doc.Grants = append(doc.Grants, GrantRule{Src: []string{DeviceTag(a.Short)}, Dst: []string{DeviceTag(b.Short)}, IP: []string{port}})
		if err := Verify(s, t0, doc); !errors.Is(err, ErrViolation) {
			t.Fatalf("injected edge %s -> %s %s was not rejected: %v", a.Short, b.Short, port, err)
		}
	})
}

func TestTimelineRevisionsMatchPointInTimeCompiles(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := genSnapshot(t)
		revs, err := Timeline(s, t0, 6*time.Hour, 64, Options{})
		if err != nil {
			t.Fatalf("Timeline: %v", err)
		}
		for i, r := range revs {
			c, err := Compile(s, r.EffectiveAt, Options{})
			if err != nil || c.SHA256 != r.Compiled.SHA256 {
				t.Fatalf("revision %d differs from a direct compile at %v (%v)", i, r.EffectiveAt, err)
			}
			if i > 0 && (!r.EffectiveAt.After(revs[i-1].EffectiveAt) || r.Compiled.SHA256 == revs[i-1].Compiled.SHA256) {
				t.Fatalf("revisions must be strictly ordered and distinct")
			}
		}
	})
}

// TestExplainAgreesWithThePolicy: Explain gives the compiled policy's answer for every pair of devices, and when a
// connection is allowed it names at least one entry that allows it on its own (and none when it isn't).
func TestExplainAgreesWithThePolicy(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := genSnapshot(t)
		if len(s.Devices) < 2 {
			return
		}
		at := t0.Add(time.Duration(rapid.IntRange(-60, 60).Draw(t, "at")) * time.Minute)
		c, err := Compile(s, at, Options{Canaries: -1})
		if err != nil {
			t.Fatalf("Compile: %v", err)
		}
		a := rapid.SampledFrom(s.Devices).Draw(t, "src")
		b := rapid.SampledFrom(s.Devices).Draw(t, "dst")
		if a.Short == b.Short {
			return
		}
		p := rapid.SampledFrom(probes).Draw(t, "probe")
		e, err := Explain(s, at, a.Short, b.Short, p.proto, p.port)
		if err != nil {
			t.Fatalf("Explain: %v", err)
		}
		want := c.Document.Allows(a, b, p.proto, p.port)
		if e.Allowed != want {
			t.Fatalf("%s -> %s %s:%d: explain=%v policy=%v", a.Short, b.Short, p.proto, p.port, e.Allowed, want)
		}
		if e.Allowed != (len(e.Because) > 0) {
			t.Fatalf("%s -> %s %s:%d: allowed=%v with reasons %+v", a.Short, b.Short, p.proto, p.port, e.Allowed, e.Because)
		}
		for _, r := range e.Because {
			// A reason for a cross-org connection is never the template or a rule: those stay inside one org.
			if a.Org != b.Org && (r.Kind == ReasonTemplate || r.Kind == ReasonRule) {
				t.Fatalf("%s(%s) -> %s(%s) explained by %+v", a.Short, a.Org, b.Short, b.Org, r)
			}
		}
	})
}
