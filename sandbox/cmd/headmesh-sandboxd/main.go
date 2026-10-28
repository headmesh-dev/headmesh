// Command headmesh-sandboxd is the MCP server and session manager inside the
// agent sandbox, plus the small helpers the s6 services and runner hooks
// call (see sandbox/docs/container-contract.md). Invoked as `hm`, it is the
// in-container status and agents client.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/headmesh-dev/headmesh/sandbox/config"
	"github.com/headmesh-dev/headmesh/sandbox/internal/sockclient"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/agents"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/audit"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/authn"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/dnscheck"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/guidelines"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/netcheck"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/procutil"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/runnerhook"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/server"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/status"
)

// version is set with -ldflags "-X main.version=…".
var version = "dev"

const defaultTemplate = "/etc/headmesh/agent-guidelines.md.tmpl"

func main() {
	if err := newRoot().Execute(); err != nil {
		var ec exitCode
		if errors.As(err, &ec) {
			os.Exit(int(ec))
		}
		_, _ = fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// exitCode ends the process with a code and no extra message.
type exitCode int

func (e exitCode) Error() string { return "exit " + strconv.Itoa(int(e)) }

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           filepath.Base(os.Args[0]),
		Short:         "Headmesh sandbox daemon (MCP server, sessions) and in-container helpers",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(serveCmd(), healthCmd(), statusCmd(), dnsCheckCmd(), guidelinesCmd(), yoloTTLCmd(),
		runnerHookCmd(), agentsCmd(), versionCmd())
	return root
}

func configPath(flag string) string {
	if flag != "" {
		return flag
	}
	if p := os.Getenv("HEADMESH_SANDBOX_CONFIG"); p != "" {
		return p
	}
	return config.DefaultPath
}

func modeFromEnv() (config.Mode, time.Time, error) {
	mode, err := config.ParseMode(os.Getenv("HEADMESH_MODE"))
	if err != nil {
		return "", time.Time{}, err
	}
	var exp time.Time
	if v := os.Getenv("HEADMESH_YOLO_EXPIRES_AT"); v != "" && mode == config.ModeYOLO {
		if exp, err = time.Parse(time.RFC3339, v); err != nil {
			return "", time.Time{}, fmt.Errorf("HEADMESH_YOLO_EXPIRES_AT: %w", err)
		}
	}
	return mode, exp, nil
}

func versionCmd() *cobra.Command {
	return &cobra.Command{Use: "version", Short: "Print the version", Run: func(cmd *cobra.Command, _ []string) {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), version)
	}}
}

func serveCmd() *cobra.Command {
	var cfgFlag, listen, unixPath string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the MCP server on TCP (tailnet) and the unix socket (host CLI)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
			cfg, err := config.Load(configPath(cfgFlag))
			if err != nil {
				return err
			}
			mode, exp, err := modeFromEnv()
			if err != nil {
				return err
			}
			tmpl := ""
			if _, err := os.Stat(defaultTemplate); err == nil {
				tmpl = defaultTemplate
			}
			srv, err := server.New(server.Options{
				Config: cfg, Mode: mode, YOLOExpiresAt: exp, Version: version, GuidelinesTemplate: tmpl, Logger: log,
				Tailscale: netcheck.Tailscale{Socket: envOr("TS_SOCKET", "/run/tailscale/tailscaled.sock")},
			})
			if err != nil {
				return err
			}
			defer func() { _ = srv.Close() }()

			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT)
			defer stop()
			for i := 0; ; i++ {
				if err := srv.EnsureTmux(ctx); err == nil || i >= 30 {
					if err != nil {
						log.Warn("tmux session not available; sessions will fail until it is", "err", err)
					}
					break
				}
				time.Sleep(500 * time.Millisecond)
			}
			if listen == "" {
				listen = cfg.Sandboxd.Listen
			}
			lc := net.ListenConfig{}
			tcpLn, err := lc.Listen(ctx, "tcp", listen)
			if err != nil {
				return fmt.Errorf("listen %s: %w", listen, err)
			}
			unixLn, err := listenUnix(ctx, unixPath)
			if err != nil {
				return err
			}
			tcpSrv := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
			unixSrv := &http.Server{Handler: srv.UnixHandler(), ReadHeaderTimeout: 10 * time.Second, ConnContext: authn.UnixConnContext}
			errc := make(chan error, 2)
			go func() { errc <- tcpSrv.Serve(tcpLn) }()
			go func() { errc <- unixSrv.Serve(unixLn) }()
			log.Info("headmesh-sandboxd serving", "version", version, "mode", mode, "listen", listen, "unix", unixPath,
				"tokens", srv.Tokens().Count())
			select {
			case <-ctx.Done():
			case err := <-errc:
				if !errors.Is(err, http.ErrServerClosed) {
					log.Error("server stopped", "err", err)
				}
			}
			shCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			_ = tcpSrv.Shutdown(shCtx)
			_ = unixSrv.Shutdown(shCtx)
			_ = os.Remove(unixPath)
			return nil
		},
	}
	cmd.Flags().StringVar(&cfgFlag, "config", "", "sandbox.toml (default $HEADMESH_SANDBOX_CONFIG or "+config.DefaultPath+")")
	cmd.Flags().StringVar(&listen, "listen", "", "TCP address (default sandboxd.listen)")
	cmd.Flags().StringVar(&unixPath, "unix", sockclient.DefaultSocket, "unix socket for the host CLI")
	return cmd
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// listenUnix replaces a stale socket and makes it group-accessible (UID
// 1000 on both sides of the bind mount).
func listenUnix(ctx context.Context, path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("socket dir: %w", err)
	}
	if _, err := os.Stat(path); err == nil {
		d := net.Dialer{Timeout: time.Second}
		if c, err := d.DialContext(ctx, "unix", path); err == nil {
			_ = c.Close()
			return nil, fmt.Errorf("another sandboxd is serving %s", path)
		}
		_ = os.Remove(path)
	}
	lc := net.ListenConfig{}
	ln, err := lc.Listen(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o660); err != nil { //nolint:gosec // G302: socket shared with the host user (same uid) through the bind mount
		_ = ln.Close()
		return nil, fmt.Errorf("chmod socket: %w", err)
	}
	return ln, nil
}

func healthCmd() *cobra.Command {
	var sock string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "health",
		Short: "Exit 0 when sandboxd is healthy (Docker HEALTHCHECK, s6 readiness)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			h, err := sockclient.New(sock).Health(ctx)
			if err != nil {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), err)
				return exitCode(1)
			}
			b, _ := json.Marshal(h)
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), string(b))
			if !h.OK {
				return exitCode(1)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&sock, "unix", sockclient.DefaultSocket, "sandboxd socket")
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Second, "give up after")
	return cmd
}

func statusCmd() *cobra.Command {
	var sock string
	var asJSON, reach bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show mode, tailnet, entry points, sessions and agents",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 60*time.Second)
			defer cancel()
			raw, err := sockclient.New(sock).Status(ctx, reach)
			if err != nil {
				return err
			}
			if asJSON {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), string(raw))
				return err //nolint:wrapcheck // stdout write
			}
			var rep status.Report
			if err := json.Unmarshal(raw, &rep); err != nil {
				return fmt.Errorf("decode status: %w", err)
			}
			status.WriteText(cmd.OutOrStdout(), rep)
			return nil
		},
	}
	cmd.Flags().StringVar(&sock, "unix", sockclient.DefaultSocket, "sandboxd socket")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.Flags().BoolVar(&reach, "reach", false, "also probe the configured reach targets")
	return cmd
}

func dnsCheckCmd() *cobra.Command {
	var cfgFlag, out string
	var strict bool
	cmd := &cobra.Command{
		Use:   "dns-check",
		Short: "Check MagicDNS once enrolled (pitfall 3) and write the result for health",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(configPath(cfgFlag))
			if err != nil {
				return err
			}
			name := cfg.Tailnet.DNSCheckName
			if name != "" && !strings.Contains(name, ".") {
				name += "." + cfg.Tailnet.MagicDNSSuffix
			}
			ch := &dnscheck.Checker{TS: netcheck.Tailscale{Socket: envOr("TS_SOCKET", "/run/tailscale/tailscaled.sock")}}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			res := ch.Run(ctx, name, cfg.Tailnet.DNSCheckIP)
			if err := os.MkdirAll(filepath.Dir(out), 0o750); err != nil {
				return fmt.Errorf("output dir: %w", err)
			}
			if err := procutil.WriteJSONAtomic(out, res, 0o644); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "dns-check %s: %s\n", res.Status, res.Message)
			if strict && res.Status == "failed" {
				return exitCode(1)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&cfgFlag, "config", "", "sandbox.toml")
	cmd.Flags().StringVar(&out, "out", "/run/headmesh/dns-check.json", "result file")
	cmd.Flags().BoolVar(&strict, "strict", false, "exit 1 when the check fails")
	return cmd
}

func guidelinesCmd() *cobra.Command {
	g := &cobra.Command{Use: "guidelines", Short: "Agent guidelines"}
	var cfgFlag, tmplPath, home, bannerOut string
	render := &cobra.Command{
		Use:   "render",
		Short: "Write ~/.claude/CLAUDE.md, ~/.codex/AGENTS.md and the shell banner for the active mode",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(configPath(cfgFlag))
			if err != nil {
				return err
			}
			mode, exp, err := modeFromEnv()
			if err != nil {
				return err
			}
			tmpl := ""
			if tmplPath != "" {
				b, err := os.ReadFile(tmplPath) //nolint:gosec // G304: operator-chosen template
				if err != nil {
					return fmt.Errorf("template: %w", err)
				}
				tmpl = string(b)
			}
			d := guidelines.FromConfig(cfg, mode, exp)
			block, err := guidelines.Render(tmpl, d)
			if err != nil {
				return err
			}
			for _, p := range []string{filepath.Join(home, ".claude", "CLAUDE.md"), filepath.Join(home, ".codex", "AGENTS.md")} {
				if err := guidelines.WriteInto(p, block); err != nil {
					return err
				}
			}
			if bannerOut != "" {
				if err := os.MkdirAll(filepath.Dir(bannerOut), 0o750); err != nil {
					return fmt.Errorf("banner dir: %w", err)
				}
				if err := os.WriteFile(bannerOut, []byte(guidelines.Banner(d, "")), 0o644); err != nil { //nolint:gosec // G306: the banner is world-readable by design
					return fmt.Errorf("banner: %w", err)
				}
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "guidelines rendered for %s mode\n", mode)
			return nil
		},
	}
	render.Flags().StringVar(&cfgFlag, "config", "", "sandbox.toml")
	render.Flags().StringVar(&tmplPath, "template", "", "template file (default: built in)")
	render.Flags().StringVar(&home, "home", "/home/agent", "agent home")
	render.Flags().StringVar(&bannerOut, "banner-out", "", "write the login banner here")
	g.AddCommand(render)
	return g
}

func yoloTTLCmd() *cobra.Command {
	var expiresAt, halt, auditPath string
	cmd := &cobra.Command{
		Use:   "yolo-ttl",
		Short: "Wait until the YOLO TTL expires, audit, then halt the container",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if expiresAt == "" {
				expiresAt = os.Getenv("HEADMESH_YOLO_EXPIRES_AT")
			}
			exp, err := time.Parse(time.RFC3339, expiresAt)
			if err != nil {
				return fmt.Errorf("--expires-at: %w", err)
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT)
			defer stop()
			for {
				left := time.Until(exp)
				if left <= 0 {
					break
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "YOLO mode: halting in %s (at %s)\n", left.Round(time.Second), exp.Format(time.RFC3339))
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(min(left, 15*time.Minute)):
				}
			}
			if l, err := audit.Open(auditPath, string(config.ModeYOLO)); err == nil {
				_ = l.Write(audit.Entry{
					Client: "yolo-ttl", Tool: "yolo_ttl_halt", Result: audit.ResultOK,
					Detail: "TTL expired at " + exp.Format(time.RFC3339) + "; halting; the next up starts Regular",
				})
				_ = l.Close()
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "YOLO TTL expired; halting the container")
			return syscall.Exec(halt, []string{halt}, os.Environ()) //nolint:gosec // G204: fixed s6 halt path
		},
	}
	cmd.Flags().StringVar(&expiresAt, "expires-at", "", "RFC 3339 time (default $HEADMESH_YOLO_EXPIRES_AT)")
	cmd.Flags().StringVar(&halt, "halt", "/run/s6/basedir/bin/halt", "command that stops the container")
	cmd.Flags().StringVar(&auditPath, "audit", "/var/log/headmesh/audit.jsonl", "audit log")
	return cmd
}

// dropToAgent re-executes the current command as `agent` when running as
// root (runner hooks run with the runner's root identity).
func dropToAgent() error {
	if os.Geteuid() != 0 || os.Getenv("HEADMESH_PRIV_DROPPED") == "1" {
		return nil
	}
	u, err := user.Lookup("agent")
	if err != nil {
		return fmt.Errorf("lookup agent: %w", err)
	}
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("own path: %w", err)
	}
	setpriv, err := exec.LookPath("setpriv")
	if err != nil {
		return fmt.Errorf("setpriv: %w", err)
	}
	env := append(os.Environ(), "HEADMESH_PRIV_DROPPED=1", "HOME="+u.HomeDir, "USER=agent", "LOGNAME=agent")
	argv := append([]string{setpriv, "--reuid=" + u.Uid, "--regid=" + u.Gid, "--init-groups", "--inh-caps=-all", "--", self}, os.Args[1:]...)
	return syscall.Exec(setpriv, argv, env) //nolint:gosec,wrapcheck // G204: fixed setpriv, argv only
}

func runnerHookCmd() *cobra.Command {
	hook := &cobra.Command{Use: "runner-hook", Short: "Claude self-hosted runner lifecycle hooks"}
	var cfgFlag, workDir, sock string
	checkout := &cobra.Command{
		Use:   "checkout",
		Short: "Link the session's repo to the shared working copy (or a fresh clone)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := dropToAgent(); err != nil {
				return err
			}
			cfg, err := config.Load(configPath(cfgFlag))
			if err != nil {
				return err
			}
			h := &runnerhook.Hook{Config: cfg, WorkDir: workDir, Out: cmd.OutOrStdout()}
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Minute)
			defer cancel()
			res, err := h.RunCheckout(ctx, runnerhook.CheckoutFromEnv())
			for _, w := range res.Warnings {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "warning:", w)
			}
			return err
		},
	}
	post := &cobra.Command{
		Use:   "post-session",
		Short: "Unregister the session's agents and record what it left behind",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := dropToAgent(); err != nil {
				return err
			}
			cfg, err := config.Load(configPath(cfgFlag))
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 50*time.Second)
			defer cancel()
			p := runnerhook.PostSessionFromEnv()
			h := &runnerhook.Hook{Config: cfg}
			rep := h.Report(ctx, p)
			b, _ := json.MarshalIndent(rep, "", "  ")
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), string(b))
			c := sockclient.New(sock)
			// Best effort: a failure here must not hold up the runner.
			var list struct{ Agents []agents.Agent }
			if err := c.Call(ctx, "agents_list", nil, &list); err == nil {
				for _, a := range list.Agents {
					if p.SessionID != "" && a.Session == p.SessionID {
						_ = c.Call(ctx, "agents_unregister", map[string]any{"id": a.ID}, nil)
					}
				}
			}
			name := "runner-session-" + strings.TrimPrefix(p.SessionID, "session_") + ".json"
			if err := c.Call(ctx, "artifact_put", map[string]any{"name": name, "content": string(b)}, nil); err != nil {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "warning: could not store the session report:", err)
			}
			return nil
		},
	}
	hook.PersistentFlags().StringVar(&cfgFlag, "config", "", "sandbox.toml")
	hook.PersistentFlags().StringVar(&sock, "unix", sockclient.DefaultSocket, "sandboxd socket")
	checkout.Flags().StringVar(&workDir, "work-dir", "/srv/runner-work", "fresh clones for repos not in [repos]")
	hook.AddCommand(checkout, post)
	return hook
}

func agentsCmd() *cobra.Command {
	a := &cobra.Command{Use: "agents", Short: "Register, heartbeat and list agents (plan §6.10)"}
	var sock string
	var asJSON bool
	a.PersistentFlags().StringVar(&sock, "unix", sockclient.DefaultSocket, "sandboxd socket")
	a.PersistentFlags().BoolVar(&asJSON, "json", false, "print JSON")

	var agent, task, session string
	var repos []string
	register := &cobra.Command{
		Use:   "register",
		Short: "Announce this agent; prints who else is active",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if session == "" {
				session = envOr("HEADMESH_SESSION", os.Getenv("CLAUDE_RUNNER_SESSION_ID"))
			}
			var res agents.RegisterResult
			if err := sockclient.New(sock).Call(cmd.Context(), "agents_register",
				map[string]any{"agent": agent, "task": task, "repos": repos, "session": session}, &res); err != nil {
				return err
			}
			if f := os.Getenv("CLAUDE_ENV_FILE"); f != "" {
				// Claude Code SessionStart hooks persist variables this way.
				if fh, err := os.OpenFile(f, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600); err == nil { //nolint:gosec // G304: path from Claude Code
					_, _ = fmt.Fprintf(fh, "export HEADMESH_AGENT_ID=%s\n", res.Agent.ID)
					_ = fh.Close()
				}
			}
			return printAgents(cmd, asJSON, res, res.Notice+"\nHEADMESH_AGENT_ID="+res.Agent.ID)
		},
	}
	register.Flags().StringVar(&agent, "agent", envOr("HEADMESH_ENTRYPOINT", ""), "entry point (claude-runner, claude-rc, codex-ssh, …)")
	register.Flags().StringVar(&task, "task", "", "one-line task")
	register.Flags().StringArrayVar(&repos, "repo", nil, "repo path or [repos] name (repeatable)")
	register.Flags().StringVar(&session, "session", "", "session id (default $HEADMESH_SESSION)")

	var id, hbTask string
	heartbeat := &cobra.Command{
		Use:   "heartbeat",
		Short: "Keep a registration alive",
		RunE: func(cmd *cobra.Command, _ []string) error {
			var res agents.RegisterResult
			if err := sockclient.New(sock).Call(cmd.Context(), "agents_heartbeat", map[string]any{"id": agentID(id), "task": hbTask}, &res); err != nil {
				return err
			}
			return printAgents(cmd, asJSON, res, res.Notice)
		},
	}
	heartbeat.Flags().StringVar(&id, "id", "", "agent id (default $HEADMESH_AGENT_ID)")
	heartbeat.Flags().StringVar(&hbTask, "task", "", "updated task")

	var unID string
	unregister := &cobra.Command{
		Use:   "unregister",
		Short: "Remove a registration",
		RunE: func(cmd *cobra.Command, _ []string) error {
			var res map[string]any
			if err := sockclient.New(sock).Call(cmd.Context(), "agents_unregister", map[string]any{"id": agentID(unID)}, &res); err != nil {
				return err
			}
			return printAgents(cmd, asJSON, res, fmt.Sprint(res["message"]))
		},
	}
	unregister.Flags().StringVar(&unID, "id", "", "agent id (default $HEADMESH_AGENT_ID)")

	var listRepo string
	list := &cobra.Command{
		Use:   "list",
		Short: "List registered agents",
		RunE: func(cmd *cobra.Command, _ []string) error {
			var res struct{ Agents []agents.Agent }
			if err := sockclient.New(sock).Call(cmd.Context(), "agents_list", map[string]any{"repo": listRepo}, &res); err != nil {
				return err
			}
			var b strings.Builder
			for _, ag := range res.Agents {
				state := "stale"
				if ag.Active {
					state = "active"
				}
				_, _ = fmt.Fprintf(&b, "%s  %-14s %-6s %s  %s\n", ag.ID, ag.Agent, state, strings.Join(ag.Repos, ","), ag.Task)
			}
			if b.Len() == 0 {
				b.WriteString("no agents registered\n")
			}
			return printAgents(cmd, asJSON, res, strings.TrimRight(b.String(), "\n"))
		},
	}
	list.Flags().StringVar(&listRepo, "repo", "", "only this repo")
	a.AddCommand(register, heartbeat, unregister, list)
	return a
}

func agentID(flag string) string {
	if flag != "" {
		return flag
	}
	return os.Getenv("HEADMESH_AGENT_ID")
}

func printAgents(cmd *cobra.Command, asJSON bool, v any, text string) error {
	if asJSON {
		b, _ := json.MarshalIndent(v, "", "  ")
		text = string(b)
	}
	_, err := fmt.Fprintln(cmd.OutOrStdout(), text)
	return err //nolint:wrapcheck // stdout write
}
