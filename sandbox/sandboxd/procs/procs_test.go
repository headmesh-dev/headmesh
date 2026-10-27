package procs

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/bootid"
)

func newManager(t *testing.T) (*Manager, *atomic.Value) {
	t.Helper()
	var boot atomic.Value
	boot.Store(bootid.Identity{Key: "b/1", ContainerStarted: time.Unix(1700000000, 0)})
	return &Manager{
		Dir:        filepath.Join(t.TempDir(), "procs"),
		Boot:       func() (bootid.Identity, error) { return boot.Load().(bootid.Identity), nil },
		DefaultCWD: t.TempDir(),
		NoLogin:    true,
	}, &boot
}

func waitFor(t *testing.T, m *Manager, id string, cond func(Info) bool) Info {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		in, err := m.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if cond(in) {
			return in
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not met: %+v", in)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestStartLogsExit(t *testing.T) {
	m, _ := newManager(t)
	in, err := m.Start(context.Background(), StartOpts{
		Command: `for i in 1 2 3; do echo line-$i; done; echo "$FOO $HEADMESH_PROCESS" >&2; exit 4`,
		Env:     map[string]string{"FOO": "bar"}, Client: "c",
	})
	if err != nil {
		t.Fatal(err)
	}
	if in.Name != "for" || in.PID == 0 {
		t.Fatalf("start info %+v", in)
	}
	done := waitFor(t, m, in.ID, func(i Info) bool { return i.State == StateExited })
	if done.ExitCode == nil || *done.ExitCode != 4 {
		t.Fatalf("exit %v", done.ExitCode)
	}
	logs, err := m.Logs(in.ID, 2, -1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if logs.Data != "line-3\nbar "+in.ID+"\n" || !logs.Truncated || !strings.Contains(logs.More, "since=0") {
		t.Fatalf("tail logs %+v", logs)
	}
	all, err := m.Logs(in.ID, 0, 0, 7)
	if err != nil {
		t.Fatal(err)
	}
	if all.Data != "line-1\n" || all.NextOffset != 7 || !strings.Contains(all.More, "since=7") {
		t.Fatalf("ranged logs %+v", all)
	}
	list, err := m.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("list %v %v", list, err)
	}
}

func TestStopEscalates(t *testing.T) {
	m, _ := newManager(t)
	ctx := context.Background()
	// The child ignores TERM; Stop must fall back to KILL.
	in, err := m.Start(ctx, StartOpts{Name: "stubborn", Command: `trap "" TERM; while :; do sleep 0.05; done`})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, in.ID, func(i Info) bool { return i.State == StateRunning })
	got, err := m.Stop(ctx, in.ID, "TERM", 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateExited || !strings.Contains(got.LostReason, "SIGKILL") {
		t.Fatalf("after stop %+v", got)
	}
}

func TestStopGraceful(t *testing.T) {
	m, _ := newManager(t)
	ctx := context.Background()
	in, err := m.Start(ctx, StartOpts{Command: "sleep 1000"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, in.ID, func(i Info) bool { return i.State == StateRunning })
	got, err := m.Stop(ctx, in.ID, "", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateExited || got.ExitCode == nil || *got.ExitCode != 143 {
		t.Fatalf("after stop %+v %v", got, got.ExitCode)
	}
}

func TestLostAfterRestart(t *testing.T) {
	m, boot := newManager(t)
	in, err := m.Start(context.Background(), StartOpts{Command: "sleep 1000"})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop(context.Background(), in.ID, "KILL", time.Second) //nolint:errcheck
	boot.Store(bootid.Identity{Key: "b/2", ContainerStarted: time.Unix(1700000100, 0)})
	got, err := m.Get(in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateLost || !strings.Contains(got.LostReason, "container restarted") {
		t.Fatalf("got %+v", got)
	}
}

func TestBadInput(t *testing.T) {
	m, _ := newManager(t)
	ctx := context.Background()
	if _, err := m.Start(ctx, StartOpts{Command: "  "}); err == nil {
		t.Error("empty command accepted")
	}
	if _, err := m.Start(ctx, StartOpts{Command: "true", CWD: "/nope"}); err == nil {
		t.Error("bad cwd accepted")
	}
	if _, err := m.Get("p-../../x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("bad id: %v", err)
	}
}
