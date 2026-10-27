package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/headmesh-dev/headmesh/sandbox/config"
	"github.com/headmesh-dev/headmesh/sandbox/internal/testutil"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/audit"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/authn"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/bootid"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/tmuxctl"
)

type fixture struct {
	srv       *Server
	http      *httptest.Server
	sock      string
	project   string
	home      string
	logs      string
	fullToken string
	readToken string
	tunnelTok string
	tmux      *tmuxctl.Client
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	root := t.TempDir()
	project := filepath.Join(root, "projects", "demo")
	home := filepath.Join(root, "home")
	for _, d := range []string{project, home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Point the example config at temp paths.
	src := string(config.Example())
	src = strings.ReplaceAll(src, `"/mnt/storage/projects/xcelsior",
  "/mnt/storage/projects/headmesh",`, `"`+project+`",`)
	src = strings.ReplaceAll(src, `"aabiro/xcelsior" = "/mnt/storage/projects/xcelsior"
"headmesh-dev/headmesh" = "/mnt/storage/projects/headmesh"`, `"acme/demo" = "`+project+`"`)
	src = strings.ReplaceAll(src, `project = "/mnt/storage/projects/xcelsior"`, `project = "`+project+`"`)
	src = strings.ReplaceAll(src, `rate_per_minute = 600`, `rate_per_minute = 1000`)
	cfg, err := config.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	tokens := filepath.Join(root, "config", "tokens.json")
	tf := &authn.File{Version: 1}
	full, _, _ := tf.Create("full", []string{authn.ScopeRead, authn.ScopeExec, authn.ScopeWrite}, authn.RouteMCP, time.Now())
	ro, _, _ := tf.Create("reader", []string{authn.ScopeRead}, authn.RouteMCP, time.Now())
	tun, _, _ := tf.Create("tunnel", []string{authn.ScopeRead, authn.ScopeExec}, authn.RouteTunnel, time.Now())
	if err := tf.Save(tokens); err != nil {
		t.Fatal(err)
	}
	tc := &tmuxctl.Client{SocketPath: filepath.Join(testutil.ShortDir(t), "tmux.sock")}
	t.Cleanup(func() { _ = tc.KillServer(context.Background()) })
	logs := filepath.Join(root, "logs")
	srv, err := New(Options{
		Config: cfg, Version: "test", Home: home,
		SessionsDir: filepath.Join(root, "sessions"), ArtifactsDir: filepath.Join(root, "artifacts"), LogsDir: logs,
		RunDir: filepath.Join(root, "run"), TokensPath: tokens, Tmux: tc,
		Boot:      func() (bootid.Identity, error) { return bootid.Identity{BootID: "b", Key: "b/1"}, nil },
		Tailscale: tailscaleStub(t), NoLogin: true, SkipPing: true, CacheDirs: []string{home},
		Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.EnsureTmux(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)

	sock := filepath.Join(testutil.ShortDir(t), "sandboxd.sock")
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	us := &http.Server{Handler: srv.UnixHandler(), ConnContext: authn.UnixConnContext, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = us.Serve(ln) }()
	t.Cleanup(func() { _ = us.Close() })
	return &fixture{srv: srv, http: hs, sock: sock, project: project, home: home, logs: logs, fullToken: full, readToken: ro, tunnelTok: tun, tmux: tc}
}

// tailscaleStub is a fake tailscale CLI that reports a logged-out node.
func tailscaleStub(t *testing.T) (ts struct{ Bin, Socket string }) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tailscale")
	script := "#!/bin/sh\ncase \"$*\" in *status*) echo '{\"BackendState\":\"NeedsLogin\",\"Self\":null}';; *) exit 1;; esac\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	ts.Bin = p
	return ts
}

type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

func (f *fixture) connect(t *testing.T, token, version string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	tr := &mcp.StreamableClientTransport{Endpoint: f.http.URL + "/mcp", HTTPClient: &http.Client{Transport: bearer{token, http.DefaultTransport}}}
	cs, err := client.Connect(context.Background(), tr, &mcp.ClientSessionOptions{ProtocolVersion: version})
	if err != nil {
		t.Fatalf("connect (%s): %v", version, err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func call[T any](t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (T, *mcp.CallToolResult) {
	t.Helper()
	var out T
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		return out, res
	}
	b, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("%s output %s: %v", name, b, err)
	}
	return out, res
}

func errText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

var allTools = []string{
	"agents_heartbeat", "agents_list", "agents_register", "agents_unregister",
	"artifact_get", "artifact_list", "artifact_put",
	"exec",
	"fs_delete", "fs_edit", "fs_list", "fs_move", "fs_read", "fs_search", "fs_stat", "fs_write",
	"git_push_safe", "git_status", "git_sync",
	"net_check",
	"process_list", "process_logs", "process_start", "process_stop",
	"sandbox_status",
	"session_create", "session_kill", "session_list", "session_read", "session_resize", "session_send",
}

func TestToolsListBothProtocols(t *testing.T) {
	f := newFixture(t)
	for _, v := range []string{"2026-07-28", "2025-11-25"} {
		cs := f.connect(t, f.fullToken, v)
		res, err := cs.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatalf("%s tools/list: %v", v, err)
		}
		var names []string
		for _, tool := range res.Tools {
			names = append(names, tool.Name)
			if tool.Annotations == nil || tool.Annotations.DestructiveHint == nil || tool.Description == "" {
				t.Errorf("%s: %s lacks annotations or description", v, tool.Name)
			}
		}
		slices.Sort(names)
		if !slices.Equal(names, allTools) {
			t.Fatalf("%s tools = %v", v, names)
		}
		if init := cs.InitializeResult(); init == nil || !strings.Contains(init.Instructions, "Active mode: Regular") {
			t.Fatalf("%s instructions missing the mode: %+v", v, init)
		}
	}
}

func TestExecSessionsFilesOverHTTP(t *testing.T) {
	f := newFixture(t)
	cs := f.connect(t, f.fullToken, "")

	ex, res := call[ExecOut](t, cs, "exec", map[string]any{"command": "id -u; echo $HEADMESH_MODE; pwd", "cwd": f.project})
	if res.IsError || ex.ExitCode != 0 || !strings.Contains(ex.Stdout, "regular\n"+f.project) || ex.TimeoutS != 120 {
		t.Fatalf("exec %+v %s", ex, errText(res))
	}

	w, res := call[map[string]any](t, cs, "fs_write", map[string]any{"path": filepath.Join(f.project, "notes.md"), "content": "alpha\nbeta\n"})
	if res.IsError || w["created"] != true {
		t.Fatalf("fs_write %v %s", w, errText(res))
	}
	_, res = call[map[string]any](t, cs, "fs_edit", map[string]any{"path": filepath.Join(f.project, "notes.md"), "old_string": "beta", "new_string": "gamma"})
	if res.IsError {
		t.Fatalf("fs_edit %s", errText(res))
	}
	rd, _ := call[map[string]any](t, cs, "fs_read", map[string]any{"path": filepath.Join(f.project, "notes.md")})
	if rd["content"] != "     1\talpha\n     2\tgamma\n" {
		t.Fatalf("fs_read %v", rd)
	}
	ls, _ := call[map[string]any](t, cs, "fs_list", map[string]any{"path": f.project})
	if len(ls["entries"].([]any)) != 1 {
		t.Fatalf("fs_list %v", ls)
	}
	sr, _ := call[map[string]any](t, cs, "fs_search", map[string]any{"pattern": "gamma", "path": f.project})
	if sr["count"].(float64) != 1 {
		t.Fatalf("fs_search %v", sr)
	}

	sess, res := call[map[string]any](t, cs, "session_create", map[string]any{"name": "marker", "shell": []string{"bash", "--norc", "--noprofile"}, "cwd": f.project})
	if res.IsError {
		t.Fatalf("session_create %s", errText(res))
	}
	id := sess["id"].(string)
	if _, res = call[map[string]any](t, cs, "session_send", map[string]any{"id": id, "input": "echo marker-$((6*7)) > marker.txt; cat marker.txt", "enter": true}); res.IsError {
		t.Fatalf("session_send %s", errText(res))
	}
	var got string
	var off float64
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline) && !strings.Contains(got, "marker-42\n"); {
		r, res := call[map[string]any](t, cs, "session_read", map[string]any{"id": id, "offset": off, "wait_ms": 1000})
		if res.IsError {
			t.Fatalf("session_read %s", errText(res))
		}
		got += r["data"].(string)
		off = r["next_offset"].(float64)
	}
	if !strings.Contains(got, "marker-42\n") {
		t.Fatalf("session output %q", got)
	}

	// Disconnect and reconnect: the session is still running.
	_ = cs.Close()
	cs2 := f.connect(t, f.fullToken, "2025-11-25")
	list, _ := call[SessionListOut](t, cs2, "session_list", nil)
	if len(list.Sessions) != 1 || list.Sessions[0].State != "running" || list.Counts["running"] != 1 {
		t.Fatalf("after reconnect %+v", list)
	}
	st, _ := call[map[string]any](t, cs2, "sandbox_status", nil)
	if st["mode"] != "regular" || st["sessions"].(map[string]any)["running"].(float64) != 1 {
		t.Fatalf("status %v", st)
	}
	if _, res = call[map[string]any](t, cs2, "session_kill", map[string]any{"id": id, "remove": true}); res.IsError {
		t.Fatalf("session_kill %s", errText(res))
	}

	art, res := call[map[string]any](t, cs2, "artifact_put", map[string]any{"name": "result.json", "content": `{"ok":true}`})
	if res.IsError || !strings.Contains(art["url"].(string), "/artifacts/") {
		t.Fatalf("artifact_put %v %s", art, errText(res))
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, strings.Replace(art["url"].(string), "http://asus-sandbox.mesh.xcelsior.ca:8780", f.http.URL, 1), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != `{"ok":true}` {
		t.Fatalf("signed download %d %q", resp.StatusCode, body)
	}

	reg, res := call[map[string]any](t, cs2, "agents_register", map[string]any{"agent": "test", "task": "acceptance", "repos": []string{"demo"}})
	if res.IsError || reg["agent"].(map[string]any)["repos"].([]any)[0] != f.project {
		t.Fatalf("agents_register %v %s", reg, errText(res))
	}
}

func TestResourcesAndPrompt(t *testing.T) {
	f := newFixture(t)
	cs := f.connect(t, f.readToken, "")
	for _, uri := range []string{"sandbox://status", "sandbox://mounts", "sandbox://guidelines"} {
		r, err := cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: uri})
		if err != nil || len(r.Contents) != 1 || r.Contents[0].Text == "" {
			t.Fatalf("%s: %+v %v", uri, r, err)
		}
	}
	p, err := cs.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: "multi-agent-protocol"})
	if err != nil || len(p.Messages) != 1 {
		t.Fatalf("prompt %+v %v", p, err)
	}
	if _, err := cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "sandbox://sessions/s-000000000000/log"}); err == nil {
		t.Fatal("missing session log should fail")
	}
}

func TestScopesAndRouteLimits(t *testing.T) {
	f := newFixture(t)
	ro := f.connect(t, f.readToken, "")
	_, res := call[map[string]any](t, ro, "exec", map[string]any{"command": "true"})
	if !res.IsError || !strings.Contains(errText(res), "needs scope sandbox:exec") {
		t.Fatalf("read-only exec: %s", errText(res))
	}
	tun := f.connect(t, f.tunnelTok, "")
	ex, res := call[ExecOut](t, tun, "exec", map[string]any{"command": "true", "timeout_s": 3000})
	if res.IsError || ex.TimeoutS != 110 || !strings.Contains(ex.Note, "lowered to 110") {
		t.Fatalf("tunnel exec %+v %s", ex, errText(res))
	}
	_, res = call[map[string]any](t, tun, "fs_write", map[string]any{"path": "/tmp/x", "content": "x"})
	if !res.IsError {
		t.Fatal("tunnel token without write scope wrote a file")
	}

	// The audit log recorded the denial without the arguments.
	fh, err := os.Open(filepath.Join(f.logs, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fh.Close() }()
	var denied int
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		var e audit.Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(sc.Text(), `"command"`) {
			t.Fatal("audit log contains raw arguments")
		}
		if e.Result == audit.ResultDenied {
			denied++
		}
	}
	if denied != 2 {
		t.Fatalf("denied entries = %d", denied)
	}
}

func TestHTTPAuthAndOrigin(t *testing.T) {
	f := newFixture(t)
	post := func(token, origin string) (int, http.Header) {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, f.http.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode, resp.Header
	}
	if code, h := post("", ""); code != http.StatusUnauthorized || !strings.Contains(h.Get("WWW-Authenticate"), "resource_metadata=") {
		t.Fatalf("no token: %d %q", code, h.Get("WWW-Authenticate"))
	}
	if code, _ := post(f.fullToken, "https://evil.example"); code != http.StatusForbidden {
		t.Fatalf("bad origin: %d", code)
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, f.http.URL+"/.well-known/oauth-protected-resource/mcp", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&meta)
	_ = resp.Body.Close()
	if meta["resource"] != "http://asus-sandbox.mesh.xcelsior.ca:8780/mcp" {
		t.Fatalf("metadata %v", meta)
	}
}

func unixClient(sock string) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}}
}

func TestUnixSocketAPI(t *testing.T) {
	f := newFixture(t)
	c := unixClient(f.sock)
	do := func(method, path, body string) (int, map[string]any) {
		req, _ := http.NewRequestWithContext(context.Background(), method, "http://sandboxd"+path, bytes.NewBufferString(body))
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}
	code, h := do(http.MethodGet, "/v1/health", "")
	if code != 200 || h["ok"] != true {
		t.Fatalf("health %d %v", code, h)
	}
	code, out := do(http.MethodPost, "/v1/call/exec", `{"command":"echo via-unix"}`)
	if code != 200 || out["result"].(map[string]any)["stdout"] != "via-unix\n" {
		t.Fatalf("exec %d %v", code, out)
	}
	code, out = do(http.MethodPost, "/v1/call/fs_stat", `{"path":"`+f.project+`","bogus":1}`)
	if code != 400 || !strings.Contains(out["error"].(string), "unknown field") {
		t.Fatalf("unknown field %d %v", code, out)
	}
	code, _ = do(http.MethodPost, "/v1/call/nope", `{}`)
	if code != 404 {
		t.Fatalf("unknown tool %d", code)
	}
	code, out = do(http.MethodGet, "/v1/status", "")
	if code != 200 || out["tailnet"].(map[string]any)["backend_state"] != "NeedsLogin" {
		t.Fatalf("status %d %v", code, out)
	}
	code, out = do(http.MethodGet, "/v1/tools", "")
	if code != 200 || len(out["tools"].([]any)) != len(allTools) {
		t.Fatalf("tools %d %v", code, out)
	}

	// MCP over the unix socket needs no token.
	client := mcp.NewClient(&mcp.Implementation{Name: "cli", Version: "1"}, nil)
	cs, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: "http://sandboxd/mcp", HTTPClient: c}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	ex, res := call[ExecOut](t, cs, "exec", map[string]any{"command": "echo mcp-unix"})
	if res.IsError || ex.Stdout != "mcp-unix\n" {
		t.Fatalf("unix mcp exec %+v %s", ex, errText(res))
	}
}
