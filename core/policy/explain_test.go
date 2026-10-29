package policy

import (
	"testing"
	"time"
)

func TestExplainNamesEachEntryThatAllows(t *testing.T) {
	at := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	s := Snapshot{
		Cell: "x",
		Orgs: []Org{{Short: "aaaaaaaaaa", Template: TemplateTeam}, {Short: "bbbbbbbbbb", Template: TemplateLocked}},
		Devices: []Device{
			{Short: "aaaaaaaaa2", Org: "aaaaaaaaaa", Roles: []string{"ops"}, State: DeviceActive, Registered: true},
			{Short: "aaaaaaaaa3", Org: "aaaaaaaaaa", Roles: []string{"gpu"}, State: DeviceActive, Registered: true},
			{Short: "bbbbbbbbb2", Org: "bbbbbbbbbb", Roles: []string{"gw"}, State: DeviceActive, Registered: true},
		},
		Rules: []Rule{
			{ID: "r-ssh", Org: "aaaaaaaaaa", Src: Role("ops"), Dst: Role("gpu"), Ports: []Port{"tcp:22"}},
			{ID: "r-web", Org: "aaaaaaaaaa", Src: All(), Dst: All(), Ports: []Port{"tcp:80"}},
		},
		Grants: []Grant{{
			ID: "g1", Grantor: "aaaaaaaaaa", Grantee: "bbbbbbbbbb", Src: All(), Dst: "aaaaaaaaa3",
			Ports: []Port{"tcp:22"}, StartsAt: at.Add(-time.Hour), ExpiresAt: at.Add(time.Hour),
		}},
	}
	for _, tc := range []struct {
		name, src, dst string
		port           int
		want           []string
	}{
		{"template and rule", "aaaaaaaaa2", "aaaaaaaaa3", 22, []string{"template:", "rule:r-ssh"}},
		{"template alone", "aaaaaaaaa3", "aaaaaaaaa2", 22, []string{"template:"}},
		{"grant", "bbbbbbbbb2", "aaaaaaaaa3", 22, []string{"grant:g1"}},
		{"nothing across orgs the other way", "aaaaaaaaa3", "bbbbbbbbb2", 22, nil},
		{"grant covers only its port", "bbbbbbbbb2", "aaaaaaaaa3", 80, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, err := Explain(s, at, tc.src, tc.dst, "tcp", tc.port)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, r := range e.Because {
				got = append(got, string(r.Kind)+":"+r.ID)
			}
			if e.Allowed != (tc.want != nil) || len(got) != len(tc.want) {
				t.Fatalf("allowed=%v because=%v, want %v", e.Allowed, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("because=%v, want %v", got, tc.want)
				}
			}
		})
	}
	if _, err := Explain(s, at, "zzzzzzzzzz", "aaaaaaaaa2", "tcp", 22); err == nil {
		t.Fatal("an unknown device must be an error")
	}
}
