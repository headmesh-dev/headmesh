// Package status aggregates sandbox_status: mode, boot identity, tailnet,
// entry-point health, sessions, processes, agents, repos, GPU and caches.
// Every probe has its own deadline, so one slow probe yields a partial
// report instead of a hung call.
package status

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/agents"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/bootid"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/gitx"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/netcheck"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/procs"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/sessions"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/tmuxctl"
)

// Entry-point states.
const (
	StateUp       = "up"
	StateDown     = "down"
	StateDisabled = "disabled"
	StateUnknown  = "unknown"
)

// EntryPoint is one front door's health.
type EntryPoint struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	State   string `json:"state"`
	Healthy bool   `json:"healthy"`
	Detail  string `json:"detail,omitempty"`
	Hint    string `json:"hint,omitempty"`
}

// DNSCheck is what the dns-check oneshot writes to /run/headmesh/dns-check.json.
type DNSCheck struct {
	CheckedAt time.Time `json:"checked_at"`
	Status    string    `json:"status"` // ok, failed, skipped
	Name      string    `json:"name,omitempty"`
	Resolved  []string  `json:"resolved,omitempty"`
	IP        string    `json:"ip,omitempty"`
	IPPingOK  bool      `json:"ip_ping_ok"`
	Message   string    `json:"message"`
}

// GPU describes the device when gpu = "shared".
type GPU struct {
	Mode      string   `json:"mode"`
	Devices   []string `json:"devices,omitempty"`
	Processes []string `json:"processes,omitempty"`
	Error     string   `json:"error,omitempty"`
}

// Cache is one package cache volume.
type Cache struct {
	Path      string `json:"path"`
	UsedBytes int64  `json:"used_bytes"`
	FreeBytes uint64 `json:"fs_free_bytes"`
	Error     string `json:"error,omitempty"`
}

// Counts summarizes sessions or processes.
type Counts struct {
	Running int `json:"running"`
	Exited  int `json:"exited"`
	Lost    int `json:"lost"`
}

// SessionBrief is a session line in status.
type SessionBrief struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	State      string    `json:"state"`
	ExitCode   *int      `json:"exit_code,omitempty"`
	LostReason string    `json:"lost_reason,omitempty"`
	Client     string    `json:"client"`
	CreatedAt  time.Time `json:"created_at"`
	Kind       string    `json:"kind"` // session or process
}

// Report is the sandbox_status output.
type Report struct {
	Name               string            `json:"name"`
	Mode               string            `json:"mode"`
	YOLOExpiresAt      *time.Time        `json:"yolo_expires_at,omitempty"`
	YOLORemaining      string            `json:"yolo_remaining,omitempty"`
	BootID             string            `json:"boot_id"`
	BootKey            string            `json:"boot_key"`
	ContainerStartedAt time.Time         `json:"container_started_at"`
	SandboxdStartedAt  time.Time         `json:"sandboxd_started_at"`
	Tailnet            netcheck.Node     `json:"tailnet"`
	Reach              []netcheck.Result `json:"reach,omitempty"`
	DNSCheck           *DNSCheck         `json:"dns_check,omitempty"`
	EntryPoints        []EntryPoint      `json:"entry_points"`
	Sessions           Counts            `json:"sessions"`
	Processes          Counts            `json:"processes"`
	Tasks              []SessionBrief    `json:"tasks"`
	Agents             []agents.Agent    `json:"active_agents"`
	Repos              []gitx.Summary    `json:"repos"`
	GPU                GPU               `json:"gpu"`
	Caches             []Cache           `json:"caches,omitempty"`
	TokensActive       int               `json:"tokens_active"`
	Healthy            bool              `json:"healthy"`
	Problems           []string          `json:"problems"`
	GeneratedAt        time.Time         `json:"generated_at"`
}

// Collector gathers a Report.
type Collector struct {
	Name          string
	Mode          string
	YOLOExpiresAt time.Time
	StartedAt     time.Time
	Boot          bootid.Source
	Sessions      *sessions.Manager
	Procs         *procs.Manager
	Agents        *agents.Registry
	Git           *gitx.Git
	Repos         []string
	Tailscale     netcheck.Tailscale
	Net           *netcheck.Checker
	Reach         []string
	Tmux          *tmuxctl.Client
	DNSCheckFile  string
	GPUMode       string
	CacheDirs     []string
	Entrypoints   []string // enabled ones
	RunnerHealth  string   // URL
	S6ServiceDir  string   // /run/service
	TokensActive  func() (int, error)
	Now           func() time.Time

	cacheMu   sync.Mutex
	cacheAt   time.Time
	cacheData []Cache
}

// Options tune a collection.
type Options struct {
	ProbeReach bool
	Light      bool // skip git, GPU and cache probes (health checks)
}

func (c *Collector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Collect builds the report.
func (c *Collector) Collect(ctx context.Context, o Options) Report {
	r := Report{
		Name: c.Name, Mode: c.Mode, SandboxdStartedAt: c.StartedAt.UTC(), GeneratedAt: c.now().UTC(),
		Tasks: []SessionBrief{}, Agents: []agents.Agent{}, Repos: []gitx.Summary{}, Problems: []string{},
	}
	if !c.YOLOExpiresAt.IsZero() {
		t := c.YOLOExpiresAt.UTC()
		r.YOLOExpiresAt = &t
		r.YOLORemaining = max(0, time.Until(t)).Round(time.Minute).String()
	}
	if id, err := c.Boot(); err == nil {
		r.BootID, r.BootKey, r.ContainerStartedAt = id.BootID, id.Key, id.ContainerStarted
	} else {
		r.Problems = append(r.Problems, "boot identity: "+err.Error())
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	run := func(timeout time.Duration, f func(ctx context.Context)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			f(cctx)
		}()
	}
	problem := func(s string) {
		mu.Lock()
		r.Problems = append(r.Problems, s)
		mu.Unlock()
	}
	run(6*time.Second, func(ctx context.Context) {
		n := c.Tailscale.Status(ctx)
		mu.Lock()
		r.Tailnet = n
		mu.Unlock()
	})
	run(5*time.Second, func(ctx context.Context) {
		eps := c.entryPoints(ctx)
		mu.Lock()
		r.EntryPoints = eps
		mu.Unlock()
	})
	run(5*time.Second, func(ctx context.Context) {
		if c.Sessions == nil {
			return
		}
		list, err := c.Sessions.List(ctx)
		if err != nil {
			problem("sessions: " + err.Error())
			return
		}
		mu.Lock()
		defer mu.Unlock()
		for _, s := range list {
			bump(&r.Sessions, s.State)
			r.Tasks = append(r.Tasks, SessionBrief{ID: s.ID, Name: s.Name, State: s.State, ExitCode: s.ExitCode, LostReason: s.LostReason, Client: s.Client, CreatedAt: s.CreatedAt, Kind: "session"})
		}
	})
	run(5*time.Second, func(context.Context) {
		if c.Procs == nil {
			return
		}
		list, err := c.Procs.List()
		if err != nil {
			problem("processes: " + err.Error())
			return
		}
		mu.Lock()
		defer mu.Unlock()
		for _, p := range list {
			bump(&r.Processes, p.State)
			r.Tasks = append(r.Tasks, SessionBrief{ID: p.ID, Name: p.Name, State: p.State, ExitCode: p.ExitCode, LostReason: p.LostReason, Client: p.Client, CreatedAt: p.StartedAt, Kind: "process"})
		}
	})
	if c.Agents != nil {
		if list, err := c.Agents.List(""); err == nil {
			for _, a := range list {
				if a.Active {
					r.Agents = append(r.Agents, a)
				}
			}
		}
	}
	if !o.Light {
		for _, repo := range c.Repos {
			run(10*time.Second, func(ctx context.Context) {
				s := c.Git.Summarize(ctx, repo)
				mu.Lock()
				r.Repos = append(r.Repos, s)
				mu.Unlock()
			})
		}
		run(8*time.Second, func(ctx context.Context) {
			g := c.gpu(ctx)
			mu.Lock()
			r.GPU = g
			mu.Unlock()
		})
		run(15*time.Second, func(ctx context.Context) {
			cs := c.caches(ctx)
			mu.Lock()
			r.Caches = cs
			mu.Unlock()
		})
	} else {
		r.GPU = GPU{Mode: c.GPUMode}
	}
	if o.ProbeReach && c.Net != nil {
		for _, target := range c.Reach {
			run(16*time.Second, func(ctx context.Context) {
				res, err := c.Net.Check(ctx, target, 0)
				if err != nil {
					res = netcheck.Result{Target: target, Summary: err.Error()}
				}
				mu.Lock()
				r.Reach = append(r.Reach, res)
				mu.Unlock()
			})
		}
	}
	wg.Wait()
	slices.SortFunc(r.Repos, func(a, b gitx.Summary) int { return strings.Compare(a.Repo, b.Repo) })
	slices.SortFunc(r.Tasks, func(a, b SessionBrief) int { return b.CreatedAt.Compare(a.CreatedAt) })
	if len(r.Tasks) > 50 {
		r.Tasks = r.Tasks[:50]
	}
	if d, err := ReadDNSCheck(c.DNSCheckFile); err == nil {
		r.DNSCheck = &d
		if d.Status == "failed" {
			r.Problems = append(r.Problems, "dns-check failed: "+d.Message)
		}
	}
	if c.TokensActive != nil {
		n, err := c.TokensActive()
		r.TokensActive = n
		if err != nil {
			r.Problems = append(r.Problems, "tokens.json: "+err.Error())
		}
	}
	if c.Tmux != nil && !c.Tmux.Running(ctx) {
		r.Problems = append(r.Problems, "tmux server (tmux -L headmesh) is not running; sessions can't be created")
	}
	for _, ep := range r.EntryPoints {
		if ep.Enabled && !ep.Healthy {
			r.Problems = append(r.Problems, fmt.Sprintf("%s: %s %s", ep.Name, ep.State, ep.Detail))
		}
	}
	slices.Sort(r.Problems)
	r.Healthy = len(r.Problems) == 0
	return r
}

func bump(c *Counts, state string) {
	switch state {
	case sessions.StateRunning:
		c.Running++
	case sessions.StateExited:
		c.Exited++
	case sessions.StateLost:
		c.Lost++
	}
}

// ReadDNSCheck loads the dns-check result file.
func ReadDNSCheck(path string) (DNSCheck, error) {
	var d DNSCheck
	b, err := os.ReadFile(path) //nolint:gosec // G304: path built by sandboxd or chosen by the calling agent
	if err != nil {
		return d, err //nolint:wrapcheck // absence is normal
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return d, fmt.Errorf("parse %s: %w", path, err)
	}
	return d, nil
}

// s6State returns up/down from s6-svstat, or unknown outside s6.
func (c *Collector) s6State(ctx context.Context, svc string) (string, string) {
	dir := c.S6ServiceDir
	if dir == "" {
		dir = "/run/service"
	}
	out, err := exec.CommandContext(ctx, "s6-svstat", "-o", "up,ready,pid,normallyup", filepath.Join(dir, svc)).Output() //nolint:gosec // G204: fixed binary; args are passed as argv, never through a shell
	if err != nil {
		return StateUnknown, "s6-svstat unavailable"
	}
	f := strings.Fields(string(out))
	if len(f) >= 3 && f[0] == "true" {
		return StateUp, "pid " + f[2]
	}
	return StateDown, strings.TrimSpace(string(out))
}

func (c *Collector) enabled(name string) bool { return slices.Contains(c.Entrypoints, name) }

func (c *Collector) entryPoints(ctx context.Context) []EntryPoint {
	var eps []EntryPoint

	// MCP is this process.
	eps = append(eps, EntryPoint{Name: "mcp", Enabled: true, State: StateUp, Healthy: true, Detail: "headmesh-sandboxd answering"})

	ts := EntryPoint{Name: "tailscale-ssh", Enabled: true}
	n := c.Tailscale.Status(ctx)
	switch n.BackendState {
	case "Running":
		ts.State, ts.Healthy, ts.Detail = StateUp, true, n.DNSName
	case "NeedsLogin", "NoState", "Stopped":
		// Not enrolled yet is a setup step, not a fault.
		ts.Enabled = false
		ts.State, ts.Detail = StateDown, "not enrolled ("+n.BackendState+")"
		ts.Hint = "put a tagged pre-auth key in ${root}/secrets/ts_authkey and run headmesh-sandbox recreate"
	default:
		ts.State, ts.Detail = StateUnknown, n.BackendState+" "+n.Error
	}
	eps = append(eps, ts)

	runner := EntryPoint{Name: "claude-runner", Enabled: c.enabled("claude-runner"), State: StateDisabled}
	if runner.Enabled {
		runner.State, runner.Detail = c.s6State(ctx, "claude-runner")
		if c.RunnerHealth != "" {
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.RunnerHealth, nil)
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				_ = resp.Body.Close()
				runner.Healthy = resp.StatusCode == http.StatusOK
				runner.Detail = "healthz " + strconv.Itoa(resp.StatusCode)
			} else {
				runner.Detail = "healthz unreachable (runner restarting between sessions, or not set up)"
				runner.Hint = "headmesh-sandbox logs claude-runner"
			}
		}
	}
	eps = append(eps, runner)

	rc := EntryPoint{Name: "claude-rc", Enabled: c.enabled("claude-rc"), State: StateDisabled}
	if rc.Enabled && c.Tmux != nil {
		rc.State = StateDown
		rc.Hint = "first run is interactive: headmesh-sandbox shell → claude auth login → claude remote-control"
		if panes, err := c.Tmux.Panes(ctx); err == nil {
			for _, p := range panes {
				if p.WindowName == "rc" {
					if p.Dead {
						rc.Detail = "rc window exited"
					} else {
						rc.State, rc.Healthy, rc.Detail, rc.Hint = StateUp, true, "tmux window rc running "+p.Command, ""
					}
				}
			}
		}
	}
	eps = append(eps, rc)

	tun := EntryPoint{Name: "openai-tunnel", Enabled: c.enabled("openai-tunnel"), State: StateDisabled}
	if tun.Enabled {
		tun.State, tun.Detail = c.s6State(ctx, "openai-tunnel")
		tun.Healthy = tun.State == StateUp
		if !tun.Healthy {
			tun.Hint = "headmesh-sandbox logs openai-tunnel; check the runtime key and tunnel_id"
		}
	}
	eps = append(eps, tun)

	execs := liveExecServers()
	eps = append(eps, EntryPoint{
		Name: "codex-exec", Enabled: true, State: StateUp, Healthy: true,
		Detail: fmt.Sprintf("%d live executor(s); started on demand by `headmesh-sandbox agent run`", len(execs)),
	})
	return eps
}

// liveExecServers lists PIDs of running `codex exec-server` processes.
func liveExecServers() []int {
	entries, _ := os.ReadDir("/proc")
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		args := strings.Split(string(b), "\x00")
		if len(args) > 1 && strings.HasSuffix(args[0], "codex") && slices.Contains(args, "exec-server") {
			out = append(out, pid)
		}
	}
	return out
}

func (c *Collector) gpu(ctx context.Context) GPU {
	g := GPU{Mode: c.GPUMode}
	if c.GPUMode != "shared" {
		return g
	}
	out, err := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=name,memory.used,memory.total,utilization.gpu", "--format=csv,noheader").Output()
	if err != nil {
		g.Error = "nvidia-smi: " + err.Error()
		return g
	}
	g.Devices = nonEmptyLines(string(out))
	out, err = exec.CommandContext(ctx, "nvidia-smi", "--query-compute-apps=pid,process_name,used_memory", "--format=csv,noheader").Output()
	if err == nil {
		g.Processes = nonEmptyLines(string(out))
	}
	return g
}

func nonEmptyLines(s string) []string {
	var out []string
	for l := range strings.SplitSeq(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// caches runs du at most every 10 minutes; it can be slow on big caches.
func (c *Collector) caches(ctx context.Context) []Cache {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	if c.cacheData != nil && time.Since(c.cacheAt) < 10*time.Minute {
		return c.cacheData
	}
	var out []Cache
	for _, dir := range c.CacheDirs {
		cc := Cache{Path: dir}
		var st syscall.Statfs_t
		if err := syscall.Statfs(dir, &st); err != nil {
			cc.Error = err.Error()
			out = append(out, cc)
			continue
		}
		cc.FreeBytes = st.Bavail * uint64(max(st.Bsize, 0))
		b, err := exec.CommandContext(ctx, "du", "-sk", dir).Output() //nolint:gosec // G204: fixed binary; args are passed as argv, never through a shell
		if err == nil {
			if f := strings.Fields(string(b)); len(f) > 0 {
				kb, _ := strconv.ParseInt(f[0], 10, 64)
				cc.UsedBytes = kb * 1024
			}
		} else {
			cc.Error = "du: " + err.Error()
		}
		out = append(out, cc)
	}
	c.cacheData, c.cacheAt = out, time.Now()
	return out
}
