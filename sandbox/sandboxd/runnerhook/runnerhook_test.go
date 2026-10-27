package runnerhook

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/headmesh-dev/headmesh/sandbox/config"
)

func TestOwnerRepo(t *testing.T) {
	cases := map[string]string{
		"https://github.com/aabiro/xcelsior.git":        "aabiro/xcelsior",
		"https://github.com/headmesh-dev/headmesh":      "headmesh-dev/headmesh",
		"git@github.com:aabiro/xcelsior.git":            "aabiro/xcelsior",
		"ssh://git@github.com/aabiro/xcelsior.git":      "aabiro/xcelsior",
		"https://ghe.example.com/org/sub/Repo.Name.git": "sub/Repo.Name",
	}
	for in, want := range cases {
		if got, err := OwnerRepo(in); err != nil || got != want {
			t.Errorf("OwnerRepo(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "https://github.com/onlyowner", "x"} {
		if _, err := OwnerRepo(bad); err == nil {
			t.Errorf("OwnerRepo(%q) accepted", bad)
		}
	}
}

func gitEnv(t *testing.T) []string {
	t.Helper()
	home := t.TempDir()
	cfg := filepath.Join(home, "gitconfig")
	if err := os.WriteFile(cfg, []byte("[user]\n\tname = T\n\temail = t@e.x\n[init]\n\tdefaultBranch = main\n[protocol \"file\"]\n\tallow = always\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return append(os.Environ(), "HOME="+home, "GIT_CONFIG_GLOBAL="+cfg, "GIT_CONFIG_NOSYSTEM=1")
}

func run(t *testing.T, env []string, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir, cmd.Env = dir, env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestCheckoutMappedAndUnmapped(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	env := gitEnv(t)
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	run(t, env, root, "init", "-q", "--bare", origin)
	shared := filepath.Join(root, "projects", "xcelsior")
	run(t, env, root, "clone", "-q", origin, shared)
	if err := os.WriteFile(filepath.Join(shared, "README.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, env, shared, "add", ".")
	run(t, env, shared, "commit", "-q", "-m", "init")
	run(t, env, shared, "push", "-q", "-u", "origin", "main")
	run(t, env, shared, "checkout", "-q", "-b", "feature")

	cfg := &config.Config{Repos: map[string]string{"aabiro/xcelsior": shared}}
	var out bytes.Buffer
	h := &Hook{Config: cfg, WorkDir: filepath.Join(root, "runner-work"), Env: env, Out: &out}
	base := filepath.Join(root, "runner")

	// Mapped: clean tree on a feature branch is moved back to main and linked.
	res, err := h.RunCheckout(context.Background(), Checkout{RepoURL: "https://github.com/aabiro/xcelsior.git", CheckoutPath: filepath.Join(base, "aabiro", "xcelsior")})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Mapped || res.Branch != "main" || !res.Fetched || res.Target != shared {
		t.Fatalf("mapped result %+v", res)
	}
	if got, _ := os.Readlink(filepath.Join(base, "aabiro", "xcelsior")); got != shared {
		t.Fatalf("link -> %q", got)
	}
	if !strings.Contains(out.String(), `"mapped":true`) {
		t.Fatalf("json output %q", out.String())
	}

	// A dirty tree on another branch is left alone with a warning.
	run(t, env, shared, "checkout", "-q", "feature")
	if err := os.WriteFile(filepath.Join(shared, "wip.txt"), []byte("wip"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = h.RunCheckout(context.Background(), Checkout{RepoURL: "git@github.com:aabiro/xcelsior.git", CheckoutPath: filepath.Join(base, "aabiro", "xcelsior")})
	if err != nil {
		t.Fatal(err)
	}
	if res.Branch != "feature" || len(res.Warnings) == 0 || !strings.Contains(strings.Join(res.Warnings, " "), "not switching") {
		t.Fatalf("dirty result %+v", res)
	}

	// Unmapped: fresh clone under runner-work; an old directory is moved aside.
	link := filepath.Join(base, "other", "repo")
	if err := os.MkdirAll(link, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(link, "stale"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = h.RunCheckout(context.Background(), Checkout{RepoURL: origin + "/other/repo", CheckoutPath: link})
	if err == nil {
		t.Fatalf("expected clone of a missing repo to fail, got %+v", res)
	}
	h.Config.Repos = map[string]string{}
	res, err = h.RunCheckout(context.Background(), Checkout{RepoURL: "file://" + origin, CheckoutPath: link, RepoRef: "main"})
	if err != nil {
		t.Fatalf("unmapped clone: %v", err)
	}
	if res.Mapped || !strings.HasPrefix(res.Target, h.WorkDir) {
		t.Fatalf("unmapped result %+v", res)
	}
	if _, err := os.Stat(filepath.Join(res.Target, "README.md")); err != nil {
		t.Fatalf("clone missing files: %v", err)
	}
	matches, _ := filepath.Glob(link + ".old-*")
	if len(matches) != 1 {
		t.Fatalf("old checkout not moved aside: %v", matches)
	}

	rep := h.Report(context.Background(), PostSession{SessionID: "session_1", ExitReason: "completed", WorkspacePaths: []string{filepath.Join(base, "aabiro", "xcelsior"), link}})
	if len(rep.Repos) != 2 || rep.Repos[0].Dirty == 0 {
		t.Fatalf("report %+v", rep)
	}
}

func TestPostSessionFromEnv(t *testing.T) {
	t.Setenv("CLAUDE_RUNNER_WORKSPACE_PATHS", "/srv/runner/a/b:/srv/runner/c/d\n")
	t.Setenv("CLAUDE_RUNNER_SESSION_ID", "session_x")
	p := PostSessionFromEnv()
	if p.SessionID != "session_x" || len(p.WorkspacePaths) != 2 {
		t.Fatalf("%+v", p)
	}
}
