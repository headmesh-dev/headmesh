package acceptance

import (
	"context"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/headmesh-dev/headmesh/sandbox/config"
	"github.com/headmesh-dev/headmesh/sandbox/internal/testutil"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/authn"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/bootid"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/server"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/tmuxctl"
)

// Live run against a real sandbox (plan §18):
//
//	go test ./tests/acceptance -run TestFiveChecks -args -route=mcp \
//	    -url=http://asus-sandbox.mesh.xcelsior.ca:8780/mcp -token-file=$HOME/.config/headmesh/sandbox-token
//
// Without -url it uses the host's bind-mounted socket (<root>/run/sandboxd.sock).
var (
	flagRoute  = flag.String("route", "", "front door to test (mcp, ssh, exec-server, runner, tunnel)")
	flagURL    = flag.String("url", "", "MCP URL")
	flagToken  = flag.String("token-file", "", "bearer token file for -url")
	flagConfig = flag.String("config", "/mnt/storage/headmesh/sandbox/config/sandbox.toml", "sandbox.toml on the host")
	flagOut    = flag.String("out-dir", "", "results dir (default <root>/artifacts/acceptance)")
)

func TestFiveChecks(t *testing.T) {
	if *flagRoute == "" {
		t.Skip("live acceptance run: pass -args -route=mcp (see the comment above)")
	}
	cfg, err := config.Load(*flagConfig)
	if err != nil {
		t.Fatal(err)
	}
	p := Params{
		Route: *flagRoute, AgentUID: cfg.Agent.UID, Project: cfg.Acceptance.Project, File: cfg.Acceptance.File,
		DependencyCommand: cfg.Acceptance.DependencyCommand, MagicDNSName: cfg.Acceptance.MagicDNSName,
		AllowedTarget: cfg.Acceptance.AllowedTarget, DeniedTarget: cfg.Acceptance.DeniedTarget, Home: "/home/agent",
	}
	if sum, err := FileSHA256(filepath.Join(p.Project, p.File)); err == nil {
		p.ExpectedFileSHA256 = sum
	}
	dial := SocketDialer(filepath.Join(cfg.Root, "run", "sandboxd.sock"))
	if *flagURL != "" {
		tok, err := os.ReadFile(*flagToken)
		if err != nil {
			t.Fatal(err)
		}
		dial = HTTPDialer(*flagURL, strings.TrimSpace(string(tok)))
	}
	res, err := Run(context.Background(), dial, p, "all", "")
	if err != nil {
		t.Fatal(err)
	}
	out := *flagOut
	if out == "" {
		out = filepath.Join(cfg.Root, "artifacts", "acceptance")
	}
	path, err := Save(out, res)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("result %s", path)
	for _, c := range res.Checks {
		if !c.Pass {
			t.Errorf("check %d %s: %s", c.ID, c.Name, c.Details)
		}
	}
}

func TestTailnetAddress(t *testing.T) {
	cases := map[string]bool{
		"100.64.0.1      prod-hetzner.mesh.xcelsior.ca\n": true,
		"fd7a:115c:a1e0::1 x\n":                           true,
		"<prod-hetzner IPv4>    prod-hetzner.mesh.xcelsior.ca\n": false,
		"": false,
	}
	for in, want := range cases {
		if got := TailnetAddress(in); got != want {
			t.Errorf("TailnetAddress(%q) = %v", in, got)
		}
	}
}

// TestHarnessAgainstLocalSandboxd runs the harness end to end against an
// in-process sandboxd with real tmux. On a dev host the uid/OS and tailnet
// checks fail for known reasons; the test asserts the harness reports them
// correctly and that the file, dependency and reconnect mechanics work.
func TestHarnessAgainstLocalSandboxd(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	root := t.TempDir()
	project, home := filepath.Join(root, "project"), filepath.Join(root, "home")
	for _, d := range []string{project, home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(project, "README.md"), []byte("# demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := strings.NewReplacer(
		"\"/mnt/storage/projects/xcelsior\",\n  \"/mnt/storage/projects/headmesh\",", `"`+project+`",`,
		"\"aabiro/xcelsior\" = \"/mnt/storage/projects/xcelsior\"\n\"headmesh-dev/headmesh\" = \"/mnt/storage/projects/headmesh\"", `"acme/demo" = "`+project+`"`,
		`project = "/mnt/storage/projects/xcelsior"`, `project = "`+project+`"`,
	).Replace(string(config.Example()))
	cfg, err := config.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	tsBin := filepath.Join(root, "tailscale")
	if err := os.WriteFile(tsBin, []byte("#!/bin/sh\necho '{\"BackendState\":\"NeedsLogin\",\"Self\":null}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	tc := &tmuxctl.Client{SocketPath: filepath.Join(testutil.ShortDir(t), "tmux.sock")}
	t.Cleanup(func() { _ = tc.KillServer(context.Background()) })
	srv, err := server.New(server.Options{
		Config: cfg, Version: "test", Home: home, SessionsDir: filepath.Join(root, "sessions"), ArtifactsDir: filepath.Join(root, "artifacts"),
		LogsDir: filepath.Join(root, "logs"), RunDir: filepath.Join(root, "run"), TokensPath: filepath.Join(root, "tokens.json"), Tmux: tc,
		Boot:      func() (bootid.Identity, error) { return bootid.Identity{BootID: "b", Key: "b/1"}, nil },
		Tailscale: struct{ Bin, Socket string }{Bin: tsBin}, NoLogin: true, SkipPing: true, CacheDirs: []string{home},
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	if err := srv.EnsureTmux(context.Background()); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(testutil.ShortDir(t), "sandboxd.sock")
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	us := &http.Server{Handler: srv.UnixHandler(), ConnContext: authn.UnixConnContext, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = us.Serve(ln) }()
	defer func() { _ = us.Close() }()
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()

	p := Params{
		Route: "mcp", AgentUID: os.Getuid(), Project: project, File: "README.md", DependencyCommand: "echo dep-ok",
		MagicDNSName: "hm-no-such-host.invalid", Home: home,
	}
	sum, _ := FileSHA256(filepath.Join(project, "README.md"))
	p.ExpectedFileSHA256 = sum

	// Phase split over the socket, as an SSH or runner session would do it.
	state := filepath.Join(root, "state.json")
	r1, err := Run(context.Background(), SocketDialer(sock), p, "1", state)
	if err != nil {
		t.Fatal(err)
	}
	if len(r1.Checks) != 4 || !r1.Checks[1].Pass || !r1.Checks[2].Pass || r1.Checks[3].Pass {
		t.Fatalf("phase 1 checks %+v", r1.Checks)
	}
	if !strings.Contains(r1.Checks[0].Details, "VERSION_ID") {
		t.Fatalf("check 1 details %q", r1.Checks[0].Details)
	}
	r2, err := Run(context.Background(), SocketDialer(sock), p, "2", state)
	if err != nil {
		t.Fatal(err)
	}
	c5 := r2.Checks[0]
	if c5.ID != 5 || !strings.Contains(c5.Details, "is running") || !strings.Contains(c5.Details, "marker present") || !strings.Contains(c5.Details, "not enrolled") {
		t.Fatalf("check 5 %+v", c5)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("state file not removed after phase 2")
	}
	dir := filepath.Join(root, "results")
	if _, err := Save(dir, r1); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond) // distinct timestamps in file names
	r2.StartedAt = time.Now().UTC()
	if _, err := Save(dir, r2); err != nil {
		t.Fatal(err)
	}
	merged, problems, err := Verify(dir, "mcp", filepath.Join(project, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if merged.Total != 5 || len(problems) == 0 || strings.Contains(strings.Join(problems, "\n"), "check 2") {
		t.Fatalf("verify %+v %v", merged, problems)
	}

	// The HTTP path needs a bearer token; without one the dial fails.
	if _, err := HTTPDialer(hs.URL+"/mcp", "hms_wrong")(context.Background()); err == nil {
		t.Fatal("HTTP dial with a bad token succeeded")
	}
}
