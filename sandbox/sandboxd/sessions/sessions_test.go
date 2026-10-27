package sessions

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/headmesh-dev/headmesh/sandbox/internal/testutil"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/bootid"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/tmuxctl"
)

// newManager starts a private tmux server (real tmux, its own socket).
func newManager(t *testing.T) (*Manager, *atomic.Value) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	sock := filepath.Join(testutil.ShortDir(t), "tmux.sock")
	tc := &tmuxctl.Client{SocketPath: sock, Session: "headmesh"}
	t.Cleanup(func() { _ = tc.KillServer(context.Background()) })
	var boot atomic.Value
	boot.Store(bootid.Identity{BootID: "b1", Key: "b1/100", ContainerStarted: time.Unix(1700000000, 0).UTC()})
	m := &Manager{
		Dir:          filepath.Join(t.TempDir(), "sessions"),
		Tmux:         tc,
		Boot:         func() (bootid.Identity, error) { return boot.Load().(bootid.Identity), nil },
		DefaultCWD:   t.TempDir(),
		DefaultShell: []string{"bash", "--norc", "--noprofile"},
		Env:          map[string]string{"PS1": "$ ", "HEADMESH_MODE": "regular"},
	}
	return m, &boot
}

// readUntil long-polls until the stripped output contains want.
func readUntil(t *testing.T, m *Manager, id, want string) (string, int64) {
	t.Helper()
	var all strings.Builder
	var off int64
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r, err := m.Read(context.Background(), id, ReadOpts{Offset: off, Wait: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		all.WriteString(r.Data)
		off = r.NextOffset
		if strings.Contains(all.String(), want) {
			return all.String(), off
		}
		if !r.Alive && !r.Truncated {
			break
		}
	}
	t.Fatalf("output never contained %q; got %q", want, all.String())
	return "", 0
}

func waitState(t *testing.T, m *Manager, id, state string) Info {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		in, err := m.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if in.State == state {
			return in
		}
		if time.Now().After(deadline) {
			t.Fatalf("session %s state %s, want %s (%+v)", id, in.State, state, in)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestInteractiveSession(t *testing.T) {
	m, _ := newManager(t)
	ctx := context.Background()
	in, err := m.Create(ctx, CreateOpts{Name: "build", Client: "test", Env: map[string]string{"MARKER": "m-123"}})
	if err != nil {
		t.Fatal(err)
	}
	if in.State != StateRunning || in.WindowID == "" || in.PanePID == 0 {
		t.Fatalf("created %+v", in)
	}
	if _, err := m.Send(ctx, in.ID, `echo "hello-$((40+2)) $MARKER $HEADMESH_SESSION $HEADMESH_ENTRYPOINT"`, true, nil); err != nil {
		t.Fatal(err)
	}
	out, off := readUntil(t, m, in.ID, "hello-42 m-123 "+in.ID+" mcp:test")
	if off == 0 || strings.Contains(out, "\x1b[") {
		t.Fatalf("offset %d / escapes left in %q", off, out)
	}

	// A read past the end waits, then returns nothing new.
	start := time.Now()
	r, err := m.Read(ctx, in.ID, ReadOpts{Offset: off + 1000, Wait: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 150*time.Millisecond || r.Data != "" {
		t.Fatalf("long poll returned early or with data: %+v", r)
	}

	// Screen capture and resize.
	r, err = m.Read(ctx, in.ID, ReadOpts{Offset: off, Screen: true})
	if err != nil || !strings.Contains(r.Screen, "hello-42") {
		t.Fatalf("screen %q, %v", r.Screen, err)
	}
	if in, err = m.Resize(ctx, in.ID, 120, 40); err != nil || in.Cols != 120 {
		t.Fatalf("resize: %+v %v", in, err)
	}

	if _, err := m.Send(ctx, in.ID, "", false, []string{"bad key!"}); err == nil {
		t.Fatal("bad key accepted")
	}
	if _, err := m.Send(ctx, in.ID, "exit 7", true, nil); err != nil {
		t.Fatal(err)
	}
	done := waitState(t, m, in.ID, StateExited)
	if done.ExitCode == nil || *done.ExitCode != 7 {
		t.Fatalf("exit code %v", done.ExitCode)
	}
	if _, err := m.Send(ctx, in.ID, "echo x", true, nil); err == nil {
		t.Fatal("send to exited session accepted")
	}
}

func TestKillAndRemove(t *testing.T) {
	m, _ := newManager(t)
	ctx := context.Background()
	in, err := m.Create(ctx, CreateOpts{Shell: []string{"sleep", "1000"}})
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, m, in.ID, StateRunning)
	got, err := m.Kill(ctx, in.ID, "TERM", false)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateExited || got.ExitCode == nil || *got.ExitCode != 143 {
		t.Fatalf("after TERM: %+v code=%v", got, got.ExitCode)
	}
	got, err = m.Kill(ctx, in.ID, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Removed {
		t.Fatalf("not marked removed: %+v", got)
	}
	if _, err := m.Kill(ctx, in.ID, "BOGUS", false); err == nil {
		t.Fatal("bogus signal accepted")
	}
}

func TestOutputBeforePipeIsKept(t *testing.T) {
	m, _ := newManager(t)
	in, err := m.Create(context.Background(), CreateOpts{Shell: []string{"bash", "-c", "echo first-line; sleep 0.2"}})
	if err != nil {
		t.Fatal(err)
	}
	readUntil(t, m, in.ID, "first-line")
	done := waitState(t, m, in.ID, StateExited)
	if done.ExitCode == nil || *done.ExitCode != 0 {
		t.Fatalf("exit %v", done.ExitCode)
	}
}

func TestLostAfterRestart(t *testing.T) {
	m, boot := newManager(t)
	ctx := context.Background()
	in, err := m.Create(ctx, CreateOpts{Shell: []string{"sleep", "1000"}})
	if err != nil {
		t.Fatal(err)
	}
	// tmux server restart: window gone, same container.
	if err := m.Tmux.KillServer(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := m.Get(ctx, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateLost || !strings.Contains(got.LostReason, "tmux server") {
		t.Fatalf("after tmux restart: %+v", got)
	}
	// Container restart: new boot identity.
	boot.Store(bootid.Identity{BootID: "b1", Key: "b1/999", ContainerStarted: time.Unix(1700000500, 0).UTC()})
	list, err := m.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].State != StateLost || !strings.Contains(list[0].LostReason, "container restarted at") {
		t.Fatalf("after container restart: %+v", list)
	}
	if s := Summary(list); s[StateLost] != 1 {
		t.Fatalf("summary %v", s)
	}
	if _, err := m.Send(ctx, in.ID, "x", false, nil); err == nil {
		t.Fatal("send to lost session accepted")
	}
}

func TestUnknownAndInvalidIDs(t *testing.T) {
	m, _ := newManager(t)
	for _, id := range []string{"", "s-zz", "../etc", "s-000000000000"} {
		if _, err := m.Get(context.Background(), id); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%q) = %v", id, err)
		}
	}
	if _, err := m.Create(context.Background(), CreateOpts{CWD: "/does/not/exist"}); err == nil {
		t.Fatal("bad cwd accepted")
	}
	if _, err := m.Create(context.Background(), CreateOpts{Env: map[string]string{"A=B": "x"}}); err == nil {
		t.Fatal("bad env name accepted")
	}
	if _, err := os.Stat(m.Dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}
