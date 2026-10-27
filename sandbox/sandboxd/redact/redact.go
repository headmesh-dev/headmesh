// Package redact removes likely secrets from tool output when
// sandboxd.redact_output is on: built-in patterns always, plus gitleaks
// findings when the gitleaks binary is installed.
package redact

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Placeholder replaces each secret.
const Placeholder = "[REDACTED]"

var builtin = []*regexp.Regexp{
	regexp.MustCompile(`hms_[A-Za-z0-9_-]{43}`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{60,}`),
	regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`\bsk-(?:proj-|svcacct-|admin-)?[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\btskey-[a-z]+-[A-Za-z0-9-]{16,}`),
	regexp.MustCompile(`\bhskey-[a-z]+-[A-Za-z0-9_.-]{16,}`),
	regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`),
	regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`),
}

// Redactor applies the patterns.
type Redactor struct {
	// Gitleaks is the gitleaks binary; empty disables it.
	Gitleaks string
	// MaxGitleaksBytes skips gitleaks for larger outputs (default 1 MiB).
	MaxGitleaksBytes int
}

// New returns a redactor that uses gitleaks when it's on PATH.
func New() *Redactor {
	r := &Redactor{}
	if p, err := exec.LookPath("gitleaks"); err == nil {
		r.Gitleaks = p
	}
	return r
}

// String redacts s.
func (r *Redactor) String(ctx context.Context, s string) string {
	if r == nil || s == "" {
		return s
	}
	for _, re := range builtin {
		s = re.ReplaceAllString(s, Placeholder)
	}
	limit := r.MaxGitleaksBytes
	if limit == 0 {
		limit = 1 << 20
	}
	if r.Gitleaks == "" || len(s) > limit {
		return s
	}
	for _, secret := range r.gitleaksFindings(ctx, s) {
		if len(secret) >= 8 {
			s = strings.ReplaceAll(s, secret, Placeholder)
		}
	}
	return s
}

func (r *Redactor) gitleaksFindings(ctx context.Context, s string) []string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.Gitleaks, "stdin", //nolint:gosec // G204: fixed binary; args are passed as argv, never through a shell
		"--no-banner", "--log-level", "error",
		"--report-format", "json", "--report-path", "-", "--exit-code", "0")
	cmd.Stdin = strings.NewReader(s)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil
	}
	var findings []struct{ Secret string }
	if json.Unmarshal(out.Bytes(), &findings) != nil {
		return nil
	}
	secrets := make([]string, 0, len(findings))
	for _, f := range findings {
		secrets = append(secrets, f.Secret)
	}
	// Longest first so overlapping secrets are fully replaced.
	slices.SortFunc(secrets, func(a, b string) int { return len(b) - len(a) })
	return secrets
}
