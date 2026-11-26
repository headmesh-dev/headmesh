package repo

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// agplPackage matches the AGPL-3.0 packages of LICENSING.md that permissively licensed code must never import.
var agplPackage = regexp.MustCompile(`^` + regexp.QuoteMeta(modulePath) + `/(api|core|internal|db)(/|$)`)

// importers runs `go list -deps -test` over patterns in dir and returns, for every AGPL package in the result, the
// packages that import it directly. Test variants ("p [p.test]") count, because test files ship in the source too.
func importers(t *testing.T, dir string, patterns ...string) map[string][]string {
	t.Helper()
	args := append([]string{"list", "-deps", "-test", "-f", `{{.ImportPath}}{{range .Imports}} {{.}}{{end}}`}, patterns...)
	found := map[string][]string{}
	for line := range strings.Lines(run(t, dir, "go", args...)) {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		pkg, _, _ := strings.Cut(fields[0], " [")
		if agplPackage.MatchString(pkg) {
			continue // AGPL importing AGPL is fine; only the edge into it matters
		}
		for _, imp := range fields[1:] {
			if agplPackage.MatchString(imp) && !slices.Contains(found[imp], pkg) {
				found[imp] = append(found[imp], pkg)
			}
		}
	}
	return found
}

// TestPermissiveCodeNeverImportsAGPLCode enforces LICENSING.md: the Apache-2.0 parts (the CLI, the generated cell
// protocol and the sandbox module) and the vendored BSD-3-Clause Headscale code never import an AGPL-3.0 package,
// directly or through a dependency, including from their tests.
func TestPermissiveCodeNeverImportsAGPLCode(t *testing.T) {
	r := root(t)
	sets := []struct {
		name     string
		dir      string
		patterns []string
	}{
		{
			"Apache-2.0 and BSD-3-Clause packages of the main module", r,
			[]string{"./cli/...", "./cmd/headmesh", "./gen/cell/...", "./gen/headscale/..."},
		},
		{"the sandbox module", filepath.Join(r, "sandbox"), []string{"./..."}},
	}
	// The native clients (client roadmap C0) are Apache-2.0 too, whether they become their own module or not.
	if _, err := os.Stat(filepath.Join(r, "clients")); err == nil {
		if _, err := os.Stat(filepath.Join(r, "clients", "go.mod")); err == nil {
			sets = append(sets, struct {
				name     string
				dir      string
				patterns []string
			}{"the clients module", filepath.Join(r, "clients"), []string{"./..."}})
		} else {
			sets[0].patterns = append(sets[0].patterns, "./clients/...")
		}
	}
	for _, s := range sets {
		for agpl, by := range importers(t, s.dir, s.patterns...) {
			t.Errorf("%s: %s is AGPL-3.0 but imported by %s (LICENSING.md)", s.name, agpl, strings.Join(by, ", "))
		}
	}
}

// TestLicenceCheckSeesAGPLImports is the control for the test above: the server binary does import AGPL packages,
// so a matcher or `go list` invocation that silently found nothing would fail here.
func TestLicenceCheckSeesAGPLImports(t *testing.T) {
	if len(importers(t, root(t), "./cmd/headmesh-server")) == 0 {
		t.Fatal("found no AGPL imports under cmd/headmesh-server; the licence boundary check can't see anything")
	}
}
