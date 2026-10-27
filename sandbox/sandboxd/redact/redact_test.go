package redact

import (
	"context"
	"strings"
	"testing"
)

func TestBuiltinPatterns(t *testing.T) {
	r := &Redactor{}
	secrets := []string{
		"hms_" + strings.Repeat("A", 43),
		"ghp_" + strings.Repeat("a", 36),
		"github_pat_" + strings.Repeat("B", 70),
		"sk-ant-api03-" + strings.Repeat("x", 30),
		"sk-proj-" + strings.Repeat("y", 30),
		"AKIA" + strings.Repeat("Q", 16),
		"tskey-auth-" + strings.Repeat("k", 20),
		"eyJ" + strings.Repeat("h", 16) + "." + strings.Repeat("p", 16) + "." + strings.Repeat("s", 16),
		"-----BEGIN OPENSSH PRIVATE KEY-----\nabc\ndef\n-----END OPENSSH PRIVATE KEY-----",
	}
	for _, s := range secrets {
		got := r.String(context.Background(), "before "+s+" after")
		if got != "before "+Placeholder+" after" {
			t.Errorf("not redacted: %q -> %q", s, got)
		}
	}
	plain := "build ok in 3.2s; sk-short; token=abc"
	if got := r.String(context.Background(), plain); got != plain {
		t.Errorf("false positive: %q", got)
	}
	var nilR *Redactor
	if nilR.String(context.Background(), "x") != "x" {
		t.Error("nil redactor changed text")
	}
}

func TestGitleaksWhenAvailable(t *testing.T) {
	r := New()
	if r.Gitleaks == "" {
		t.Skip("gitleaks not installed")
	}
	in := `api_key = "abcdef0123456789ABCDEF0123456789"`
	if got := r.String(context.Background(), in); strings.Contains(got, "abcdef0123456789") {
		t.Errorf("gitleaks finding not redacted: %q", got)
	}
}
