package policy

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite golden files")

// xcelsiorSnapshot models the adopted Xcelsior cell: a platform org holding infrastructure, Aaryn's personal
// devices (user-owned) and the agent sandbox, with each GPU provider as a locked child org.
func xcelsiorSnapshot() Snapshot {
	platform, owner1, owner2, renter := sid('x', 1), sid('p', 1), sid('p', 2), sid('r', 1)
	hetzner, gpu1, gpu2, sandbox, mac, renterBox := sid('d', 1), sid('d', 2), sid('d', 3), sid('d', 4), sid('d', 5), sid('d', 6)
	return Snapshot{
		Cell: "xcelsior",
		Orgs: []Org{
			{Short: platform, Template: TemplateLocked, Platform: true, Users: []string{"xcelsior"}},
			{Short: owner1, Template: TemplateLocked, Parent: platform},
			{Short: owner2, Template: TemplateLocked, Parent: platform},
			{Short: renter, Template: TemplateTeam, Parent: platform},
		},
		Devices: []Device{
			{Short: hetzner, Org: platform, Roles: []string{"control", "gateway"}, State: DeviceActive},
			{Short: sandbox, Org: platform, Roles: []string{"sandbox"}, State: DeviceActive},
			{Short: mac, Org: platform, User: "xcelsior", State: DeviceActive},
			{Short: gpu1, Org: owner1, Roles: []string{"gpu-host"}, State: DeviceActive},
			{Short: gpu2, Org: owner2, Roles: []string{"gpu-host"}, State: DeviceActive},
			{Short: renterBox, Org: renter, State: DeviceActive},
		},
		Rules: []Rule{
			// Aaryn's personal devices reach everything the platform owns.
			{ID: "personal-all", Org: platform, Src: Users(), Dst: All(), Ports: ports("*")},
			// The sandbox reaches only the control plane's observability ports.
			{ID: "sandbox-reach", Org: platform, Src: Role("sandbox"), Dst: Role("control"), Ports: ports("tcp:3100", "tcp:9090")},
		},
		PlatformRules: []PlatformRule{
			{
				ID: "gateway-ssh", Platform: platform, Src: PlatformSelector{Scope: ScopePlatform, Role: "gateway"},
				Dst: PlatformSelector{Scope: ScopeChildren, Role: "gpu-host"}, Ports: ports("tcp:22"),
			},
			{
				ID: "hosts-spire", Platform: platform, Src: PlatformSelector{Scope: ScopeChildren, Role: "gpu-host"},
				Dst: PlatformSelector{Scope: ScopePlatform, Role: "control"}, Ports: ports("tcp:8095", "tcp:2049"),
			},
		},
		Grants: []Grant{
			// A rental: the gateway may reach gpu1's job port for two hours.
			{
				ID: "rental-1", Grantor: owner1, Grantee: platform, Src: Role("gateway"), Dst: gpu1,
				Ports: ports("tcp:10443"), StartsAt: t0.Add(-time.Hour), ExpiresAt: t0.Add(time.Hour),
			},
			// An expired rental on gpu2 must not appear.
			{
				ID: "rental-old", Grantor: owner2, Grantee: platform, Src: Role("gateway"), Dst: gpu2,
				Ports: ports("tcp:12000"), StartsAt: t0.Add(-3 * time.Hour), ExpiresAt: t0.Add(-time.Hour),
			},
		},
	}
}

func TestCompileGolden(t *testing.T) {
	c, err := Compile(xcelsiorSnapshot(), t0, Options{})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	golden := filepath.Join("testdata", "xcelsior.golden.hujson")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, c.HuJSON, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if !bytes.Equal(c.HuJSON, want) {
		t.Fatalf("compiled policy differs from %s; rerun with -update and review the diff\n--- got ---\n%s", golden, c.HuJSON)
	}
}

func TestCompileIsDeterministic(t *testing.T) {
	s := xcelsiorSnapshot()
	a, err := Compile(s, t0, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// Reverse every input slice: output must not depend on input order.
	reverse := func(n int, swap func(i, j int)) {
		for i, j := 0, n-1; i < j; i, j = i+1, j-1 {
			swap(i, j)
		}
	}
	reverse(len(s.Orgs), func(i, j int) { s.Orgs[i], s.Orgs[j] = s.Orgs[j], s.Orgs[i] })
	reverse(len(s.Devices), func(i, j int) { s.Devices[i], s.Devices[j] = s.Devices[j], s.Devices[i] })
	reverse(len(s.Rules), func(i, j int) { s.Rules[i], s.Rules[j] = s.Rules[j], s.Rules[i] })
	reverse(len(s.Grants), func(i, j int) { s.Grants[i], s.Grants[j] = s.Grants[j], s.Grants[i] })
	b, err := Compile(s, t0, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if a.SHA256 != b.SHA256 {
		t.Fatalf("output depends on input order:\n%s\nvs\n%s", a.HuJSON, b.HuJSON)
	}
}

func TestEmptySnapshotDeniesEverything(t *testing.T) {
	c, err := Compile(Snapshot{Cell: "empty"}, t0, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// An absent "grants" section would make Headscale allow all traffic.
	if !strings.Contains(string(c.HuJSON), `"grants": []`) {
		t.Fatalf("empty policy must render an explicit empty grants list:\n%s", c.HuJSON)
	}
}

func TestXcelsiorIsolation(t *testing.T) {
	s := xcelsiorSnapshot()
	c, err := Compile(s, t0, Options{})
	if err != nil {
		t.Fatal(err)
	}
	dev := map[string]Device{}
	for _, d := range s.Devices {
		dev[d.Short] = d
	}
	hetzner, gpu1, gpu2, sandbox, mac, renterBox := dev[sid('d', 1)], dev[sid('d', 2)], dev[sid('d', 3)], dev[sid('d', 4)], dev[sid('d', 5)], dev[sid('d', 6)]
	cases := []struct {
		name     string
		src, dst Device
		proto    string
		port     int
		want     bool
	}{
		{"gateway reaches gpu ssh (platform rule)", hetzner, gpu1, "tcp", 22, true},
		{"gateway reaches active rental port", hetzner, gpu1, "tcp", 10443, true},
		{"expired rental is closed", hetzner, gpu2, "tcp", 12000, false},
		{"gpu host reaches spire", gpu1, hetzner, "tcp", 8095, true},
		{"gpu host cannot reach control ssh", gpu1, hetzner, "tcp", 22, false},
		{"providers are isolated from each other", gpu1, gpu2, "tcp", 22, false},
		{"provider cannot reach personal devices", gpu1, mac, "tcp", 22, false},
		{"provider cannot reach the sandbox", gpu2, sandbox, "tcp", 8780, false},
		{"renter is isolated from providers", renterBox, gpu1, "tcp", 22, false},
		{"personal device reaches the sandbox", mac, sandbox, "tcp", 8780, true},
		{"sandbox reaches only allowed control ports", sandbox, hetzner, "tcp", 9090, true},
		{"sandbox cannot reach control ssh", sandbox, hetzner, "tcp", 22, false},
		{"sandbox cannot reach providers", sandbox, gpu1, "tcp", 22, false},
	}
	for _, tc := range cases {
		if got := c.Document.Allows(tc.src, tc.dst, tc.proto, tc.port); got != tc.want {
			t.Errorf("%s: Allows = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestQuarantinedAndRemovedDevicesAreUnreachable(t *testing.T) {
	org := sid('o', 1)
	a, b, q := sid('d', 1), sid('d', 2), sid('d', 3)
	s := Snapshot{
		Cell: "c",
		Orgs: []Org{{Short: org, Template: TemplateTeam}},
		Devices: []Device{
			{Short: a, Org: org, State: DeviceActive},
			{Short: b, Org: org, State: DeviceRemoved},
			{Short: q, Org: org, State: DeviceQuarantined},
		},
	}
	c, err := Compile(s, t0, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range s.Devices[1:] {
		if c.Document.Allows(s.Devices[0], d, "tcp", 22) || c.Document.Allows(d, s.Devices[0], "tcp", 22) {
			t.Errorf("device in state %d must be isolated", d.State)
		}
	}
	for _, g := range c.Document.Grants {
		if slices.Contains(g.Src, QuarantineTag) || slices.Contains(g.Dst, QuarantineTag) {
			t.Error("the quarantine tag must never appear in a rule")
		}
	}
	if _, ok := c.Document.TagOwners[QuarantineTag]; !ok {
		t.Error("a quarantined node's tag must still be declared, or Headscale rejects the policy")
	}
}

func TestValidationRejectsBadInput(t *testing.T) {
	org, other := sid('o', 1), sid('o', 2)
	dev := sid('d', 1)
	good := func() Snapshot {
		return Snapshot{
			Cell:    "c",
			Orgs:    []Org{{Short: org, Template: TemplateTeam}, {Short: other, Template: TemplateTeam}},
			Devices: []Device{{Short: dev, Org: org, State: DeviceActive}},
		}
	}
	cases := map[string]func(*Snapshot){
		"bad org id":          func(s *Snapshot) { s.Orgs[0].Short = "BAD" },
		"unknown template":    func(s *Snapshot) { s.Orgs[0].Template = "open" },
		"device in no org":    func(s *Snapshot) { s.Devices[0].Org = sid('z', 9) },
		"bad role":            func(s *Snapshot) { s.Devices[0].Roles = []string{"Bad Role"} },
		"parent not platform": func(s *Snapshot) { s.Orgs[1].Parent = org },
		"grant without expiry": func(s *Snapshot) {
			s.Grants = []Grant{{ID: "g", Grantor: org, Grantee: other, Src: All(), Dst: dev, Ports: ports("tcp:22"), StartsAt: t0}}
		},
		"grant with wildcard ports": func(s *Snapshot) {
			s.Grants = []Grant{{ID: "g", Grantor: org, Grantee: other, Src: All(), Dst: dev, Ports: ports("*"), StartsAt: t0, ExpiresAt: t0.Add(time.Hour)}}
		},
		"grant to own org": func(s *Snapshot) {
			s.Grants = []Grant{{ID: "g", Grantor: org, Grantee: org, Src: All(), Dst: dev, Ports: ports("tcp:22"), StartsAt: t0, ExpiresAt: t0.Add(time.Hour)}}
		},
		"grant dst in wrong org": func(s *Snapshot) {
			s.Grants = []Grant{{ID: "g", Grantor: other, Grantee: org, Src: All(), Dst: dev, Ports: ports("tcp:22"), StartsAt: t0, ExpiresAt: t0.Add(time.Hour)}}
		},
		"rule selects foreign device": func(s *Snapshot) {
			s.Rules = []Rule{{ID: "r", Org: other, Src: All(), Dst: DeviceSel(dev), Ports: ports("tcp:22")}}
		},
		"user shared by two orgs": func(s *Snapshot) { s.Orgs[0].Users = []string{"u"}; s.Orgs[1].Users = []string{"u"} },
		"roles on user-owned device": func(s *Snapshot) {
			s.Orgs[0].Users = []string{"u"}
			s.Devices[0].User = "u"
			s.Devices[0].Roles = []string{"gpu"}
		},
	}
	if err := good().Validate(); err != nil {
		t.Fatalf("baseline must validate: %v", err)
	}
	for name, mutate := range cases {
		s := good()
		mutate(&s)
		if err := s.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Validate() = %v, want ErrInvalid", name, err)
		}
	}
}

func TestPlatformRuleMustNotLinkChildren(t *testing.T) {
	p, c1 := sid('x', 1), sid('p', 1)
	s := Snapshot{
		Cell: "c",
		Orgs: []Org{{Short: p, Template: TemplateLocked, Platform: true}, {Short: c1, Template: TemplateLocked, Parent: p}},
		PlatformRules: []PlatformRule{{
			ID: "bad", Platform: p,
			Src: PlatformSelector{Scope: ScopeChildren}, Dst: PlatformSelector{Scope: ScopeChildren}, Ports: ports("tcp:22"),
		}},
	}
	if err := s.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("children-to-children platform rule must be rejected, got %v", err)
	}
}

func TestVerifyRejectsTampering(t *testing.T) {
	s := xcelsiorSnapshot()
	c, err := Compile(s, t0, Options{})
	if err != nil {
		t.Fatal(err)
	}
	owner1, owner2 := sid('p', 1), sid('p', 2)
	tampered := []struct {
		name   string
		mutate func(d *Document)
	}{
		{"cross-tenant rule", func(d *Document) {
			d.Grants = append(d.Grants, GrantRule{Src: []string{OrgTag(owner1)}, Dst: []string{OrgTag(owner2)}, IP: []string{"tcp:22"}})
		}},
		{"wildcard source", func(d *Document) {
			d.Grants = append(d.Grants, GrantRule{Src: []string{"*"}, Dst: []string{OrgTag(owner1)}, IP: []string{"tcp:22"}})
		}},
		{"autogroup", func(d *Document) {
			d.Grants = append(d.Grants, GrantRule{Src: []string{"autogroup:member"}, Dst: []string{OrgTag(owner1)}, IP: []string{"tcp:22"}})
		}},
		{"grant widened beyond its port", func(d *Document) {
			for i, g := range d.Grants {
				if strings.Join(g.IP, ",") == "tcp:10443" {
					d.Grants[i].IP = []string{"tcp:10000-11000"}
				}
			}
		}},
		{"foreign tag owner", func(d *Document) { d.TagOwners["tag:admin"] = []string{} }},
		{"undeclared tag", func(d *Document) { delete(d.TagOwners, OrgTag(owner1)) }},
	}
	for _, tc := range tampered {
		doc, err := ParseDocument(c.HuJSON)
		if err != nil {
			t.Fatal(err)
		}
		tc.mutate(&doc)
		if err := Verify(s, t0, doc); !errors.Is(err, ErrViolation) {
			t.Errorf("%s: Verify() = %v, want ErrViolation", tc.name, err)
		}
	}
	// The untampered document verifies.
	if err := Verify(s, t0, c.Document); err != nil {
		t.Fatalf("clean document must verify: %v", err)
	}
	// The same document becomes invalid once the rental grant has expired.
	if err := Verify(s, t0.Add(2*time.Hour), c.Document); !errors.Is(err, ErrViolation) {
		t.Fatalf("a policy that still contains an expired grant must fail verification, got %v", err)
	}
}

func TestParseDocumentRejectsAllowAll(t *testing.T) {
	for _, in := range []string{`{}`, `{"grants": null}`, `{"acls": [], "grants": []}`} {
		if _, err := ParseDocument([]byte(in)); err == nil {
			t.Errorf("ParseDocument(%s) must fail", in)
		}
	}
}

func TestTimeline(t *testing.T) {
	s := xcelsiorSnapshot()
	revs, err := Timeline(s, t0, 24*time.Hour, 32, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(revs) != 2 {
		t.Fatalf("want 2 revisions (now, rental expiry), got %d", len(revs))
	}
	if !revs[1].EffectiveAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("second revision effective at %v, want rental expiry", revs[1].EffectiveAt)
	}
	gw, gpu1 := s.Devices[0], s.Devices[3]
	if !revs[0].Compiled.Document.Allows(gw, gpu1, "tcp", 10443) || revs[1].Compiled.Document.Allows(gw, gpu1, "tcp", 10443) {
		t.Fatal("the rental port must be open now and closed in the next revision")
	}
	if one, _ := Timeline(s, t0, 24*time.Hour, 1, Options{}); len(one) != 1 {
		t.Fatal("max revisions must be honoured")
	}
}
