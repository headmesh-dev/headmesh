// Package agents is the concurrency registry from plan §6.10: agents
// announce what they work on, heartbeat, and see who else is active in the
// same repo. With repo_lock = "advisory" or "enforce" registration also
// maintains .git/headmesh-agent.lock, which the pre-commit hook honours in
// enforce mode.
package agents

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/procutil"
)

// Timing.
const (
	ActiveWindow = 2 * time.Minute
	PruneAfter   = 30 * time.Minute
	LockFileName = "headmesh-agent.lock"
)

// ErrNotFound is returned for unknown agent IDs.
var ErrNotFound = errors.New("agent not registered")

// Agent is one registration.
type Agent struct {
	ID           string    `json:"id"`
	Agent        string    `json:"agent"`
	Task         string    `json:"task"`
	Repos        []string  `json:"repos"`
	Client       string    `json:"client"`
	Session      string    `json:"session,omitempty"`
	RegisteredAt time.Time `json:"registered_at"`
	HeartbeatAt  time.Time `json:"heartbeat_at"`
	Active       bool      `json:"active"`
}

// Lock is the JSON written to .git/headmesh-agent.lock.
type Lock struct {
	AgentID     string    `json:"agent_id"`
	Agent       string    `json:"agent"`
	Task        string    `json:"task"`
	HeartbeatAt time.Time `json:"heartbeat_at"`
	Mode        string    `json:"mode"`
}

// Registry persists registrations in one JSON file.
type Registry struct {
	Path     string
	LockMode string // off, advisory, enforce
	Now      func() time.Time
	mu       sync.Mutex
}

type file struct {
	Agents []Agent `json:"agents"`
}

func (r *Registry) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Registry) load() (file, error) {
	var f file
	err := procutil.ReadJSON(r.Path, &f)
	if errors.Is(err, os.ErrNotExist) {
		return file{}, nil
	}
	return f, err
}

func (r *Registry) save(f file) error {
	if err := os.MkdirAll(filepath.Dir(r.Path), 0o750); err != nil {
		return fmt.Errorf("registry dir: %w", err)
	}
	return procutil.WriteJSONAtomic(r.Path, f, 0o640)
}

func (r *Registry) refresh(f *file) {
	now := r.now()
	kept := f.Agents[:0]
	for _, a := range f.Agents {
		age := now.Sub(a.HeartbeatAt)
		if age > PruneAfter {
			continue
		}
		a.Active = age <= ActiveWindow
		kept = append(kept, a)
	}
	f.Agents = kept
}

func newID() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return "ag-" + hex.EncodeToString(b)
}

// RegisterResult tells the agent who else is active.
type RegisterResult struct {
	Agent  Agent    `json:"agent"`
	Others []Agent  `json:"others_active"`
	Notice string   `json:"notice"`
	Locks  []string `json:"lock_conflicts,omitempty"`
}

// Register adds an agent. repos must be absolute paths.
func (r *Registry) Register(agent, task string, repos []string, client, session string) (RegisterResult, error) {
	if strings.TrimSpace(agent) == "" {
		return RegisterResult{}, errors.New("agent is required (e.g. claude-runner, codex-ssh)")
	}
	for _, p := range repos {
		if !filepath.IsAbs(p) {
			return RegisterResult{}, fmt.Errorf("repo %q must be an absolute path", p)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	f, err := r.load()
	if err != nil {
		return RegisterResult{}, err
	}
	r.refresh(&f)
	now := r.now().UTC()
	a := Agent{
		ID: newID(), Agent: agent, Task: task, Repos: slices.Clone(repos), Client: client, Session: session,
		RegisteredAt: now, HeartbeatAt: now, Active: true,
	}
	f.Agents = append(f.Agents, a)
	if err := r.save(f); err != nil {
		return RegisterResult{}, err
	}
	res := RegisterResult{Agent: a, Others: others(f.Agents, a)}
	res.Locks = r.writeLocks(a)
	res.Notice = Notice(res.Others)
	return res, nil
}

func others(all []Agent, me Agent) []Agent {
	out := []Agent{}
	for _, o := range all {
		if o.ID == me.ID || !o.Active {
			continue
		}
		for _, repo := range o.Repos {
			if slices.Contains(me.Repos, repo) {
				out = append(out, o)
				break
			}
		}
	}
	return out
}

// Notice renders the banner line, e.g. "1 other agent active in /x (claude-runner, task …)".
func Notice(others []Agent) string {
	if len(others) == 0 {
		return "no other agents active in these repos"
	}
	parts := make([]string, 0, len(others))
	for _, o := range others {
		parts = append(parts, fmt.Sprintf("%s in %s (task %q)", o.Agent, strings.Join(o.Repos, ", "), o.Task))
	}
	word := "agents"
	if len(others) == 1 {
		word = "agent"
	}
	return fmt.Sprintf("%d other %s active: %s. Stick to your own files, commit small, pull --rebase before pushing, and ask Aaryn before overwriting changes you didn't make.",
		len(others), word, strings.Join(parts, "; "))
}

// Heartbeat refreshes an agent and optionally updates its task.
func (r *Registry) Heartbeat(id, task string) (RegisterResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, err := r.load()
	if err != nil {
		return RegisterResult{}, err
	}
	r.refresh(&f)
	i := slices.IndexFunc(f.Agents, func(a Agent) bool { return a.ID == id })
	if i < 0 {
		return RegisterResult{}, fmt.Errorf("%w: %s (register again)", ErrNotFound, id)
	}
	f.Agents[i].HeartbeatAt = r.now().UTC()
	f.Agents[i].Active = true
	if task != "" {
		f.Agents[i].Task = task
	}
	if err := r.save(f); err != nil {
		return RegisterResult{}, err
	}
	res := RegisterResult{Agent: f.Agents[i], Others: others(f.Agents, f.Agents[i])}
	res.Locks = r.writeLocks(f.Agents[i])
	res.Notice = Notice(res.Others)
	return res, nil
}

// Unregister removes an agent and releases its locks.
func (r *Registry) Unregister(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, err := r.load()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(f.Agents, func(a Agent) bool { return a.ID == id })
	if i < 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	a := f.Agents[i]
	f.Agents = slices.Delete(f.Agents, i, i+1)
	r.releaseLocks(a)
	return r.save(f)
}

// List returns live registrations, optionally only those touching repo.
func (r *Registry) List(repo string) ([]Agent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, err := r.load()
	if err != nil {
		return nil, err
	}
	r.refresh(&f)
	out := []Agent{}
	for _, a := range f.Agents {
		if repo == "" || slices.Contains(a.Repos, repo) {
			out = append(out, a)
		}
	}
	return out, nil
}

// ActiveIn returns active agents in repo other than exceptID.
func (r *Registry) ActiveIn(repo, exceptID string) []Agent {
	list, err := r.List(repo)
	if err != nil {
		return nil
	}
	out := []Agent{}
	for _, a := range list {
		if a.Active && a.ID != exceptID {
			out = append(out, a)
		}
	}
	return out
}

func lockPath(repo string) string {
	gitDir := filepath.Join(repo, ".git")
	if fi, err := os.Stat(gitDir); err != nil || !fi.IsDir() {
		return ""
	}
	return filepath.Join(gitDir, LockFileName)
}

// writeLocks takes or refreshes .git/headmesh-agent.lock in each repo and
// returns the repos where another live agent holds it.
func (r *Registry) writeLocks(a Agent) []string {
	if r.LockMode == "" || r.LockMode == "off" {
		return nil
	}
	var conflicts []string
	for _, repo := range a.Repos {
		p := lockPath(repo)
		if p == "" {
			continue
		}
		var cur Lock
		if err := procutil.ReadJSON(p, &cur); err == nil && cur.AgentID != a.ID && r.now().Sub(cur.HeartbeatAt) <= ActiveWindow {
			conflicts = append(conflicts, fmt.Sprintf("%s: held by %s (%s, task %q)", repo, cur.AgentID, cur.Agent, cur.Task))
			continue
		}
		_ = procutil.WriteJSONAtomic(p, Lock{AgentID: a.ID, Agent: a.Agent, Task: a.Task, HeartbeatAt: a.HeartbeatAt, Mode: r.LockMode}, 0o644)
	}
	return conflicts
}

func (r *Registry) releaseLocks(a Agent) {
	for _, repo := range a.Repos {
		p := lockPath(repo)
		if p == "" {
			continue
		}
		var cur Lock
		if err := procutil.ReadJSON(p, &cur); err == nil && cur.AgentID == a.ID {
			_ = os.Remove(p)
		}
	}
}
