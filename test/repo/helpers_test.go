package repo

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const modulePath = "github.com/headmesh-dev/headmesh"

// root returns the repository root: two levels up from this package, checked by its go.mod.
func root(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	mod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil || !bytes.HasPrefix(mod, []byte("module "+modulePath+"\n")) {
		t.Fatalf("%s is not the %s module root (%v)", dir, modulePath, err)
	}
	return dir
}

// run runs a command in dir and returns its standard output, failing the test with standard error on error.
func run(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), name, args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %s (in %s): %v\n%s", name, strings.Join(args, " "), dir, err, stderr.String())
	}
	return string(out)
}

// readFile returns the file at rel, relative to the repository root r.
func readFile(t *testing.T, r, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// repoFiles lists the files under dir that git tracks or would track (new files not yet committed, minus the
// ignored ones), relative to dir, so a check also covers work in progress. It skips the test outside a git
// checkout, such as in a module zip.
func repoFiles(t *testing.T, dir string) []string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	if err := exec.CommandContext(t.Context(), "git", "-C", dir, "rev-parse", "--is-inside-work-tree").Run(); err != nil {
		t.Skip("not a git checkout")
	}
	var files []string
	for f := range strings.SplitSeq(run(t, dir, "git", "ls-files", "-z", "--cached", "--others", "--exclude-standard"), "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files
}
