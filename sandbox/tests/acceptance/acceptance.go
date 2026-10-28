// Package acceptance is the five-check harness from plan §6.15. The same
// checks run through every front door: over MCP from outside (route mcp),
// or inside the container through the sandboxd socket when an agent that
// arrived by SSH, the Claude runner, Codex exec-server or the OpenAI tunnel
// runs `headmesh-sandbox acceptance run --route <route>`. Results are JSON
// files that the host verifier cross-checks.
package acceptance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/headmesh-dev/headmesh/sandbox/internal/sockclient"
)

// Routes from plan §6.15.
var Routes = []string{"mcp", "ssh", "exec-server", "runner", "tunnel"}

// Conn calls sandboxd tools.
type Conn interface {
	Call(ctx context.Context, tool string, args map[string]any, out any) error
	Close() error
}

// Dialer opens a fresh connection (the reconnect in check 5 calls it twice).
type Dialer func(ctx context.Context) (Conn, error)

// Params come from sandbox.toml [acceptance].
type Params struct {
	Route              string
	AgentUID           int
	Project            string
	File               string
	DependencyCommand  string
	MagicDNSName       string
	AllowedTarget      string
	DeniedTarget       string
	ExpectedFileSHA256 string
	Home               string
}

// Check is one of the five.
type Check struct {
	ID      int            `json:"id"`
	Name    string         `json:"name"`
	Pass    bool           `json:"pass"`
	Details string         `json:"details"`
	Data    map[string]any `json:"data,omitempty"`
}

// Node is the tailnet identity seen during the run.
type Node struct {
	BackendState string   `json:"backend_state"`
	NodeID       string   `json:"node_id"`
	IPs          []string `json:"ips"`
	DNSName      string   `json:"dns_name"`
	BootKey      string   `json:"boot_key"`
}

// Result is written to artifacts/acceptance/<route>-<ts>.json.
type Result struct {
	Route      string    `json:"route"`
	Phase      string    `json:"phase"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Pass       bool      `json:"pass"`
	Passed     int       `json:"passed"`
	Total      int       `json:"total"`
	Checks     []Check   `json:"checks"`
	Node       Node      `json:"node"`
	Host       string    `json:"host,omitempty"`
}

// State carries check 5 across the disconnect when phases run separately.
type State struct {
	Route     string    `json:"route"`
	SessionID string    `json:"session_id"`
	Marker    string    `json:"marker"`
	Node      Node      `json:"node"`
	CreatedAt time.Time `json:"created_at"`
}

type execOut struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	TimedOut bool   `json:"timed_out"`
}

func run(ctx context.Context, c Conn, cmd string, timeout int) (execOut, error) {
	var o execOut
	err := c.Call(ctx, "exec", map[string]any{"command": cmd, "timeout_s": timeout}, &o)
	return o, err
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func node(ctx context.Context, c Conn) (Node, error) {
	var st struct {
		BootKey string `json:"boot_key"`
		Tailnet struct {
			BackendState string   `json:"backend_state"`
			NodeID       string   `json:"node_id"`
			IPs          []string `json:"ips"`
			DNSName      string   `json:"dns_name"`
		} `json:"tailnet"`
	}
	if err := c.Call(ctx, "sandbox_status", nil, &st); err != nil {
		return Node{}, err
	}
	return Node{BackendState: st.Tailnet.BackendState, NodeID: st.Tailnet.NodeID, IPs: st.Tailnet.IPs, DNSName: st.Tailnet.DNSName, BootKey: st.BootKey}, nil
}

var versionIDRe = regexp.MustCompile(`(?m)^VERSION_ID="?([0-9.]+)"?`)

// checksOneToFour runs the stateless checks.
func checksOneToFour(ctx context.Context, c Conn, p Params) []Check {
	var cs []Check

	// 1. Run a command.
	ch := Check{ID: 1, Name: "run a command", Data: map[string]any{}}
	o, err := run(ctx, c, "id -u; uname -a; cat /etc/os-release", 60)
	if err != nil {
		ch.Details = err.Error()
	} else {
		lines := strings.SplitN(o.Stdout, "\n", 3)
		uid, _ := strconv.Atoi(strings.TrimSpace(lines[0]))
		ver := ""
		if m := versionIDRe.FindStringSubmatch(o.Stdout); m != nil {
			ver = m[1]
		}
		ch.Data["uid"], ch.Data["version_id"] = uid, ver
		if len(lines) > 1 {
			ch.Data["uname"] = strings.TrimSpace(lines[1])
		}
		ch.Pass = o.ExitCode == 0 && uid == p.AgentUID && ver == "26.04"
		ch.Details = fmt.Sprintf("id -u = %d (want %d), VERSION_ID = %s (want 26.04)", uid, p.AgentUID, ver)
	}
	cs = append(cs, ch)

	// 2. Read a real project file.
	ch = Check{ID: 2, Name: "read a real project file", Data: map[string]any{}}
	path := filepath.Join(p.Project, p.File)
	o, err = run(ctx, c, "sha256sum -- "+quote(path), 60)
	if err != nil {
		ch.Details = err.Error()
	} else {
		sum, _, _ := strings.Cut(strings.TrimSpace(o.Stdout), " ")
		ch.Data["path"], ch.Data["sha256"] = path, sum
		ch.Pass = o.ExitCode == 0 && len(sum) == 64
		ch.Details = "sha256 " + sum
		if p.ExpectedFileSHA256 != "" {
			ch.Pass = ch.Pass && sum == p.ExpectedFileSHA256
			ch.Details += " (host: " + p.ExpectedFileSHA256 + ")"
		} else {
			ch.Details += " (the host verifier compares it with the host's copy)"
		}
	}
	cs = append(cs, ch)

	// 3. Use a project dependency.
	ch = Check{ID: 3, Name: "use a project dependency", Data: map[string]any{}}
	o, err = run(ctx, c, "cd "+quote(p.Project)+" && "+p.DependencyCommand, 900)
	if err != nil {
		ch.Details = err.Error()
	} else {
		ch.Pass = o.ExitCode == 0 && !o.TimedOut
		ch.Data["stdout"] = strings.TrimSpace(tailStr(o.Stdout, 400))
		ch.Details = fmt.Sprintf("exit %d: %s", o.ExitCode, strings.TrimSpace(tailStr(o.Stdout+o.Stderr, 300)))
	}
	cs = append(cs, ch)

	// 4. Reach a private service by MagicDNS name, and fail to reach a denied one.
	ch = Check{ID: 4, Name: "reach a private service by MagicDNS name", Data: map[string]any{}}
	var detail []string
	pass := true
	o, err = run(ctx, c, "getent hosts "+quote(p.MagicDNSName), 30)
	switch {
	case err != nil || o.ExitCode != 0:
		pass = false
		detail = append(detail, "getent hosts "+p.MagicDNSName+" failed")
	case !TailnetAddress(o.Stdout):
		// A public DNS record for the same name doesn't prove MagicDNS works.
		pass = false
		ch.Data["getent"] = strings.TrimSpace(o.Stdout)
		detail = append(detail, "getent resolved "+p.MagicDNSName+" to a non-tailnet address ("+strings.Fields(o.Stdout)[0]+"): that's public DNS, not MagicDNS")
	default:
		ch.Data["getent"] = strings.TrimSpace(o.Stdout)
		detail = append(detail, "MagicDNS ok: "+strings.TrimSpace(o.Stdout))
	}
	var allowed, denied struct {
		Summary string `json:"summary"`
		TCP     *struct {
			OK bool `json:"ok"`
		} `json:"tcp"`
	}
	if p.AllowedTarget != "" {
		if err := c.Call(ctx, "net_check", map[string]any{"target": p.AllowedTarget}, &allowed); err != nil || allowed.TCP == nil || !allowed.TCP.OK {
			pass = false
			detail = append(detail, "allowed "+p.AllowedTarget+": "+allowed.Summary+errText(err))
		} else {
			detail = append(detail, "allowed "+p.AllowedTarget+" reachable")
		}
	}
	if p.DeniedTarget != "" {
		if err := c.Call(ctx, "net_check", map[string]any{"target": p.DeniedTarget}, &denied); err != nil || denied.TCP == nil {
			pass = false
			detail = append(detail, "denied probe "+p.DeniedTarget+" could not run"+errText(err))
		} else if denied.TCP.OK {
			pass = false
			detail = append(detail, "denied "+p.DeniedTarget+" is REACHABLE (policy or firewall too open)")
		} else {
			detail = append(detail, "denied "+p.DeniedTarget+" blocked as expected")
		}
	}
	ch.Pass, ch.Details = pass, strings.Join(detail, "; ")
	cs = append(cs, ch)
	return cs
}

// TailnetAddress reports whether getent output starts with a tailnet
// address (100.64.0.0/10 or Tailscale's fd7a:115c:a1e0::/48).
func TailnetAddress(getent string) bool {
	f := strings.Fields(getent)
	if len(f) == 0 {
		return false
	}
	ip, err := netip.ParseAddr(f[0])
	if err != nil {
		return false
	}
	return netip.MustParsePrefix("100.64.0.0/10").Contains(ip) || netip.MustParsePrefix("fd7a:115c:a1e0::/48").Contains(ip)
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return " (" + err.Error() + ")"
}

func tailStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// startSession is check 5, part 1.
func startSession(ctx context.Context, c Conn, p Params) (State, error) {
	n, err := node(ctx, c)
	if err != nil {
		return State{}, err
	}
	stamp := time.Now().UTC().Format("20060102T150405")
	marker := filepath.Join(p.Home, ".hm-acceptance-"+p.Route+"-"+stamp)
	var info struct {
		ID string `json:"id"`
	}
	cmd := "echo $$ > " + quote(marker) + " && exec sleep 86400"
	if err := c.Call(ctx, "session_create", map[string]any{"name": "acceptance-" + p.Route, "cwd": p.Home, "shell": []string{"bash", "-c", cmd}}, &info); err != nil {
		return State{}, fmt.Errorf("session_create: %w", err)
	}
	return State{Route: p.Route, SessionID: info.ID, Marker: marker, Node: n, CreatedAt: time.Now().UTC()}, nil
}

// verifySession is check 5, part 2 (on a new connection).
func verifySession(ctx context.Context, c Conn, s State) Check {
	ch := Check{ID: 5, Name: "disconnect and reconnect", Data: map[string]any{"session_id": s.SessionID, "marker": s.Marker}}
	var list struct {
		Sessions []struct {
			ID         string `json:"id"`
			State      string `json:"state"`
			LostReason string `json:"lost_reason"`
		} `json:"sessions"`
	}
	var detail []string
	pass := true
	if err := c.Call(ctx, "session_list", nil, &list); err != nil {
		return Check{ID: 5, Name: ch.Name, Details: "session_list: " + err.Error()}
	}
	state := "missing"
	for _, x := range list.Sessions {
		if x.ID == s.SessionID {
			state = x.State
		}
	}
	ch.Data["state"] = state
	if state != "running" {
		pass = false
	}
	detail = append(detail, "session "+s.SessionID+" is "+state)
	o, err := run(ctx, c, "test -s "+quote(s.Marker)+" && cat "+quote(s.Marker), 30)
	if err != nil || o.ExitCode != 0 {
		pass = false
		detail = append(detail, "marker missing")
	} else {
		detail = append(detail, "marker present (pid "+strings.TrimSpace(o.Stdout)+")")
	}
	after, err := node(ctx, c)
	if err != nil {
		pass = false
		detail = append(detail, "status: "+err.Error())
	} else {
		ch.Data["node_before"], ch.Data["node_after"] = s.Node, after
		switch {
		case after.BackendState != "Running":
			pass = false
			detail = append(detail, "tailnet "+after.BackendState+" (not enrolled)")
		case after.NodeID != s.Node.NodeID || !slices.Equal(after.IPs, s.Node.IPs):
			pass = false
			detail = append(detail, fmt.Sprintf("node changed: %s %v → %s %v", s.Node.NodeID, s.Node.IPs, after.NodeID, after.IPs))
		default:
			detail = append(detail, fmt.Sprintf("node %s %v unchanged", after.NodeID, after.IPs))
		}
	}
	// Clean up the marker session.
	_ = c.Call(ctx, "session_kill", map[string]any{"id": s.SessionID, "signal": "KILL", "remove": true}, nil)
	_, _ = run(ctx, c, "rm -f "+quote(s.Marker), 10)
	ch.Pass, ch.Details = pass, strings.Join(detail, "; ")
	return ch
}

// Run executes a phase: "all" (create, disconnect, reconnect, verify in one
// process), "1" (checks 1–4 and start the session; saves stateFile) or "2"
// (verify the session from stateFile on a new connection).
func Run(ctx context.Context, dial Dialer, p Params, phase, stateFile string) (Result, error) {
	res := Result{Route: p.Route, Phase: phase, StartedAt: time.Now().UTC()}
	res.Host, _ = os.Hostname()
	c, err := dial(ctx)
	if err != nil {
		return res, fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = c.Close() }()
	if res.Node, err = node(ctx, c); err != nil {
		return res, fmt.Errorf("sandbox_status: %w", err)
	}
	switch phase {
	case "all", "":
		res.Phase = "all"
		res.Checks = checksOneToFour(ctx, c, p)
		st, err := startSession(ctx, c, p)
		if err != nil {
			res.Checks = append(res.Checks, Check{ID: 5, Name: "disconnect and reconnect", Details: err.Error()})
			break
		}
		_ = c.Close() // drop the client
		c2, err := dial(ctx)
		if err != nil {
			res.Checks = append(res.Checks, Check{ID: 5, Name: "disconnect and reconnect", Details: "reconnect: " + err.Error()})
			break
		}
		defer func() { _ = c2.Close() }()
		res.Checks = append(res.Checks, verifySession(ctx, c2, st))
	case "1":
		res.Checks = checksOneToFour(ctx, c, p)
		st, err := startSession(ctx, c, p)
		if err != nil {
			return res, err
		}
		b, _ := json.MarshalIndent(st, "", "  ")
		if err := os.WriteFile(stateFile, b, 0o600); err != nil {
			return res, fmt.Errorf("save state: %w", err)
		}
	case "2":
		var st State
		b, err := os.ReadFile(stateFile) //nolint:gosec // G304: harness state file
		if err != nil {
			return res, fmt.Errorf("phase 2 needs the state from phase 1: %w", err)
		}
		if err := json.Unmarshal(b, &st); err != nil {
			return res, fmt.Errorf("state: %w", err)
		}
		res.Checks = append(res.Checks, verifySession(ctx, c, st))
		_ = os.Remove(stateFile)
	default:
		return res, fmt.Errorf("phase %q must be all, 1 or 2", phase)
	}
	res.FinishedAt = time.Now().UTC()
	res.Total = len(res.Checks)
	for _, ch := range res.Checks {
		if ch.Pass {
			res.Passed++
		}
	}
	res.Pass = res.Passed == res.Total && res.Total > 0
	return res, nil
}

// Save writes the result as <dir>/<route>-<phase>-<ts>.json.
func Save(dir string, r Result) (string, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("results dir: %w", err)
	}
	name := fmt.Sprintf("%s-%s-%s.json", r.Route, r.Phase, r.StartedAt.Format("20060102T150405Z"))
	p := filepath.Join(dir, name)
	b, _ := json.MarshalIndent(r, "", "  ")
	if err := os.WriteFile(p, append(b, '\n'), 0o640); err != nil { //nolint:gosec // G306: results aren't secret
		return "", fmt.Errorf("write result: %w", err)
	}
	return p, nil
}

// SocketDialer connects with MCP over sandboxd's unix socket.
func SocketDialer(socket string) Dialer {
	return func(ctx context.Context) (Conn, error) {
		sc := sockclient.New(socket)
		return dialMCP(ctx, "http://sandboxd/mcp", sc.HTTPClient())
	}
}

// HTTPDialer connects with MCP over HTTP (the tailnet URL) with a bearer token.
func HTTPDialer(url, token string) Dialer {
	return func(ctx context.Context) (Conn, error) {
		return dialMCP(ctx, url, &http.Client{Transport: bearer{token: token, base: http.DefaultTransport}})
	}
}

type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r) //nolint:wrapcheck // transport passthrough
}

type mcpConn struct{ cs *mcp.ClientSession }

func dialMCP(ctx context.Context, url string, hc *http.Client) (Conn, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "headmesh-acceptance", Version: "1"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: hc}, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp connect %s: %w", url, err)
	}
	return &mcpConn{cs: cs}, nil
}

func (m *mcpConn) Call(ctx context.Context, tool string, args map[string]any, out any) error {
	if args == nil {
		args = map[string]any{}
	}
	res, err := m.cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return fmt.Errorf("%s: %w", tool, err)
	}
	if res.IsError {
		var b strings.Builder
		for _, c := range res.Content {
			if t, ok := c.(*mcp.TextContent); ok {
				b.WriteString(t.Text)
			}
		}
		return fmt.Errorf("%s: %s", tool, b.String())
	}
	if out == nil {
		return nil
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return fmt.Errorf("%s result: %w", tool, err)
	}
	return json.Unmarshal(raw, out) //nolint:wrapcheck // decode into caller type
}

func (m *mcpConn) Close() error { return m.cs.Close() } //nolint:wrapcheck // close

// Latest returns the newest result file for route in dir.
func Latest(dir, route string) (Result, string, error) {
	matches, _ := filepath.Glob(filepath.Join(dir, route+"-*.json"))
	if len(matches) == 0 {
		return Result{}, "", fmt.Errorf("no results for route %s in %s", route, dir)
	}
	slices.Sort(matches)
	p := matches[len(matches)-1]
	b, err := os.ReadFile(p) //nolint:gosec // G304: results dir
	if err != nil {
		return Result{}, p, fmt.Errorf("read %s: %w", p, err)
	}
	var r Result
	if err := json.Unmarshal(b, &r); err != nil {
		return r, p, fmt.Errorf("parse %s: %w", p, err)
	}
	return r, p, nil
}

// Verify cross-checks a result against the host: the file hash must match
// the host's copy, and the result must be complete. Phase-1 results are
// merged with the following phase-2 result for the same route.
func Verify(dir, route, hostFile string) (Result, []string, error) {
	r, _, err := Latest(dir, route)
	if err != nil {
		return r, nil, err
	}
	if r.Phase == "2" {
		matches, _ := filepath.Glob(filepath.Join(dir, route+"-1-*.json"))
		slices.Sort(matches)
		if len(matches) > 0 {
			b, _ := os.ReadFile(matches[len(matches)-1])
			var p1 Result
			if json.Unmarshal(b, &p1) == nil {
				r.Checks = append(p1.Checks, r.Checks...)
			}
		}
	}
	var problems []string
	ids := map[int]bool{}
	for _, c := range r.Checks {
		ids[c.ID] = true
		if !c.Pass {
			problems = append(problems, fmt.Sprintf("check %d (%s) failed: %s", c.ID, c.Name, c.Details))
		}
		if c.ID == 2 && hostFile != "" {
			want, herr := FileSHA256(hostFile)
			got, _ := c.Data["sha256"].(string)
			switch {
			case herr != nil:
				problems = append(problems, "host file: "+herr.Error())
			case got != want:
				problems = append(problems, fmt.Sprintf("check 2: sandbox saw %s, host has %s", got, want))
			}
		}
	}
	for id := 1; id <= 5; id++ {
		if !ids[id] {
			problems = append(problems, fmt.Sprintf("check %d missing", id))
		}
	}
	r.Total, r.Passed = 0, 0
	for _, c := range r.Checks {
		r.Total++
		if c.Pass {
			r.Passed++
		}
	}
	r.Pass = len(problems) == 0
	return r, problems, nil
}

// FileSHA256 hashes a file.
func FileSHA256(p string) (string, error) {
	f, err := os.Open(p) //nolint:gosec // G304: configured project file
	if err != nil {
		return "", err //nolint:wrapcheck // shown as is
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err //nolint:wrapcheck // shown as is
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ErrNotConfigured means the harness has nothing to run against.
var ErrNotConfigured = errors.New("acceptance: no route configured")
