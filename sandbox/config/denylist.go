package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// DefaultDenylist is the secret denylist from plan §6.4, plus the host's
// Docker and tailscaled sockets (either one is root on the host or its
// tailnet identity). `~` is the host user's home.
var DefaultDenylist = []string{
	"~/.ssh",
	"~/.gnupg",
	"~/.aws",
	"~/.azure",
	"~/.config/gcloud",
	"~/.kube",
	"~/.docker",
	"~/.password-store",
	"~/.local/share/keyrings",
	"~/.config/gh",
	"~/.netrc",
	"~/.pgpass",
	"~/.claude/.credentials.json",
	"~/.codex/auth.json",
	"~/.config/restic",
	"/etc/headscale",
	"/etc/wireguard",
	"/etc/headmesh",
	"/var/lib/tailscale*",
	"/var/lib/headscale",
	"/var/backups/headscale",
	"/root",
	"/etc/shadow",
	"/etc/gshadow",
	"/etc/ssh/ssh_host_*",
	"/var/run/docker.sock",
	"/run/docker.sock",
	"/var/run/tailscale*",
	"/run/tailscale*",
}

// Denylist matches host paths against secret patterns.
type Denylist struct {
	entries []denyEntry
}

type denyEntry struct {
	raw  string // as written in the config, e.g. "~/.ssh"
	path string // expanded absolute pattern, may contain glob metacharacters
}

// DenyMatch is an existing host path that a pattern denies.
type DenyMatch struct {
	Pattern string
	Path    string
	IsDir   bool
}

// Denylist builds the effective denylist: built-ins, extra_denylist and the
// sandbox's own secrets directory, minus allow_secrets.
func (c *Config) Denylist() *Denylist {
	raw := slices.Concat(DefaultDenylist, c.Mounts.ExtraDenylist, []string{filepath.Join(c.Root, "secrets")})
	return NewDenylist(raw, c.HostHome, c.Mounts.AllowSecrets)
}

// NewDenylist expands `~/` against home and drops allowed entries (matched
// by their raw or expanded spelling).
func NewDenylist(patterns []string, home string, allow []string) *Denylist {
	d := &Denylist{}
	for _, p := range dedupe(patterns) {
		exp := expandHome(p, home)
		if slices.Contains(allow, p) || slices.Contains(allow, exp) {
			continue
		}
		d.entries = append(d.entries, denyEntry{raw: p, path: exp})
	}
	return d
}

func expandHome(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return filepath.Clean(p)
}

// Patterns returns the raw patterns in order.
func (d *Denylist) Patterns() []string {
	out := make([]string, len(d.entries))
	for i, e := range d.entries {
		out[i] = e.raw
	}
	return out
}

// Inside reports the first pattern that p equals or lies below.
func (d *Denylist) Inside(p string) (string, bool) {
	p = filepath.Clean(p)
	for _, e := range d.entries {
		if matchPrefix(e.path, p) {
			return e.raw, true
		}
	}
	return "", false
}

// matchPrefix reports whether p or one of its ancestors matches the pattern.
func matchPrefix(pattern, p string) bool {
	for cur := p; ; cur = filepath.Dir(cur) {
		if ok, _ := filepath.Match(pattern, cur); ok {
			return true
		}
		if cur == "/" || cur == "." {
			return false
		}
	}
}

// Under lists existing host paths that the denylist covers strictly below
// root. Those need masks when root is mounted.
func (d *Denylist) Under(root string) []DenyMatch {
	root = filepath.Clean(root)
	var out []DenyMatch
	seen := map[string]bool{}
	for _, e := range d.entries {
		var candidates []string
		if strings.ContainsAny(e.path, "*?[") {
			candidates, _ = filepath.Glob(e.path)
		} else {
			candidates = []string{e.path}
		}
		for _, c := range candidates {
			if c == root || !isWithin(root, c) || seen[c] {
				continue
			}
			fi, err := os.Lstat(c)
			if err != nil {
				continue
			}
			seen[c] = true
			out = append(out, DenyMatch{Pattern: e.raw, Path: c, IsDir: fi.IsDir()})
		}
	}
	slices.SortFunc(out, func(a, b DenyMatch) int { return strings.Compare(a.Path, b.Path) })
	return out
}

// MountViolation explains why a mount source was refused.
type MountViolation struct {
	Mount   string
	Pattern string
}

func (v MountViolation) Error() string {
	return fmt.Sprintf("mount %s is inside denylisted secret path %s (list it under [mounts] allow_secrets only if you mean it)", v.Mount, v.Pattern)
}

// CheckMount refuses a source that equals or lies inside a denied path,
// looking at both the spelled path and its resolved target.
func (d *Denylist) CheckMount(src string) error {
	if pat, ok := d.Inside(src); ok {
		return MountViolation{Mount: src, Pattern: pat}
	}
	if real, err := filepath.EvalSymlinks(src); err == nil && real != src {
		if pat, ok := d.Inside(real); ok {
			return MountViolation{Mount: src + " -> " + real, Pattern: pat}
		}
	}
	return nil
}
