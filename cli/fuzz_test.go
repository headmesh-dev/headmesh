package cli

import (
	"net/url"
	"strings"
	"testing"
)

// FuzzParseSelector: a selector that parses prints back as the same text and parses to the same value, so what
// the CLI sends is exactly what the user typed.
func FuzzParseSelector(f *testing.F) {
	for _, s := range []string{"all", "users", "role:gpu", "device:dev123", "role:", "device", "all:x", "role:a:b", ":", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, v string) {
		s, err := ParseSelector(v)
		if err != nil {
			return
		}
		if s.String() != v {
			t.Fatalf("ParseSelector(%q) = %+v, which prints as %q", v, s, s.String())
		}
		again, err := ParseSelector(s.String())
		if err != nil || again != s {
			t.Fatalf("selector %+v does not round-trip: %+v, %v", s, again, err)
		}
	})
}

// FuzzParseInvite: an invite is an hmj_ token, alone or as the last /join/ segment of an https link, and the API
// base it yields is that link's own https origin and path prefix.
func FuzzParseInvite(f *testing.F) {
	for _, s := range []string{
		"hmj_abc", "https://api.headmesh.dev/join/hmj_abc", "https://x.example/sub/join/hmj_t",
		"http://api.headmesh.dev/join/hmj_abc", "https://api.headmesh.dev/join/abc", "https:///join/hmj_x",
		"https://h/join/hmj_a/join/hmj_b", "hmj_", "", "https://h/%zz/join/hmj_a",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, arg string) {
		token, base, err := parseInvite(arg)
		if err != nil {
			if token != "" || base != "" {
				t.Fatalf("parseInvite(%q) failed (%v) but returned %q, %q", arg, err, token, base)
			}
			return
		}
		if !strings.HasPrefix(token, "hmj_") {
			t.Fatalf("parseInvite(%q) token %q lacks the hmj_ prefix", arg, token)
		}
		if strings.HasPrefix(arg, "hmj_") {
			if token != arg || base != "" {
				t.Fatalf("a raw token must pass through unchanged: %q -> %q, %q", arg, token, base)
			}
			return
		}
		u, err := url.Parse(arg)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			t.Fatalf("parseInvite accepted %q, which is not an https URL with a host (%v)", arg, err)
		}
		if base+"/join/"+token != "https://"+u.Host+u.Path {
			t.Fatalf("parseInvite(%q) = %q, %q: base + /join/ + token is not the link's origin and path", arg, token, base)
		}
		if strings.Contains(token, "/join/hmj_") {
			t.Fatalf("parseInvite(%q) token %q is not the last /join/ segment", arg, token)
		}
	})
}
