package policy

import (
	"slices"
	"time"
)

// Revision is a compiled policy and the instant it takes effect.
type Revision struct {
	EffectiveAt time.Time
	Compiled    *Compiled
}

// Timeline compiles the policy in force at now plus one revision at every scheduled boundary (a rule or grant
// starting or expiring) within horizon, keeping at most max revisions. Cell agents apply future revisions on
// schedule, so scheduled expiries are enforced even while the control server is unreachable.
// Consecutive revisions with identical output are collapsed.
func Timeline(s Snapshot, now time.Time, horizon time.Duration, maxRevisions int, opt Options) ([]Revision, error) {
	if maxRevisions < 1 {
		maxRevisions = 1
	}
	first, err := Compile(s, now, opt)
	if err != nil {
		return nil, err
	}
	out := []Revision{{EffectiveAt: now, Compiled: first}}
	for _, at := range boundaries(s, now, now.Add(horizon)) {
		if len(out) >= maxRevisions {
			break
		}
		c, err := Compile(s, at, opt)
		if err != nil {
			return nil, err
		}
		if c.SHA256 == out[len(out)-1].Compiled.SHA256 {
			continue
		}
		out = append(out, Revision{EffectiveAt: at, Compiled: c})
	}
	return out, nil
}

// boundaries returns the sorted instants in (from, to] at which some rule or grant changes state.
func boundaries(s Snapshot, from, to time.Time) []time.Time {
	var ts []time.Time
	add := func(t time.Time) {
		if !t.IsZero() && t.After(from) && !t.After(to) {
			ts = append(ts, t)
		}
	}
	for _, r := range s.Rules {
		add(r.StartsAt)
		add(r.ExpiresAt)
	}
	for _, g := range s.Grants {
		if g.Revoked {
			continue
		}
		add(g.StartsAt)
		add(g.ExpiresAt)
	}
	slices.SortFunc(ts, func(a, b time.Time) int { return a.Compare(b) })
	return slices.CompactFunc(ts, func(a, b time.Time) bool { return a.Equal(b) })
}
