package policy

import (
	"fmt"
	"testing"
	"time"
)

// largeSnapshot builds a cell at the plan's §9.3 budget: 2,000 orgs, 5,000 devices and 10,000 grants under one
// platform, plus a team-template org per 10 orgs.
func largeSnapshot(orgs, devices, grants int) Snapshot {
	s := Snapshot{Cell: "bench"}
	platform := sid('x', 0)
	s.Orgs = append(s.Orgs, Org{Short: platform, Template: TemplateLocked, Platform: true})
	for i := range orgs {
		tmpl := TemplateLocked
		if i%10 == 0 {
			tmpl = TemplateTeam
		}
		s.Orgs = append(s.Orgs, Org{Short: sid('o', i), Template: tmpl, Parent: platform})
	}
	s.Devices = append(s.Devices, Device{Short: sid('g', 0), Org: platform, Roles: []string{"gateway"}, State: DeviceActive})
	for i := range devices {
		s.Devices = append(s.Devices, Device{Short: sid('d', i), Org: sid('o', i%orgs), Roles: []string{"gpu-host"}, State: DeviceActive})
	}
	for i := range grants {
		s.Grants = append(s.Grants, Grant{
			ID: fmt.Sprint(i), Grantor: sid('o', i%orgs), Grantee: platform, Src: Role("gateway"),
			Dst: sid('d', i%orgs), Ports: []Port{Port(fmt.Sprintf("tcp:%d", 10000+i%50000))},
			StartsAt: t0.Add(-time.Hour), ExpiresAt: t0.Add(time.Duration(i%600+1) * time.Minute),
		})
	}
	s.PlatformRules = []PlatformRule{{
		ID: "ssh", Platform: platform,
		Src: PlatformSelector{Scope: ScopePlatform, Role: "gateway"},
		Dst: PlatformSelector{Scope: ScopeChildren, Role: "gpu-host"}, Ports: []Port{"tcp:22"},
	}}
	return s
}

func BenchmarkCompileLargeCell(b *testing.B) {
	s := largeSnapshot(2000, 5000, 10000)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Compile(s, t0, Options{}); err != nil {
			b.Fatal(err)
		}
	}
}
