package agents

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/procutil"
)

func TestRegistryFlow(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &Registry{Path: filepath.Join(t.TempDir(), "agents.json"), LockMode: "enforce", Now: func() time.Time { return now }}

	a, err := r.Register("claude-runner", "fix tests", []string{repo}, "runner", "cse_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Others) != 0 || !strings.Contains(a.Notice, "no other agents") || len(a.Locks) != 0 {
		t.Fatalf("first register %+v", a)
	}
	var lock Lock
	if err := procutil.ReadJSON(filepath.Join(repo, ".git", LockFileName), &lock); err != nil || lock.AgentID != a.Agent.ID {
		t.Fatalf("lock %+v %v", lock, err)
	}

	b, err := r.Register("codex-ssh", "docs", []string{repo, "/elsewhere"}, "ssh", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Others) != 1 || b.Others[0].ID != a.Agent.ID || !strings.Contains(b.Notice, "1 other agent active: claude-runner") {
		t.Fatalf("second register %+v", b)
	}
	if len(b.Locks) != 1 || !strings.Contains(b.Locks[0], "held by "+a.Agent.ID) {
		t.Fatalf("lock conflict not reported: %+v", b.Locks)
	}

	// a goes quiet; after the active window b can take the lock.
	now = now.Add(3 * time.Minute)
	hb, err := r.Heartbeat(b.Agent.ID, "docs v2")
	if err != nil {
		t.Fatal(err)
	}
	if len(hb.Others) != 0 || hb.Agent.Task != "docs v2" || len(hb.Locks) != 0 {
		t.Fatalf("heartbeat %+v", hb)
	}
	if err := procutil.ReadJSON(filepath.Join(repo, ".git", LockFileName), &lock); err != nil || lock.AgentID != b.Agent.ID {
		t.Fatalf("lock not taken over: %+v", lock)
	}
	list, _ := r.List(repo)
	if len(list) != 2 || list[0].Active {
		t.Fatalf("list %+v", list)
	}
	if got := r.ActiveIn(repo, ""); len(got) != 1 || got[0].ID != b.Agent.ID {
		t.Fatalf("active in %+v", got)
	}

	if err := r.Unregister(b.Agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", LockFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("lock not released")
	}
	if err := r.Unregister(b.Agent.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double unregister %v", err)
	}

	// Stale registrations are pruned.
	now = now.Add(PruneAfter + time.Minute)
	if list, _ := r.List(""); len(list) != 0 {
		t.Fatalf("not pruned: %+v", list)
	}
	if _, err := r.Heartbeat(a.Agent.ID, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("heartbeat after prune: %v", err)
	}
}

func TestRegisterValidation(t *testing.T) {
	r := &Registry{Path: filepath.Join(t.TempDir(), "a.json")}
	if _, err := r.Register("", "t", nil, "", ""); err == nil {
		t.Error("empty agent accepted")
	}
	if _, err := r.Register("x", "t", []string{"relative"}, "", ""); err == nil {
		t.Error("relative repo accepted")
	}
	// Lock mode off writes no lock files.
	repo := t.TempDir()
	_ = os.Mkdir(filepath.Join(repo, ".git"), 0o755)
	if _, err := r.Register("x", "t", []string{repo}, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", LockFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Error("lock written with lock mode off")
	}
}
