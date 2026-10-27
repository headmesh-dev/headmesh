package execx

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newRunner(t *testing.T) *Runner {
	t.Helper()
	return &Runner{OutputDir: filepath.Join(t.TempDir(), "exec"), DefaultCWD: t.TempDir(), NoLogin: true, KillGrace: 200 * time.Millisecond}
}

func TestRunBasics(t *testing.T) {
	r := newRunner(t)
	cwd := t.TempDir()
	res, err := r.Run(context.Background(), Request{
		Command: `echo out; echo err >&2; pwd; echo "$FOO $HEADMESH_ENTRYPOINT"; read line; echo "got $line"; exit 3`,
		CWD:     cwd, Env: map[string]string{"FOO": "bar"}, Timeout: 10 * time.Second, Stdin: "hi\n", Client: "c1",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "out\n" + cwd + "\nbar mcp:c1\ngot hi\n"
	if res.ExitCode != 3 || res.Stdout != want || res.Stderr != "err\n" || res.TimedOut {
		t.Fatalf("result %+v", res)
	}
	if res.StdoutTruncated || res.More != "" || !strings.HasPrefix(res.OutputRef, "exec/") {
		t.Fatalf("truncation fields %+v", res)
	}
	full, _ := os.ReadFile(res.StdoutPath)
	if string(full) != want {
		t.Fatalf("stdout file %q", full)
	}
}

func TestTruncation(t *testing.T) {
	r := newRunner(t)
	res, err := r.Run(context.Background(), Request{Command: `head -c 200000 /dev/zero | tr '\0' 'a'; echo END`, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if !res.StdoutTruncated || res.StdoutBytes != 200004 || len(res.Stdout) != TailBytes || !strings.HasSuffix(res.Stdout, "END\n") {
		t.Fatalf("truncated=%v bytes=%d len=%d", res.StdoutTruncated, res.StdoutBytes, len(res.Stdout))
	}
	if !strings.Contains(res.More, res.StdoutPath) {
		t.Fatalf("more hint %q", res.More)
	}
}

func TestTimeoutKillsGroup(t *testing.T) {
	r := newRunner(t)
	marker := filepath.Join(t.TempDir(), "child.pid")
	start := time.Now()
	res, err := r.Run(context.Background(), Request{
		Command: `sleep 1000 & echo $! > ` + marker + `; trap "" TERM; wait`,
		Timeout: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || res.ExitCode == 0 || time.Since(start) > 5*time.Second {
		t.Fatalf("result %+v after %s", res, time.Since(start))
	}
	pid, _ := os.ReadFile(marker)
	if _, err := os.Stat("/proc/" + strings.TrimSpace(string(pid))); err == nil {
		// Might be a zombie for a moment; check state.
		st, _ := os.ReadFile("/proc/" + strings.TrimSpace(string(pid)) + "/stat")
		if !strings.Contains(string(st), ") Z ") {
			t.Fatalf("background child %s survived the timeout", pid)
		}
	}
}

func TestBackgroundChildDoesNotBlock(t *testing.T) {
	r := newRunner(t)
	start := time.Now()
	res, err := r.Run(context.Background(), Request{Command: `(sleep 3; echo late) & echo early`, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second || res.Stdout != "early\n" {
		t.Fatalf("blocked on background child: %s %+v", time.Since(start), res)
	}
}

func TestCancel(t *testing.T) {
	r := newRunner(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := r.Run(ctx, Request{Command: "sleep 1000", Timeout: time.Minute}); err == nil {
		t.Fatal("expected cancellation error")
	}
}

func TestBadRequests(t *testing.T) {
	r := newRunner(t)
	ctx := context.Background()
	for _, req := range []Request{
		{Command: "", Timeout: time.Second},
		{Command: "true"},
		{Command: "true", Timeout: time.Second, CWD: "/no/such"},
		{Command: "true", Timeout: time.Second, Env: map[string]string{"A B": "x"}},
	} {
		if _, err := r.Run(ctx, req); err == nil {
			t.Errorf("accepted %+v", req)
		}
	}
}

func TestPrune(t *testing.T) {
	r := newRunner(t)
	r.Keep = 3
	for range 6 {
		if _, err := r.Run(context.Background(), Request{Command: "true", Timeout: time.Second}); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(r.OutputDir)
	if len(entries) != 3 {
		t.Fatalf("kept %d dirs", len(entries))
	}
}
