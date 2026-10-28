package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/headmesh-dev/headmesh/sandbox/config"
	"github.com/headmesh-dev/headmesh/sandbox/internal/dockerx"
	"github.com/headmesh-dev/headmesh/sandbox/internal/render"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/status"
)

// HostStatus is `status --json`.
type HostStatus struct {
	Mode          string                   `json:"mode"`
	Containers    map[string]dockerx.State `json:"containers"`
	Sandbox       json.RawMessage          `json:"sandbox,omitempty"`
	SandboxError  string                   `json:"sandbox_error,omitempty"`
	BackupAge     string                   `json:"backup_age,omitempty"`
	Firewall      *FirewallStatus          `json:"firewall,omitempty"`
	OverrideFresh bool                     `json:"override_up_to_date"`
}

func statusCmd(a *app) *cobra.Command {
	var asJSON, reach bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Mode, container and service state, tailnet, entry points, sessions, agents, backups",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := a.loadConfig()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			hs := HostStatus{Mode: "stopped", Containers: map[string]dockerx.State{}}
			for _, m := range []config.Mode{config.ModeRegular, config.ModeYOLO} {
				st, err := a.docker.Inspect(ctx, cfg.ContainerName(m))
				if err != nil {
					return err
				}
				hs.Containers[cfg.ContainerName(m)] = st
				if st.Running {
					hs.Mode = string(m)
				}
			}
			if age, ok := backupAge(cfg); ok {
				hs.BackupAge = age.Round(time.Minute).String()
			}
			var fw FirewallStatus
			if readStatusFile(firewallStatusPath, &fw) {
				hs.Firewall = &fw
			}
			if old, err := os.ReadFile(a.composeFiles()[1]); err == nil {
				if res, rerr := a.dryRender(ctx, cfg); rerr == nil {
					hs.OverrideFresh = string(old) == string(res)
				}
			}
			if hs.Mode != "stopped" {
				sctx, cancel := context.WithTimeout(ctx, 45*time.Second)
				raw, err := a.socket().Status(sctx, reach)
				cancel()
				if err != nil {
					hs.SandboxError = err.Error()
				} else {
					hs.Sandbox = raw
				}
			}
			if asJSON {
				return printJSON(a.out, hs)
			}
			for name, st := range hs.Containers {
				if !st.Exists {
					continue
				}
				health := st.Health
				if health == "" {
					health = "-"
				}
				_, _ = fmt.Fprintf(a.out, "Container %s: %s (health %s, started %s, image %s)\n", name, st.Status, health, st.StartedAt.Local().Format("2006-01-02 15:04"), st.Image)
			}
			if hs.Mode == "stopped" {
				_, _ = fmt.Fprintln(a.out, "Sandbox is not running. Start it with `headmesh-sandbox up`.")
			}
			if hs.Sandbox != nil {
				var rep status.Report
				if err := json.Unmarshal(hs.Sandbox, &rep); err == nil {
					_, _ = fmt.Fprintln(a.out)
					status.WriteText(a.out, rep)
				}
			} else if hs.SandboxError != "" {
				_, _ = fmt.Fprintln(a.out, "sandboxd:", hs.SandboxError)
			}
			_, _ = fmt.Fprintln(a.out)
			if hs.BackupAge != "" {
				_, _ = fmt.Fprintf(a.out, "Home backup: last success %s ago\n", hs.BackupAge)
			} else {
				_, _ = fmt.Fprintln(a.out, "Home backup: none recorded (YOLO needs one)")
			}
			if hs.Firewall != nil && hs.Firewall.Applied {
				_, _ = fmt.Fprintf(a.out, "Egress firewall: %s applied (%s)\n", hs.Firewall.Table, hs.Firewall.UpdatedAt.Local().Format("2006-01-02 15:04"))
			} else {
				_, _ = fmt.Fprintln(a.out, "Egress firewall: NOT installed (see sandbox/host/README.md)")
			}
			if !hs.OverrideFresh {
				_, _ = fmt.Fprintln(a.out, "Generated override is stale: run `headmesh-sandbox render --diff`, then `up`.")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.Flags().BoolVar(&reach, "reach", false, "also probe the configured reach targets")
	return cmd
}

// dryRender renders without writing files.
func (a *app) dryRender(ctx context.Context, cfg *config.Config) ([]byte, error) {
	res, err := render.Render(cfg, render.OS(a.dataRoots(ctx)))
	return res.Override, err
}

// shellExec replaces the process with an interactive docker exec.
func (a *app) shellExec(argv ...string) error {
	bin, err := a.docker.Path()
	if err != nil {
		return err
	}
	args := append([]string{"docker", "exec", "-it", "-e", "TERM=" + envOr("TERM", "xterm-256color")}, argv...)
	return syscall.Exec(bin, args, os.Environ()) //nolint:gosec,wrapcheck // G204: docker with argv
}

func (a *app) runningContainer(ctx context.Context) (string, error) {
	cfg, err := a.loadConfig()
	if err != nil {
		return "", err
	}
	m, _, err := a.runningMode(ctx, cfg)
	if err != nil {
		return "", err
	}
	if m == "" {
		return "", fmt.Errorf("the sandbox is not running; start it with `headmesh-sandbox up`")
	}
	return cfg.ContainerName(m), nil
}

func shellCmd(a *app) *cobra.Command {
	var asRoot bool
	cmd := &cobra.Command{
		Use:   "shell",
		Short: "Open a login shell inside the sandbox as agent (first-run `claude auth login` happens here)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.runningContainer(cmd.Context())
			if err != nil {
				return err
			}
			user := "agent"
			if asRoot {
				user = "root"
			}
			return a.shellExec("-u", user, "-w", "/home/agent", c, "bash", "-l")
		},
	}
	cmd.Flags().BoolVar(&asRoot, "root", false, "shell as root (debugging only)")
	return cmd
}

func attachCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "attach",
		Short: "Attach to the shared tmux session (tmux -L headmesh) to watch agents live",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.runningContainer(cmd.Context())
			if err != nil {
				return err
			}
			return a.shellExec("-u", "agent", c, "tmux", "-L", "headmesh", "new-session", "-A", "-s", "headmesh")
		},
	}
}

func logsCmd(a *app) *cobra.Command {
	var follow bool
	var lines int
	cmd := &cobra.Command{
		Use:   "logs [SERVICE]",
		Short: "Show service logs (sandboxd, tailscaled, claude-runner, …) or the container log",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := a.loadConfig(); err != nil {
				return err
			}
			n := fmt.Sprint(lines)
			if len(args) == 0 {
				c, err := a.runningContainer(cmd.Context())
				if err != nil {
					return err
				}
				dargs := []string{"logs", "--tail", n}
				if follow {
					dargs = append(dargs, "--follow")
				}
				return a.docker.Run(cmd.Context(), nil, append(dargs, c)...)
			}
			if args[0] == "audit" {
				return tail(cmd.Context(), a.path("logs", "audit.jsonl"), n, follow)
			}
			p := a.path("logs", filepath.Base(args[0]), "current")
			if _, err := os.Stat(p); err != nil {
				entries, _ := os.ReadDir(a.path("logs"))
				var names []string
				for _, e := range entries {
					if e.IsDir() {
						names = append(names, e.Name())
					}
				}
				return fmt.Errorf("no log for %q (have: %s, audit)", args[0], strings.Join(names, ", "))
			}
			return tail(cmd.Context(), p, n, follow)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "follow")
	cmd.Flags().IntVarP(&lines, "lines", "n", 100, "lines")
	return cmd
}

func tail(ctx context.Context, path, n string, follow bool) error {
	args := []string{"-n", n}
	if follow {
		args = append(args, "-F")
	}
	c := exec.CommandContext(ctx, "tail", append(args, path)...) //nolint:gosec // G204: tail with argv
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	return c.Run() //nolint:wrapcheck // interactive passthrough
}
