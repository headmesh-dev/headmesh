// Package gitx implements git_status, git_sync and git_push_safe. None of
// them ever force-pushes; sync and push stop on conflicts and say so.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Git runs git commands.
type Git struct {
	// Env is the base environment; os.Environ() when empty.
	Env []string
	// Timeout bounds each git command (default 2 minutes).
	Timeout time.Duration
}

func (g *Git) run(ctx context.Context, repo string, args ...string) (string, string, error) {
	timeout := g.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...) //nolint:gosec // G204: fixed binary; args are passed as argv, never through a shell
	env := g.Env
	if len(env) == 0 {
		env = os.Environ()
	}
	cmd.Env = append(slices.Clone(env), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		return out.String(), errb.String(), fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return out.String(), errb.String(), nil
}

func (g *Git) gitPathExists(ctx context.Context, repo, name string) bool {
	out, _, err := g.run(ctx, repo, "rev-parse", "--git-path", name)
	if err != nil {
		return false
	}
	p := strings.TrimSpace(out)
	if !filepath.IsAbs(p) {
		p = filepath.Join(repo, p)
	}
	_, err = os.Stat(p)
	return err == nil
}

// InProgress names an unfinished rebase or merge, or returns "".
func (g *Git) InProgress(ctx context.Context, repo string) string {
	switch {
	case g.gitPathExists(ctx, repo, "rebase-merge"), g.gitPathExists(ctx, repo, "rebase-apply"):
		return "rebase"
	case g.gitPathExists(ctx, repo, "MERGE_HEAD"):
		return "merge"
	case g.gitPathExists(ctx, repo, "CHERRY_PICK_HEAD"):
		return "cherry-pick"
	}
	return ""
}

// FileStatus is one changed path.
type FileStatus struct {
	Path string `json:"path"`
	XY   string `json:"xy"`
	Kind string `json:"kind"` // changed, renamed, unmerged, untracked
}

// Status is the git_status output.
type Status struct {
	Repo       string       `json:"repo"`
	Branch     string       `json:"branch"`
	Head       string       `json:"head"`
	Upstream   string       `json:"upstream,omitempty"`
	Ahead      int          `json:"ahead"`
	Behind     int          `json:"behind"`
	Staged     int          `json:"staged"`
	Unstaged   int          `json:"unstaged"`
	Untracked  int          `json:"untracked"`
	Unmerged   int          `json:"unmerged"`
	Clean      bool         `json:"clean"`
	InProgress string       `json:"in_progress,omitempty"`
	Files      []FileStatus `json:"files"`
	Truncated  bool         `json:"truncated"`
	Agents     any          `json:"other_agents,omitempty"`
}

// MaxStatusFiles caps the file list.
const MaxStatusFiles = 300

// Status reports branch, ahead/behind and changes (porcelain v2).
func (g *Git) Status(ctx context.Context, repo string) (Status, error) {
	out, _, err := g.run(ctx, repo, "status", "--porcelain=v2", "--branch", "--untracked-files=normal")
	if err != nil {
		return Status{}, err
	}
	st := ParsePorcelainV2(out)
	st.Repo = repo
	st.InProgress = g.InProgress(ctx, repo)
	return st, nil
}

// ParsePorcelainV2 parses `git status --porcelain=v2 --branch` output.
func ParsePorcelainV2(out string) Status {
	st := Status{Files: []FileStatus{}}
	add := func(fs FileStatus) {
		if len(st.Files) < MaxStatusFiles {
			st.Files = append(st.Files, fs)
		} else {
			st.Truncated = true
		}
	}
	for line := range strings.SplitSeq(out, "\n") {
		switch {
		case strings.HasPrefix(line, "# branch.oid "):
			st.Head = strings.TrimPrefix(line, "# branch.oid ")
		case strings.HasPrefix(line, "# branch.head "):
			st.Branch = strings.TrimPrefix(line, "# branch.head ")
		case strings.HasPrefix(line, "# branch.upstream "):
			st.Upstream = strings.TrimPrefix(line, "# branch.upstream ")
		case strings.HasPrefix(line, "# branch.ab "):
			var a, b int
			if _, err := fmt.Sscanf(strings.TrimPrefix(line, "# branch.ab "), "+%d -%d", &a, &b); err == nil {
				st.Ahead, st.Behind = a, b
			}
		case strings.HasPrefix(line, "1 "), strings.HasPrefix(line, "2 "):
			f := strings.SplitN(line, " ", 9)
			if line[0] == '2' {
				f = strings.SplitN(line, " ", 10)
			}
			if len(f) < 9 {
				continue
			}
			xy := f[1]
			path := f[len(f)-1]
			kind := "changed"
			if line[0] == '2' {
				kind = "renamed"
				path, _, _ = strings.Cut(path, "\t")
			}
			if xy[0] != '.' {
				st.Staged++
			}
			if xy[1] != '.' {
				st.Unstaged++
			}
			add(FileStatus{Path: path, XY: xy, Kind: kind})
		case strings.HasPrefix(line, "u "):
			f := strings.SplitN(line, " ", 11)
			if len(f) == 11 {
				st.Unmerged++
				add(FileStatus{Path: f[10], XY: f[1], Kind: "unmerged"})
			}
		case strings.HasPrefix(line, "? "):
			st.Untracked++
			add(FileStatus{Path: strings.TrimPrefix(line, "? "), XY: "??", Kind: "untracked"})
		}
	}
	st.Clean = st.Staged == 0 && st.Unstaged == 0 && st.Untracked == 0 && st.Unmerged == 0
	return st
}

// SyncResult is the git_sync output.
type SyncResult struct {
	Repo            string   `json:"repo"`
	Branch          string   `json:"branch"`
	Before          string   `json:"before"`
	After           string   `json:"after"`
	NewCommits      []string `json:"new_commits"`
	Conflict        bool     `json:"conflict"`
	ConflictedFiles []string `json:"conflicted_files,omitempty"`
	Aborted         bool     `json:"aborted,omitempty"`
	Message         string   `json:"message"`
}

// ErrInProgress means a rebase or merge must be finished first.
var ErrInProgress = errors.New("a rebase or merge is in progress")

func (g *Git) head(ctx context.Context, repo string) string {
	out, _, err := g.run(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func (g *Git) currentBranch(ctx context.Context, repo string) (string, error) {
	out, _, err := g.run(ctx, repo, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return "", errors.New("HEAD is detached; check out a branch first")
	}
	return strings.TrimSpace(out), nil
}

func (g *Git) conflicted(ctx context.Context, repo string) []string {
	out, _, _ := g.run(ctx, repo, "diff", "--name-only", "--diff-filter=U")
	return strings.Fields(out)
}

func (g *Git) log(ctx context.Context, repo, rng string) []string {
	out, _, err := g.run(ctx, repo, "log", "--oneline", "--no-decorate", "-n", "200", rng)
	if err != nil {
		return []string{}
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return []string{}
	}
	return lines
}

// Sync fetches and rebases the current branch onto its upstream with
// autostash. On conflict it stops; with abort it also aborts the rebase so
// the shared working copy stays usable for other agents.
func (g *Git) Sync(ctx context.Context, repo string, abort bool) (SyncResult, error) {
	res := SyncResult{Repo: repo, NewCommits: []string{}}
	if p := g.InProgress(ctx, repo); p != "" {
		return res, fmt.Errorf("%w (%s); finish or abort it first", ErrInProgress, p)
	}
	branch, err := g.currentBranch(ctx, repo)
	if err != nil {
		return res, err
	}
	res.Branch = branch
	if _, _, err := g.run(ctx, repo, "fetch", "--prune"); err != nil {
		return res, err
	}
	res.Before = g.head(ctx, repo)
	if _, _, err := g.run(ctx, repo, "rev-parse", "--abbrev-ref", "@{upstream}"); err != nil {
		res.After = res.Before
		res.Message = "fetched; branch " + branch + " has no upstream, nothing to rebase"
		return res, nil //nolint:nilerr // no upstream is a normal outcome, reported in Message
	}
	_, stderr, err := g.run(ctx, repo, "pull", "--rebase", "--autostash")
	if err != nil {
		if g.InProgress(ctx, repo) == "rebase" {
			res.Conflict = true
			res.ConflictedFiles = g.conflicted(ctx, repo)
			if abort {
				if _, _, aerr := g.run(ctx, repo, "rebase", "--abort"); aerr == nil {
					res.Aborted = true
				}
			}
			res.After = g.head(ctx, repo)
			if res.Aborted {
				res.Message = "rebase hit conflicts and was aborted; your commits are unchanged. Resolve by hand or ask Aaryn; never force-push."
			} else {
				res.Message = "rebase stopped on conflicts; resolve them, `git add` and `git rebase --continue`, or `git rebase --abort`."
			}
			return res, nil
		}
		return res, fmt.Errorf("pull --rebase failed: %w", err)
	}
	res.After = g.head(ctx, repo)
	if res.Before != "" && res.Before != res.After {
		res.NewCommits = g.log(ctx, repo, res.Before+".."+res.After)
	}
	res.Message = strings.TrimSpace(firstLine(stderr))
	if res.Message == "" {
		res.Message = "up to date"
	}
	return res, nil
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return l
}

// PushResult is the git_push_safe output.
type PushResult struct {
	Repo       string   `json:"repo"`
	Remote     string   `json:"remote"`
	Branch     string   `json:"branch"`
	From       string   `json:"from,omitempty"`
	To         string   `json:"to"`
	Commits    []string `json:"commits"`
	Pushed     bool     `json:"pushed"`
	Conflict   bool     `json:"conflict"`
	Conflicted []string `json:"conflicted_files,omitempty"`
	Message    string   `json:"message"`
}

// PushSafe rebases the current branch onto <remote>/<branch>, runs the
// hooks and pushes without force. A rejected push is reported, not retried
// with force.
func (g *Git) PushSafe(ctx context.Context, repo, remote, branch string) (PushResult, error) {
	if remote == "" {
		remote = "origin"
	}
	res := PushResult{Repo: repo, Remote: remote, Commits: []string{}}
	if p := g.InProgress(ctx, repo); p != "" {
		return res, fmt.Errorf("%w (%s); finish or abort it first", ErrInProgress, p)
	}
	cur, err := g.currentBranch(ctx, repo)
	if err != nil {
		return res, err
	}
	if branch == "" {
		branch = cur
	}
	if branch != cur {
		return res, fmt.Errorf("current branch is %s; check out %s before pushing it", cur, branch)
	}
	res.Branch = branch
	if strings.ContainsAny(remote+branch, " \t\n:") || strings.HasPrefix(remote, "-") || strings.HasPrefix(branch, "-") {
		return res, errors.New("invalid remote or branch name")
	}
	if _, _, err := g.run(ctx, repo, "fetch", remote); err != nil {
		return res, err
	}
	remoteRef := "refs/remotes/" + remote + "/" + branch
	if out, _, err := g.run(ctx, repo, "rev-parse", "--verify", "--quiet", remoteRef); err == nil {
		res.From = strings.TrimSpace(out)
		if _, _, err := g.run(ctx, repo, "rebase", "--autostash", remote+"/"+branch); err != nil {
			res.Conflict = true
			res.Conflicted = g.conflicted(ctx, repo)
			_, _, _ = g.run(ctx, repo, "rebase", "--abort")
			res.To = g.head(ctx, repo)
			res.Message = "rebase onto " + remote + "/" + branch + " hit conflicts and was aborted; nothing was pushed. Resolve by hand (git_sync abort_on_conflict=false) or ask Aaryn."
			return res, nil //nolint:nilerr // a conflict is a reported outcome, not a failure
		}
	}
	res.To = g.head(ctx, repo)
	if res.From == res.To {
		res.Message = "nothing to push; " + branch + " matches " + remote + "/" + branch
		return res, nil
	}
	if res.From != "" {
		res.Commits = g.log(ctx, repo, res.From+".."+res.To)
	} else {
		res.Commits = g.log(ctx, repo, res.To)
	}
	_, stderr, err := g.run(ctx, repo, "push", remote, "HEAD:refs/heads/"+branch)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "non-fast-forward") || strings.Contains(msg, "fetch first") || strings.Contains(msg, "rejected") {
			res.Message = "push rejected (someone pushed meanwhile or a hook refused); run git_push_safe again. Never force-push."
			return res, nil
		}
		return res, fmt.Errorf("push: %w", err)
	}
	res.Pushed = true
	res.Message = strings.TrimSpace(lastLine(stderr))
	return res, nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// Summary is a one-line view for sandbox_status.
type Summary struct {
	Repo     string `json:"repo"`
	Branch   string `json:"branch"`
	Ahead    int    `json:"ahead"`
	Behind   int    `json:"behind"`
	Dirty    int    `json:"dirty"`
	Progress string `json:"in_progress,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Summarize returns a compact status without the file list.
func (g *Git) Summarize(ctx context.Context, repo string) Summary {
	st, err := g.Status(ctx, repo)
	if err != nil {
		return Summary{Repo: repo, Error: err.Error()}
	}
	return Summary{
		Repo: repo, Branch: st.Branch, Ahead: st.Ahead, Behind: st.Behind,
		Dirty: st.Staged + st.Unstaged + st.Untracked + st.Unmerged, Progress: st.InProgress,
	}
}
