package repo

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestEveryFuzzTargetRunsInCI: a fuzz target only finds bugs if something runs it. Every `func FuzzX(f *testing.F)`
// in the repository must be in the 30-second list of ci-go.yml and in the 10-minute matrix of fuzz-nightly.yml.
func TestEveryFuzzTargetRunsInCI(t *testing.T) {
	r := root(t)
	pr, nightly := readFile(t, r, ".github/workflows/ci-go.yml"), readFile(t, r, ".github/workflows/fuzz-nightly.yml")
	fuzzFunc := regexp.MustCompile(`(?m)^func (Fuzz\w+)\(\w+ \*testing\.F\)`)
	found := 0
	for _, path := range repoFiles(t, r) {
		if !strings.HasSuffix(path, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(r, path))
		if err != nil {
			continue
		}
		// The sandbox is its own module, so its packages are named from sandbox/.
		dir, pkg := ".", "./"+filepath.ToSlash(filepath.Dir(path))
		if rest, ok := strings.CutPrefix(path, "sandbox/"); ok {
			dir, pkg = "sandbox", "./"+filepath.ToSlash(filepath.Dir(rest))
		}
		for _, m := range fuzzFunc.FindAllStringSubmatch(string(raw), -1) {
			found++
			if line := fmt.Sprintf("%s %s %s", dir, pkg, m[1]); !strings.Contains(pr, line) {
				t.Errorf("%s: ci-go.yml's fuzz job lacks the line %q", path, line)
			}
			if entry := fmt.Sprintf("{ dir: %s, pkg: %s, target: %s }", dir, pkg, m[1]); !strings.Contains(nightly, entry) {
				t.Errorf("%s: fuzz-nightly.yml's matrix lacks %q", path, entry)
			}
		}
	}
	if found == 0 {
		t.Fatal("found no fuzz targets; the search is broken")
	}
}
