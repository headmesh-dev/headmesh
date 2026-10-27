package gitx

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitEnv isolates tests from the host's global/system git config and hooks.
func gitEnv(t *testing.T) []string {
	t.Helper()
	home := t.TempDir()
	cfg := filepath.Join(home, "gitconfig")
	if err := os.WriteFile(cfg, []byte("[user]\n\tname = Test\n\temail = t@example.com\n[init]\n\tdefaultBranch = main\n[commit]\n\tgpgsign = false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return append(os.Environ(), "HOME="+home, "GIT_CONFIG_GLOBAL="+cfg, "GIT_CONFIG_NOSYSTEM=1")
}

func sh(t *testing.T, env []string, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir, cmd.Env = dir, env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func commit(t *testing.T, env []string, dir, file, content, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	sh(t, env, dir, "add", file)
	sh(t, env, dir, "commit", "-q", "-m", msg)
}

// setup returns an origin with two clones a and b on main.
func setup(t *testing.T) (env []string, a, b string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	env = gitEnv(t)
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	sh(t, env, root, "init", "-q", "--bare", origin)
	a, b = filepath.Join(root, "a"), filepath.Join(root, "b")
	sh(t, env, root, "clone", "-q", origin, a)
	commit(t, env, a, "README.md", "hello\n", "init")
	sh(t, env, a, "push", "-q", "origin", "HEAD:refs/heads/main")
	sh(t, env, a, "branch", "-q", "--set-upstream-to=origin/main")
	sh(t, env, root, "clone", "-q", origin, b)
	return env, a, b
}

func TestStatusParsing(t *testing.T) {
	out := strings.Join([]string{
		"# branch.oid 1234abcd",
		"# branch.head main",
		"# branch.upstream origin/main",
		"# branch.ab +2 -1",
		"1 M. N... 100644 100644 100644 aaa bbb staged.go",
		"1 .M N... 100644 100644 100644 aaa bbb dir/unstaged file.go",
		"2 R. N... 100644 100644 100644 aaa bbb R100 new.go\told.go",
		"u UU N... 100644 100644 100644 100644 aaa bbb ccc conflict.go",
		"? untracked.txt",
		"",
	}, "\n")
	st := ParsePorcelainV2(out)
	if st.Branch != "main" || st.Head != "1234abcd" || st.Upstream != "origin/main" || st.Ahead != 2 || st.Behind != 1 {
		t.Fatalf("branch info %+v", st)
	}
	if st.Staged != 2 || st.Unstaged != 1 || st.Unmerged != 1 || st.Untracked != 1 || st.Clean {
		t.Fatalf("counts %+v", st)
	}
	paths := []string{}
	for _, f := range st.Files {
		paths = append(paths, f.Path)
	}
	if strings.Join(paths, "|") != "staged.go|dir/unstaged file.go|new.go|conflict.go|untracked.txt" {
		t.Fatalf("paths %v", paths)
	}
}

func TestSyncAndPush(t *testing.T) {
	env, a, b := setup(t)
	g := &Git{Env: env}
	ctx := context.Background()

	commit(t, env, b, "b.txt", "from b\n", "b work")
	pr, err := g.PushSafe(ctx, b, "", "")
	if err != nil || !pr.Pushed || len(pr.Commits) != 1 {
		t.Fatalf("push from b: %+v %v", pr, err)
	}

	// a has a local commit and is behind: sync rebases with autostash.
	commit(t, env, a, "a.txt", "from a\n", "a work")
	if err := os.WriteFile(filepath.Join(a, "README.md"), []byte("hello\nwip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := g.Status(ctx, a)
	if err != nil || st.Unstaged != 1 {
		t.Fatalf("status before sync %+v %v", st, err)
	}
	sr, err := g.Sync(ctx, a, true)
	if err != nil || sr.Conflict || len(sr.NewCommits) != 2 {
		t.Fatalf("sync %+v %v", sr, err)
	}
	if b, _ := os.ReadFile(filepath.Join(a, "README.md")); string(b) != "hello\nwip\n" {
		t.Fatalf("autostash lost work: %q", b)
	}
	sh(t, env, a, "checkout", "-q", "README.md")
	pr, err = g.PushSafe(ctx, a, "origin", "main")
	if err != nil || !pr.Pushed {
		t.Fatalf("push from a: %+v %v", pr, err)
	}
	pr, err = g.PushSafe(ctx, a, "origin", "main")
	if err != nil || pr.Pushed || !strings.Contains(pr.Message, "nothing to push") {
		t.Fatalf("second push %+v %v", pr, err)
	}
	if _, err := g.PushSafe(ctx, a, "origin", "other"); err == nil {
		t.Fatal("pushing a branch that isn't checked out was accepted")
	}
}

func TestConflictStopsWithoutForce(t *testing.T) {
	env, a, b := setup(t)
	g := &Git{Env: env}
	ctx := context.Background()
	commit(t, env, b, "README.md", "b version\n", "b edit")
	sh(t, env, b, "push", "-q", "origin", "HEAD:refs/heads/main")
	commit(t, env, a, "README.md", "a version\n", "a edit")
	before := strings.TrimSpace(sh(t, env, a, "rev-parse", "HEAD"))

	sr, err := g.Sync(ctx, a, true)
	if err != nil || !sr.Conflict || !sr.Aborted || len(sr.ConflictedFiles) != 1 {
		t.Fatalf("sync conflict %+v %v", sr, err)
	}
	if after := strings.TrimSpace(sh(t, env, a, "rev-parse", "HEAD")); after != before {
		t.Fatalf("abort didn't restore HEAD: %s != %s", after, before)
	}
	pr, err := g.PushSafe(ctx, a, "", "")
	if err != nil || pr.Pushed || !pr.Conflict {
		t.Fatalf("push with conflict %+v %v", pr, err)
	}
	// Remote still has b's version: nothing was forced.
	remote := sh(t, env, a, "show", "origin/main:README.md")
	if remote != "b version\n" {
		t.Fatalf("remote changed: %q", remote)
	}

	// Without abort the rebase stays open and further operations refuse.
	sr, err = g.Sync(ctx, a, false)
	if err != nil || !sr.Conflict || sr.Aborted {
		t.Fatalf("sync no-abort %+v %v", sr, err)
	}
	if _, err := g.PushSafe(ctx, a, "", ""); !errors.Is(err, ErrInProgress) {
		t.Fatalf("push during rebase: %v", err)
	}
	if s := g.Summarize(ctx, a); s.Progress != "rebase" {
		t.Fatalf("summary %+v", s)
	}
}

func TestSyncWithoutUpstream(t *testing.T) {
	env, a, _ := setup(t)
	g := &Git{Env: env}
	sh(t, env, a, "checkout", "-q", "-b", "feature")
	sr, err := g.Sync(context.Background(), a, true)
	if err != nil || !strings.Contains(sr.Message, "no upstream") {
		t.Fatalf("%+v %v", sr, err)
	}
	sh(t, env, a, "checkout", "-q", "--detach")
	if _, err := g.Sync(context.Background(), a, true); err == nil {
		t.Fatal("detached HEAD accepted")
	}
}
