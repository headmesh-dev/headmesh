// Package scansecrets finds likely secret files inside the sandbox's
// read-write mounts (plan §6.4): filename heuristics produce proposed
// /dev/null masks; gitleaks findings are listed for review.
package scansecrets

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Finding is a file worth masking or reviewing.
type Finding struct {
	Path    string `json:"path"`
	Reason  string `json:"reason"`
	Propose bool   `json:"propose_mask"`
}

var skipDirs = map[string]bool{
	".git": true, "node_modules": true, ".venv": true, "venv": true, "__pycache__": true, ".cache": true,
	".headmesh-trash": true, "dist": true, "build": true, ".next": true, "target": true, ".mypy_cache": true, ".pytest_cache": true,
}

var nameRules = []struct {
	re     *regexp.Regexp
	reason string
}{
	{regexp.MustCompile(`^\.env(\..+)?$`), "dotenv file"},
	{regexp.MustCompile(`\.(pem|key|p12|pfx|jks|keystore)$`), "key material"},
	{regexp.MustCompile(`^id_(rsa|ecdsa|ed25519|dsa)(_[A-Za-z0-9_-]+)?$`), "SSH private key"},
	{regexp.MustCompile(`^credentials.*\.json$|^service[-_]account.*\.json$`), "cloud credentials"},
	{regexp.MustCompile(`^\.(netrc|pgpass|npmrc|pypirc)$`), "credential file"},
	{regexp.MustCompile(`\.kubeconfig$|^kubeconfig$`), "kubeconfig"},
	{regexp.MustCompile(`(^|[._-])secrets?(\.[A-Za-z0-9]+)*$`), "secrets file"},
}

var safeRe = regexp.MustCompile(`(?i)(example|sample|template|dist|defaults?)$|\.(example|sample|template)\.|\.pub$|test|fixture`)

// ByName walks roots and flags files whose names look like secrets.
func ByName(roots []string, maxFiles int) []Finding {
	var out []Finding
	seen := 0
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil //nolint:nilerr // skip unreadable entries
			}
			if d.IsDir() {
				if p != root && skipDirs[d.Name()] {
					return filepath.SkipDir
				}
				return nil
			}
			seen++
			if maxFiles > 0 && seen > maxFiles {
				return filepath.SkipAll
			}
			name := d.Name()
			for _, r := range nameRules {
				if r.re.MatchString(name) {
					if safeRe.MatchString(name) {
						break
					}
					out = append(out, Finding{Path: p, Reason: r.reason, Propose: true})
					break
				}
			}
			return nil
		})
	}
	slices.SortFunc(out, func(a, b Finding) int { return strings.Compare(a.Path, b.Path) })
	return out
}

// Gitleaks runs `gitleaks dir` on root through runner (the host binary, or
// `docker exec … gitleaks` when it only exists in the image) and returns
// files with findings, for review (never auto-masked: they may be source).
func Gitleaks(ctx context.Context, runner []string, root string) ([]Finding, error) {
	args := append(slices.Clone(runner[1:]), "dir", root, "--no-banner", "--log-level", "error", "--redact",
		"--report-format", "json", "--report-path", "-", "--exit-code", "0")
	cmd := exec.CommandContext(ctx, runner[0], args...) //nolint:gosec // G204: gitleaks or docker with argv
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, err //nolint:wrapcheck // caller reports it
	}
	var raw []struct {
		File   string
		RuleID string
	}
	if err := json.Unmarshal(out.Bytes(), &raw); err != nil {
		return nil, err //nolint:wrapcheck // caller reports it
	}
	byFile := map[string][]string{}
	for _, f := range raw {
		if !slices.Contains(byFile[f.File], f.RuleID) {
			byFile[f.File] = append(byFile[f.File], f.RuleID)
		}
	}
	var res []Finding
	for f, rules := range byFile {
		res = append(res, Finding{Path: f, Reason: "gitleaks: " + strings.Join(rules, ", ")})
	}
	slices.SortFunc(res, func(a, b Finding) int { return strings.Compare(a.Path, b.Path) })
	return res, nil
}

var masksRe = regexp.MustCompile(`(?ms)^masks\s*=\s*\[(.*?)\]`)

// ApplyMasks adds paths to the `masks = [...]` array in the [mounts] table
// of sandbox.toml text, keeping everything else (comments included).
func ApplyMasks(toml string, add []string) (string, int, bool) {
	loc := masksRe.FindStringSubmatchIndex(toml)
	if loc == nil {
		return toml, 0, false
	}
	body := toml[loc[2]:loc[3]]
	var lines []string
	n := 0
	for _, p := range add {
		q, _ := json.Marshal(p)
		if strings.Contains(body, string(q)) {
			continue
		}
		lines = append(lines, "  "+string(q)+",")
		n++
	}
	if n == 0 {
		return toml, 0, true
	}
	trimmed := strings.TrimRight(body, " \n\t")
	if trimmed != "" && !strings.HasSuffix(trimmed, ",") {
		trimmed += ","
	}
	newBody := trimmed + "\n" + strings.Join(lines, "\n") + "\n"
	if strings.TrimSpace(body) == "" {
		newBody = "\n" + strings.Join(lines, "\n") + "\n"
	}
	return toml[:loc[2]] + newBody + toml[loc[3]:], n, true
}
