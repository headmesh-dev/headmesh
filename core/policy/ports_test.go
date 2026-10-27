package policy

import (
	"testing"
)

func TestParsePort(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"*", "*", true},
		{"tcp:22", "tcp:22", true},
		{" TCP:22 ", "tcp:22", true},
		{"udp:53", "udp:53", true},
		{"tcp:8000-8100", "tcp:8000-8100", true},
		{"tcp:22-22", "tcp:22", true},
		{"sctp:9", "sctp:9", true},
		{"tcp:0", "", false},
		{"tcp:65536", "", false},
		{"tcp:100-10", "", false},
		{"tcp:022", "", false},
		{"icmp:1", "", false},
		{"22", "", false},
		{"tcp:", "", false},
		{"tcp:*", "", false},
	}
	for _, c := range cases {
		got, err := ParsePort(c.in)
		if (err == nil) != c.ok {
			t.Errorf("ParsePort(%q) error = %v, want ok=%v", c.in, err, c.ok)
			continue
		}
		if c.ok && string(got) != c.want {
			t.Errorf("ParsePort(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPortCoverage(t *testing.T) {
	r := func(s string) portRange {
		t.Helper()
		pr, err := Port(s).parsed()
		if err != nil {
			t.Fatal(err)
		}
		return pr
	}
	if !r("tcp:8000-8100").covers(r("tcp:8050")) {
		t.Error("range should cover inner port")
	}
	if r("tcp:8000-8100").covers(r("tcp:7999-8001")) {
		t.Error("range must not cover a straddling range")
	}
	if r("tcp:22").covers(r("udp:22")) {
		t.Error("protocol must match")
	}
	if !r("*").covers(r("udp:53")) {
		t.Error("* covers everything")
	}
	if r("tcp:22").covers(r("*")) {
		t.Error("a single port never covers *")
	}
}

func TestCheckPortsRejectsWildcardForGrants(t *testing.T) {
	if err := checkPorts(ports("*"), false); err == nil {
		t.Fatal("grants must not accept *")
	}
	if err := checkPorts(ports("TCP:22"), true); err == nil {
		t.Fatal("non-normalised ports must be rejected")
	}
	if err := checkPorts(nil, true); err == nil {
		t.Fatal("empty port list must be rejected")
	}
}

func FuzzParsePort(f *testing.F) {
	for _, s := range []string{"*", "tcp:22", "udp:1-65535", "tcp:8000-8100", "x", "tcp:-1", "tcp:1-"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		p, err := ParsePort(s)
		if err != nil {
			return
		}
		// Normalisation is idempotent and the result always parses into a sane range.
		again, err := ParsePort(string(p))
		if err != nil || again != p {
			t.Fatalf("ParsePort not idempotent: %q -> %q -> %q (%v)", s, p, again, err)
		}
		pr, err := p.parsed()
		if err != nil || pr.lo < 1 || pr.hi > 65535 || pr.lo > pr.hi {
			t.Fatalf("bad range for %q: %+v %v", p, pr, err)
		}
	})
}
