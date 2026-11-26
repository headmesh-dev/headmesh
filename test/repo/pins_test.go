package repo

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Renovate keeps every version pin current only if it can see it. Its own managers read go.mod, package.json,
// mise.toml, Dockerfile FROM lines (including `ARG X_IMAGE=…` used in FROM), workflow `uses:` and container images,
// and the pip-compile lock. Everything else, a version in a shell script, a Dockerfile ARG, a Go constant or a
// workflow env var, needs a `renovate:` annotation on the line above, read by the custom regex manager in
// .github/renovate.json. These tests find every such pin, require the annotation, and run Renovate's own regex over
// it, so a typo that Renovate would silently skip fails here instead.

// renovateCustomManager is one customManagers entry of .github/renovate.json.
type renovateCustomManager struct {
	CustomType          string   `json:"customType"`
	ManagerFilePatterns []string `json:"managerFilePatterns"`
	MatchStrings        []string `json:"matchStrings"`
}

type compiledManager struct {
	files   []*regexp.Regexp
	matches []*regexp.Regexp
}

func (m compiledManager) covers(path string) bool {
	for _, re := range m.files {
		if re.MatchString(path) {
			return true
		}
	}
	return false
}

// loadCustomManagers compiles the regex custom managers. Renovate writes patterns as "/regex/"; its regexes use
// only syntax RE2 shares with JavaScript (named groups included), so Go runs them unchanged.
func loadCustomManagers(t *testing.T, r string) []compiledManager {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(r, ".github", "renovate.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		CustomManagers []renovateCustomManager `json:"customManagers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("renovate.json: %v", err)
	}
	var out []compiledManager
	for i, m := range cfg.CustomManagers {
		if m.CustomType != "regex" {
			continue
		}
		var c compiledManager
		for _, p := range m.ManagerFilePatterns {
			if len(p) < 2 || p[0] != '/' || p[len(p)-1] != '/' {
				t.Fatalf("customManagers[%d]: managerFilePatterns entry %q is not a /regex/", i, p)
			}
			c.files = append(c.files, regexp.MustCompile(p[1:len(p)-1]))
		}
		for _, s := range m.MatchStrings {
			re, err := regexp.Compile(s)
			if err != nil {
				t.Fatalf("customManagers[%d]: matchString does not compile in RE2: %v", i, err)
			}
			if re.SubexpIndex("currentValue") < 0 || re.SubexpIndex("depName") < 0 || re.SubexpIndex("datasource") < 0 {
				t.Fatalf("customManagers[%d]: matchString needs datasource, depName and currentValue groups", i)
			}
			c.matches = append(c.matches, re)
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		t.Fatal("renovate.json has no regex custom manager")
	}
	return out
}

// pin is a version a file hard-codes.
type pin struct {
	file, name, want string
	line             int // 0-based
}

func (p pin) String() string { return fmt.Sprintf("%s:%d %s=%s", p.file, p.line+1, p.name, p.want) }

var (
	versionLike = regexp.MustCompile(`^v?[0-9]`)
	shellPin    = regexp.MustCompile(`^\s*(?:readonly\s+|export\s+)?([A-Z][A-Z0-9_]*_VERSION)="?([^"\s]+)"?\s*$`)
	dockerArg   = regexp.MustCompile(`^ARG\s+([A-Z][A-Z0-9_]*_(?:VERSION|IMAGE))=(\S+)\s*$`)
	goImage     = regexp.MustCompile(`^\s*(?:const\s+)?([a-zA-Z]\w*Image)\s*=\s*"([^"]+)"`)
	envImage    = regexp.MustCompile(`^\s*([A-Z][A-Z0-9_]*_IMAGE):\s*(\S+)\s*$`)
	envVersion  = regexp.MustCompile(`^\s*([A-Z][A-Z0-9_]*_VERSION):\s*"?([^"\s]+)"?\s*$`)
	goInstall   = regexp.MustCompile(`\bgo (?:install|run) (\S+)@(v[0-9]\S*)`)
	actionVer   = regexp.MustCompile(`^\s*(version):\s*(v?[0-9]\S*)\s*$`)
)

// exemptPins are hard-coded versions Renovate must not move, each with the reason.
var exemptPins = map[string]string{
	"deploy/tests/e2e.sh PREV_HEADSCALE_VERSION": "the release before HEADSCALE_VERSION, for the upgrade test; " +
		"it moves by hand with each Headscale bump (the Headscale group's PR notes say so)",
}

// imageTag returns the tag of an image reference such as "tailscale/tailscale:v1.104.7@sha256:…".
func imageTag(ref string) string {
	ref, _, _ = strings.Cut(ref, "@")
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ref[i+1:]
	}
	return ""
}

// findPins returns the hard-coded versions in one file that only an annotation can show Renovate.
func findPins(path, content string) []pin {
	lines := strings.Split(content, "\n")
	base := filepath.Base(path)
	isWorkflow := strings.HasPrefix(path, ".github/workflows/") && (strings.HasSuffix(path, ".yml") || strings.HasSuffix(path, ".yaml"))
	var pins []pin
	add := func(i int, name, want string) {
		if want != "" && versionLike.MatchString(want) {
			pins = append(pins, pin{file: path, name: name, want: want, line: i})
		}
	}
	for i, l := range lines {
		switch {
		case strings.HasSuffix(path, ".sh"):
			if m := shellPin.FindStringSubmatch(l); m != nil {
				add(i, m[1], m[2])
			}
		case base == "Dockerfile":
			m := dockerArg.FindStringSubmatch(l)
			if m == nil {
				continue
			}
			if strings.HasSuffix(m[1], "_IMAGE") {
				// Renovate's dockerfile manager reads an image ARG itself when a FROM uses it.
				if !strings.Contains(content, "\nFROM ${"+m[1]+"}") {
					add(i, m[1], imageTag(m[2]))
				}
				continue
			}
			add(i, m[1], m[2])
		case strings.HasSuffix(path, ".go"):
			if m := goImage.FindStringSubmatch(l); m != nil {
				add(i, m[1], imageTag(m[2]))
			}
		case isWorkflow:
			if m := envImage.FindStringSubmatch(l); m != nil {
				add(i, m[1], imageTag(m[2]))
			}
			if m := envVersion.FindStringSubmatch(l); m != nil {
				add(i, m[1], m[2])
			}
			if m := goInstall.FindStringSubmatch(l); m != nil {
				add(i, m[1], m[2])
			}
			if m := actionVer.FindStringSubmatch(l); m != nil {
				add(i, m[1], m[2])
			}
		}
	}
	return pins
}

// renovateMatch is what a custom manager extracted: the annotation's line and the currentValue's line and value.
type renovateMatch struct {
	annotation, valueLine int
	value                 string
}

func lineOf(content string, offset int) int { return strings.Count(content[:offset], "\n") }

func extract(managers []compiledManager, path, content string) []renovateMatch {
	var out []renovateMatch
	for _, m := range managers {
		if !m.covers(path) {
			continue
		}
		for _, re := range m.matches {
			cv := re.SubexpIndex("currentValue")
			for _, loc := range re.FindAllStringSubmatchIndex(content, -1) {
				out = append(out, renovateMatch{
					annotation: lineOf(content, loc[0]),
					valueLine:  lineOf(content, loc[2*cv]),
					value:      content[loc[2*cv]:loc[2*cv+1]],
				})
			}
		}
	}
	return out
}

func TestEveryPinIsVisibleToRenovate(t *testing.T) {
	r := root(t)
	managers := loadCustomManagers(t, r)
	// An annotation is a comment line of its own with the pin on the next line, the only form the custom manager
	// reads. A `# renovate:` quoted mid-line, as in the docs, is prose.
	annotation := regexp.MustCompile(`(?m)^[ \t]*(?:#|//)[ \t]*renovate:`)
	var checked int
	for _, path := range repoFiles(t, r) {
		if strings.Contains(path, "node_modules/") || strings.HasPrefix(path, "gen/") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(r, path))
		if err != nil {
			continue // a tracked file deleted in the working tree
		}
		content := string(raw)
		pins := findPins(path, content)
		hasAnnotation := annotation.MatchString(content)
		if len(pins) == 0 && !hasAnnotation {
			continue
		}
		matches := extract(managers, path, content)
		lines := strings.Split(content, "\n")

		for _, p := range pins {
			if reason, ok := exemptPins[p.file+" "+p.name]; ok {
				t.Logf("exempt: %s (%s)", p, reason)
				continue
			}
			checked++
			if p.line == 0 || !annotation.MatchString(lines[p.line-1]) {
				t.Errorf("%s has no `renovate:` annotation on the line above, so Renovate can't see it", p)
				continue
			}
			found := false
			for _, m := range matches {
				if m.annotation == p.line-1 && m.valueLine == p.line && m.value == p.want {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%s: the custom manager in .github/renovate.json doesn't extract %q from it", p, p.want)
			}
		}

		// Every annotation must be read, or Renovate skips the pin without a word.
		for i, l := range lines {
			if !annotation.MatchString(l) {
				continue
			}
			read := false
			for _, m := range matches {
				if m.annotation == i && m.valueLine == i+1 {
					read = true
					break
				}
			}
			if !read {
				t.Errorf("%s:%d: Renovate's custom manager doesn't read this annotation and the line below it", path, i+1)
			}
		}
	}
	if checked < 30 {
		t.Fatalf("checked only %d pins; the pin detectors have stopped finding them", checked)
	}
}

// TestCertbotPinMatchesItsLock: the bootstrap's CERTBOT_VERSION names the install directory, and the hashed lock
// says what is installed there. Renovate moves them in one PR (the certbot group); this keeps them equal.
func TestCertbotPinMatchesItsLock(t *testing.T) {
	r := root(t)
	pinned := regexp.MustCompile(`(?m)^readonly CERTBOT_VERSION="([^"]+)"$`).FindStringSubmatch(readFile(t, r, "deploy/server/bootstrap.sh"))
	locked := regexp.MustCompile(`(?m)^certbot==(\S+)`).FindStringSubmatch(readFile(t, r, "deploy/server/certbot-requirements.in"))
	if pinned == nil || locked == nil {
		t.Fatalf("CERTBOT_VERSION %v or certbot== in certbot-requirements.in %v not found", pinned, locked)
	}
	if pinned[1] != locked[1] {
		t.Fatalf("deploy/server/bootstrap.sh pins certbot %s but certbot-requirements.in has %s", pinned[1], locked[1])
	}
}

// TestNodeAndPnpmPinsAgree: CI and the hub server should run the Node and pnpm that developers get from mise.toml and
// package.json; a workflow pinned to another version tests a toolchain nobody runs. Renovate moves them together
// (same package, one branch; the pnpm group), and this catches a hand edit that moves only one.
func TestNodeAndPnpmPinsAgree(t *testing.T) {
	r := root(t)
	node := regexp.MustCompile(`(?m)^node = "([^"]+)"$`).FindStringSubmatch(readFile(t, r, "mise.toml"))
	pnpm := regexp.MustCompile(`"packageManager": "pnpm@([^"+]+)`).FindStringSubmatch(readFile(t, r, "package.json"))
	if node == nil || pnpm == nil {
		t.Fatalf("node in mise.toml %v or packageManager pnpm in package.json %v not found", node, pnpm)
	}
	want := map[string]string{"NODE_VERSION": node[1], "PNPM_VERSION": pnpm[1]}
	checked := 0
	for _, path := range repoFiles(t, r) {
		if !strings.HasPrefix(path, ".github/workflows/") && !strings.HasSuffix(path, ".sh") {
			continue
		}
		for _, p := range findPins(path, readFile(t, r, path)) {
			if w, ok := want[p.name]; ok {
				checked++
				if p.want != w {
					t.Errorf("%s:%d: %s is %s, but mise.toml and package.json give %s", path, p.line+1, p.name, p.want, w)
				}
			}
		}
	}
	if checked < 4 {
		t.Fatalf("checked only %d Node and pnpm pins; the search is broken", checked)
	}
}
