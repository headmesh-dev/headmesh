package policy

import (
	"fmt"
	"slices"
	"time"
)

// PairDelta is what one proposed rule changes between two devices.
type PairDelta struct {
	Src, Dst string // device short ids
	// New are the rule's ports this pair can't reach yet; Already are those it already can.
	New, Already []Port
}

// PreviewRule reports what adding r to s would change at instant at: for every pair of devices r lets through, which
// of r's ports become reachable and which already were. The pairs come from compiling r alone (every org locked), so
// they match what the compiler will emit. It fails exactly when s plus r doesn't compile and verify, which is when
// the rule could never reach a cell.
func PreviewRule(s Snapshot, at time.Time, r Rule) ([]PairDelta, error) {
	before, err := Compile(s, at, Options{Canaries: -1})
	if err != nil {
		return nil, err
	}
	with := s
	with.Rules = append(slices.Clone(s.Rules), r)
	if _, err := Compile(with, at, Options{Canaries: -1}); err != nil {
		return nil, err
	}
	alone := Snapshot{Cell: s.Cell, Devices: s.Devices, Orgs: slices.Clone(s.Orgs), Rules: []Rule{r}}
	for i := range alone.Orgs {
		alone.Orgs[i].Template = TemplateLocked
	}
	ruleOnly, err := Compile(alone, at, Options{Canaries: -1})
	if err != nil {
		return nil, fmt.Errorf("compile the rule alone: %w", err)
	}
	var out []PairDelta
	for _, a := range s.Devices {
		if a.Org != r.Org {
			continue
		}
		for _, b := range s.Devices {
			if b.Org != r.Org || a.Short == b.Short {
				continue
			}
			var d PairDelta
			for _, p := range r.Ports {
				if !ruleOnly.Document.AllowsPort(a, b, p) {
					continue
				}
				if before.Document.AllowsPort(a, b, p) {
					d.Already = append(d.Already, p)
				} else {
					d.New = append(d.New, p)
				}
			}
			if len(d.New)+len(d.Already) > 0 {
				d.Src, d.Dst = a.Short, b.Short
				out = append(out, d)
			}
		}
	}
	return out, nil
}
