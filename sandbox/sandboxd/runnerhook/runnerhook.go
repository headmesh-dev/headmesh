// Package runnerhook implements the Claude self-hosted runner lifecycle
// hooks (plan §7.1). checkout links a repo that's mapped in [repos] to the
// shared working copy, on main and fetched, so cloud tasks work in the same
// tree as every other agent; unmapped repos get a fresh clone in
// runner-work. post-session unregisters the session's agents and records
// what the session left behind.
package runnerhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/headmesh-dev/headmesh/sandbox/config"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/gitx"
)

// Checkout is the checkout hook's environment.
type Checkout struct {
	RepoURL      string // CLAUDE_RUNNER_REPO_URL
	RepoRef      string // CLAUDE_RUNNER_REPO_REF
	CheckoutPath string // CLAUDE_RUNNER_CHECKOUT_PATH
	SessionID    string // CLAUDE_RUNNER_SESSION_ID
}

// CheckoutFromEnv reads the runner's variables.
func CheckoutFromEnv() Checkout {
	return Checkout{
		RepoURL:      os.Getenv("CLAUDE_RUNNER_REPO_URL"),
		RepoRef:      os.Getenv("CLAUDE_RUNNER_REPO_REF"),
		CheckoutPath: os.Getenv("CLAUDE_RUNNER_CHECKOUT_PATH"),
		SessionID:    os.Getenv("CLAUDE_RUNNER_SESSION_ID"),
	}
}

// Result is printed as JSON for the runner log.
type Result struct {
	Repo       string   `json:"repo"`
	Mapped     bool     `json:"mapped"`
	Target     string   `json:"target"`
	Link       string   `json:"link"`
	Branch     string   `json:"branch"`
	Fetched    bool     `json:"fetched"`
	Warnings   []string `json:"warnings,omitempty"`
	RequestRef string   `json:"requested_ref,omitempty"`
}

// Hook holds the dependencies.
type Hook struct {
	Config  *config.Config
	WorkDir string // /srv/runner-work
	Env     []string
	Out     io.Writer
	Now     func() time.Time
}

var ownerRepoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// OwnerRepo extracts "owner/repo" from https, ssh or scp-style git URLs.
func OwnerRepo(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("CLAUDE_RUNNER_REPO_URL is empty")
	}
	var path string
	if u, err := url.Parse(s); err == nil && u.Scheme != "" && u.Host != "" {
		path = u.Path
	} else if _, after, ok := strings.Cut(s, ":"); ok && strings.Contains(s, "@") {
		path = after // git@github.com:owner/repo.git
	} else {
		path = s
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return "", fmt.Errorf("can't find owner/repo in %q", raw)
	}
	or := parts[len(parts)-2] + "/" + parts[len(parts)-1]
	if !ownerRepoRe.MatchString(or) {
		return "", fmt.Errorf("unexpected repo name %q", or)
	}
	return or, nil
}

func (h *Hook) git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // G204: fixed binary, argv only
	env := h.Env
	if len(env) == 0 {
		env = os.Environ()
	}
	cmd.Env = append(env[:len(env):len(env)], "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// RunCheckout prepares the checkout directory the runner expects.
func (h *Hook) RunCheckout(ctx context.Context, c Checkout) (Result, error) {
	or, err := OwnerRepo(c.RepoURL)
	if err != nil {
		return Result{}, err
	}
	if c.CheckoutPath == "" || !filepath.IsAbs(c.CheckoutPath) {
		return Result{}, fmt.Errorf("CLAUDE_RUNNER_CHECKOUT_PATH %q must be absolute", c.CheckoutPath)
	}
	res := Result{Repo: or, Link: c.CheckoutPath, RequestRef: c.RepoRef}
	target, mapped := h.mapped(or)
	res.Mapped = mapped
	if mapped {
		if _, err := os.Stat(filepath.Join(target, ".git")); err != nil {
			return res, fmt.Errorf("[repos] maps %s to %s, but that is not a git working copy", or, target)
		}
		res.Warnings = append(res.Warnings, h.prepareShared(ctx, target, &res)...)
	} else {
		target = filepath.Join(h.WorkDir, filepath.FromSlash(or))
		if err := h.prepareClone(ctx, target, c, &res); err != nil {
			return res, err
		}
	}
	res.Target = target
	if err := link(c.CheckoutPath, target, h.now()); err != nil {
		return res, err
	}
	if h.Out != nil {
		_ = json.NewEncoder(h.Out).Encode(res)
	}
	return res, nil
}

func (h *Hook) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *Hook) mapped(or string) (string, bool) {
	for k, v := range h.Config.Repos {
		if strings.EqualFold(k, or) {
			return v, true
		}
	}
	return "", false
}

// prepareShared fetches and puts the shared tree on main when that's safe.
// It never discards work: a dirty tree on another branch is left alone.
func (h *Hook) prepareShared(ctx context.Context, dir string, res *Result) []string {
	var warn []string
	if _, err := h.git(ctx, dir, "fetch", "--prune", "origin"); err != nil {
		warn = append(warn, "fetch failed; continuing with the local state: "+err.Error())
	} else {
		res.Fetched = true
	}
	branch, _ := h.git(ctx, dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	g := &gitx.Git{Env: h.Env}
	if p := g.InProgress(ctx, dir); p != "" {
		warn = append(warn, "a "+p+" is in progress in the shared working copy; left as is")
		res.Branch = branch
		return warn
	}
	if branch != "main" && branch != "master" {
		st, err := g.Status(ctx, dir)
		switch {
		case err != nil:
			warn = append(warn, "status failed: "+err.Error())
		case !st.Clean:
			warn = append(warn, fmt.Sprintf("shared working copy is on %q with uncommitted changes; not switching to main", branch))
		default:
			if _, err := h.git(ctx, dir, "checkout", "main"); err != nil {
				warn = append(warn, "checkout main failed: "+err.Error())
			} else {
				branch = "main"
			}
		}
	}
	res.Branch = branch
	if res.Fetched && (branch == "main" || branch == "master") {
		if _, err := h.git(ctx, dir, "merge", "--ff-only", "@{upstream}"); err != nil {
			warn = append(warn, "main could not fast-forward (local commits or local changes); agents will git_sync before pushing")
		}
	}
	return warn
}

func (h *Hook) prepareClone(ctx context.Context, dir string, c Checkout, res *Result) error {
	if _, err := os.Stat(filepath.Join(dir, ".git")); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
			return fmt.Errorf("runner-work: %w", err)
		}
		if _, err := h.git(ctx, filepath.Dir(dir), "clone", "--quiet", "--", c.RepoURL, dir); err != nil {
			return err
		}
	} else if _, err := h.git(ctx, dir, "fetch", "--prune", "origin"); err != nil {
		res.Warnings = append(res.Warnings, "fetch failed: "+err.Error())
	}
	res.Fetched = true
	if c.RepoRef != "" {
		if _, err := h.git(ctx, dir, "checkout", "--quiet", c.RepoRef); err != nil {
			res.Warnings = append(res.Warnings, "checkout "+c.RepoRef+" failed: "+err.Error())
		}
	}
	res.Branch, _ = h.git(ctx, dir, "symbolic-ref", "--quiet", "--short", "HEAD")
	return nil
}

// link points path at target, moving aside anything else that's there.
func link(path, target string, now time.Time) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("checkout parent: %w", err)
	}
	if cur, err := os.Readlink(path); err == nil {
		if cur == target {
			return nil
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("replace old link: %w", err)
		}
	} else if fi, err := os.Lstat(path); err == nil {
		if fi.IsDir() {
			if entries, _ := os.ReadDir(path); len(entries) == 0 {
				_ = os.Remove(path)
			}
		}
		if _, err := os.Lstat(path); err == nil {
			aside := path + ".old-" + now.UTC().Format("20060102T150405")
			if err := os.Rename(path, aside); err != nil {
				return fmt.Errorf("move old checkout aside: %w", err)
			}
		}
	}
	if err := os.Symlink(target, path); err != nil {
		return fmt.Errorf("link checkout: %w", err)
	}
	return nil
}

// PostSession is the post-session hook's environment.
type PostSession struct {
	SessionID      string   // CLAUDE_RUNNER_SESSION_ID
	ExitReason     string   // CLAUDE_RUNNER_EXIT_REASON
	WorkspacePaths []string // CLAUDE_RUNNER_WORKSPACE_PATHS (colon or newline separated)
}

// PostSessionFromEnv reads the runner's variables.
func PostSessionFromEnv() PostSession {
	raw := os.Getenv("CLAUDE_RUNNER_WORKSPACE_PATHS")
	var paths []string
	for _, p := range strings.FieldsFunc(raw, func(r rune) bool { return r == ':' || r == '\n' || r == ',' }) {
		if p = strings.TrimSpace(p); p != "" {
			paths = append(paths, p)
		}
	}
	return PostSession{SessionID: os.Getenv("CLAUDE_RUNNER_SESSION_ID"), ExitReason: os.Getenv("CLAUDE_RUNNER_EXIT_REASON"), WorkspacePaths: paths}
}

// SessionReport records what a session left behind.
type SessionReport struct {
	SessionID  string         `json:"session_id"`
	ExitReason string         `json:"exit_reason"`
	EndedAt    time.Time      `json:"ended_at"`
	Repos      []gitx.Summary `json:"repos"`
	Note       string         `json:"note"`
}

// Report summarizes the session's workspaces (resolving checkout links).
func (h *Hook) Report(ctx context.Context, p PostSession) SessionReport {
	rep := SessionReport{
		SessionID: p.SessionID, ExitReason: p.ExitReason, EndedAt: h.now().UTC(), Repos: []gitx.Summary{},
		Note: "repos linked by the checkout hook are not pushed by the runner; uncommitted or unpushed work listed here stays in the shared working copy",
	}
	g := &gitx.Git{Env: h.Env}
	seen := map[string]bool{}
	for _, wp := range p.WorkspacePaths {
		real, err := filepath.EvalSymlinks(wp)
		if err != nil || seen[real] {
			continue
		}
		seen[real] = true
		if _, err := os.Stat(filepath.Join(real, ".git")); err != nil {
			continue
		}
		rep.Repos = append(rep.Repos, g.Summarize(ctx, real))
	}
	return rep
}
