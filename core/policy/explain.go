package policy

import (
	"fmt"
	"slices"
	"time"
)

// ReasonKind names the kind of snapshot entry that allows a connection.
type ReasonKind string

// The four sources of access the compiler emits.
const (
	ReasonTemplate     ReasonKind = "template"
	ReasonRule         ReasonKind = "rule"
	ReasonGrant        ReasonKind = "grant"
	ReasonPlatformRule ReasonKind = "platform_rule"
)

// Reason is one entry of a snapshot that allows a connection on its own.
type Reason struct {
	Kind ReasonKind
	ID   string // the rule, grant or platform rule id; empty for a template
	Org  string // who owns the entry: the template's or rule's org, the grantor, or the platform
}

// Explanation answers "may src reach dst on proto/port at this instant, and why".
type Explanation struct {
	Allowed bool
	Because []Reason
}

// Explain compiles s and reports whether the device with short id src may open a connection to dst on proto/port
// at instant at. Because lists every entry that would allow it on its own: the org's template, a rule, a grant or
// a platform rule. Each candidate is compiled alone, with every org locked, so the explanation comes from the same
// compiler (and the same Verify) as the policy the cell enforces, rather than from a second copy of its logic.
func Explain(s Snapshot, at time.Time, src, dst, proto string, port int) (Explanation, error) {
	ix, err := s.index()
	if err != nil {
		return Explanation{}, err
	}
	a, ok := ix.devices[src]
	if !ok {
		return Explanation{}, invalidf("unknown device %q", src)
	}
	b, ok := ix.devices[dst]
	if !ok {
		return Explanation{}, invalidf("unknown device %q", dst)
	}
	full, err := Compile(s, at, Options{Canaries: -1})
	if err != nil {
		return Explanation{}, err
	}
	out := Explanation{Allowed: full.Document.Allows(a, b, proto, port)}
	if !out.Allowed {
		return out, nil
	}

	// Only the two orgs and their platforms can contribute an edge between these devices.
	scope := map[string]bool{a.Org: true, b.Org: true}
	for _, short := range []string{a.Org, b.Org} {
		if p := ix.orgs[short].Parent; p != "" {
			scope[p] = true
		}
	}
	base := Snapshot{Cell: s.Cell}
	for _, o := range s.Orgs {
		if scope[o.Short] {
			base.Orgs = append(base.Orgs, o)
		}
	}
	for _, d := range s.Devices {
		if scope[d.Org] {
			base.Devices = append(base.Devices, d)
		}
	}
	locked := func() Snapshot {
		c := Snapshot{Cell: base.Cell, Devices: base.Devices, Orgs: slices.Clone(base.Orgs)}
		for i := range c.Orgs {
			c.Orgs[i].Template = TemplateLocked
		}
		return c
	}
	try := func(c Snapshot, r Reason) error {
		compiled, err := Compile(c, at, Options{Canaries: -1})
		if err != nil {
			return fmt.Errorf("explain %s %s: %w", r.Kind, r.ID, err)
		}
		if compiled.Document.Allows(a, b, proto, port) {
			out.Because = append(out.Because, r)
		}
		return nil
	}

	if a.Org == b.Org && ix.orgs[a.Org].Template == TemplateTeam {
		if err := try(base, Reason{Kind: ReasonTemplate, Org: a.Org}); err != nil {
			return Explanation{}, err
		}
	}
	for _, r := range s.Rules {
		if r.Org != a.Org || a.Org != b.Org {
			continue
		}
		c := locked()
		c.Rules = []Rule{r}
		if err := try(c, Reason{Kind: ReasonRule, ID: r.ID, Org: r.Org}); err != nil {
			return Explanation{}, err
		}
	}
	for _, g := range s.Grants {
		if g.Grantee != a.Org || g.Grantor != b.Org {
			continue
		}
		c := locked()
		c.Grants = []Grant{g}
		if err := try(c, Reason{Kind: ReasonGrant, ID: g.ID, Org: g.Grantor}); err != nil {
			return Explanation{}, err
		}
	}
	for _, pr := range s.PlatformRules {
		if !scope[pr.Platform] {
			continue
		}
		c := locked()
		c.PlatformRules = []PlatformRule{pr}
		if err := try(c, Reason{Kind: ReasonPlatformRule, ID: pr.ID, Org: pr.Platform}); err != nil {
			return Explanation{}, err
		}
	}
	return out, nil
}
