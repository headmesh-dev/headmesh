package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/headmesh-dev/headmesh/sandbox/config"
	"github.com/headmesh-dev/headmesh/sandbox/internal/orchestrator"
	"github.com/headmesh-dev/headmesh/sandbox/internal/scansecrets"
)

func backupCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "backup",
		Short: "Run the restic home backup now (headmesh-home-backup.service) and show its status",
		RunE: func(cmd *cobra.Command, _ []string) (err error) {
			cfg, err := a.loadConfig()
			if err != nil {
				return err
			}
			defer func() { a.hostAudit("backup", "", "", err) }()
			_, _ = fmt.Fprintln(a.out, "starting headmesh-home-backup.service (may take a while)…")
			c := exec.CommandContext(cmd.Context(), "systemctl", "start", "headmesh-home-backup.service")
			c.Stdout, c.Stderr = a.out, a.errOut
			if err := c.Run(); err != nil {
				return fmt.Errorf("systemctl start headmesh-home-backup.service: %w (install the unit and the polkit rule from sandbox/host/backup, or run it with sudo)", err)
			}
			if age, ok := backupAge(cfg); ok {
				_, _ = fmt.Fprintf(a.out, "last successful backup: %s ago\n", age.Round(time.Second))
				return nil
			}
			return fmt.Errorf("the backup ran but %s records no success; see `journalctl -u headmesh-home-backup.service`", cfg.Backup.StatusFile)
		},
	}
}

func scanSecretsCmd(a *app) *cobra.Command {
	var apply, yolo, noGitleaks bool
	var maxFiles int
	cmd := &cobra.Command{
		Use:   "scan-secrets",
		Short: "Find secret files in the read-write mounts and propose /dev/null masks",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := a.loadConfig()
			if err != nil {
				return err
			}
			roots := cfg.RWMounts(config.ModeRegular)
			if yolo {
				roots = cfg.RWMounts(config.ModeYOLO)
			}
			_, _ = fmt.Fprintf(a.out, "scanning %s\n", strings.Join(roots, ", "))
			named := scansecrets.ByName(roots, maxFiles)
			var propose []string
			for _, f := range named {
				state := "proposed mask"
				if containsStr(cfg.Mounts.Masks, f.Path) {
					state = "already masked"
				} else {
					propose = append(propose, f.Path)
				}
				_, _ = fmt.Fprintf(a.out, "  %-15s %s (%s)\n", state, f.Path, f.Reason)
			}
			if !noGitleaks {
				ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Minute)
				defer cancel()
				runner, where := a.gitleaksRunner(ctx, cfg)
				if runner == nil {
					_, _ = fmt.Fprintln(a.out, "gitleaks: not on this host and the sandbox isn't running; skipped (filename heuristics only)")
				} else {
					_, _ = fmt.Fprintf(a.out, "gitleaks (%s):\n", where)
					for _, r := range roots {
						fs, err := scansecrets.Gitleaks(ctx, runner, r)
						if err != nil {
							_, _ = fmt.Fprintf(a.errOut, "  gitleaks %s: %v\n", r, err)
							continue
						}
						for _, f := range fs {
							_, _ = fmt.Fprintf(a.out, "  review         %s (%s)\n", f.Path, f.Reason)
						}
					}
				}
			}
			if len(propose) == 0 {
				_, _ = fmt.Fprintln(a.out, "no new masks to propose")
				return nil
			}
			if !apply {
				_, _ = fmt.Fprintf(a.out, "\n%d new mask(s) proposed. Review them, then run with --apply (or add them to [mounts] masks yourself).\n", len(propose))
				return nil
			}
			src, err := os.ReadFile(a.configFile())
			if err != nil {
				return fmt.Errorf("read config: %w", err)
			}
			out, n, ok := scansecrets.ApplyMasks(string(src), propose)
			if !ok {
				return fmt.Errorf("no `masks = [...]` array in %s; add the paths by hand", a.configFile())
			}
			if _, err := config.Parse([]byte(out)); err != nil {
				return fmt.Errorf("the edited config would not validate: %w", err)
			}
			if err := writeIfChanged(a.configFile(), []byte(out)); err != nil {
				return err
			}
			a.hostAudit("scan_secrets_apply", "", fmt.Sprintf("%d masks", n), nil)
			_, _ = fmt.Fprintf(a.out, "added %d mask(s) to %s; run `headmesh-sandbox render --diff` and `up`\n", n, a.configFile())
			return nil
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "add the proposed masks to sandbox.toml")
	cmd.Flags().BoolVar(&yolo, "yolo", false, "scan the YOLO read-write mounts (includes your home)")
	cmd.Flags().BoolVar(&noGitleaks, "no-gitleaks", false, "filename heuristics only")
	cmd.Flags().IntVar(&maxFiles, "max-files", 500000, "stop after this many files per run")
	return cmd
}

// gitleaksRunner prefers a host gitleaks, else the copy in the running image.
func (a *app) gitleaksRunner(ctx context.Context, cfg *config.Config) ([]string, string) {
	if p, err := exec.LookPath("gitleaks"); err == nil {
		return []string{p}, "host"
	}
	m, _, err := a.runningMode(ctx, cfg)
	if err != nil || m == "" {
		return nil, ""
	}
	return []string{"docker", "exec", "-u", "agent", cfg.ContainerName(m), "gitleaks"}, "inside " + cfg.ContainerName(m)
}

func agentCmd(a *app) *cobra.Command {
	ag := &cobra.Command{Use: "agent", Short: "Run agents against the sandbox from the host"}
	var provider, repo, prompt, promptFile, model, instructions, appKeyFile, envKeyFile, pidNS, baseURL string
	var timeout time.Duration
	runCmd := &cobra.Command{
		Use:   "run",
		Short: "Run one Codex task through the OpenAI Agents API with the sandbox as its self-hosted environment",
		Long: `Creates an Agents API session whose environment is self-hosted, starts
"codex exec-server" inside the sandbox with docker exec (as agent, in the repo),
sends the prompt and streams the answer. The application key never enters the
sandbox; only the restricted environment key does, through the executor's stdin.`,
		RunE: func(cmd *cobra.Command, _ []string) (err error) {
			if provider != "openai" {
				return fmt.Errorf("provider %q is not supported (only openai)", provider)
			}
			cfg, err := a.loadConfig()
			if err != nil {
				return err
			}
			workspace, err := cfg.RepoPath(repo)
			if err != nil {
				return err
			}
			text := prompt
			if promptFile != "" {
				b, err := os.ReadFile(promptFile) //nolint:gosec // G304: operator-chosen prompt
				if err != nil {
					return fmt.Errorf("prompt file: %w", err)
				}
				text = string(b)
			}
			if strings.TrimSpace(text) == "" {
				return fmt.Errorf("give --prompt or --prompt-file")
			}
			appKey, err := readKey(appKeyFile, "/etc/headmesh/openai-app.key", a.path("secrets", "openai_app_key"))
			if err != nil {
				return fmt.Errorf("application key: %w", err)
			}
			envKey, err := readKey(envKeyFile, a.path("secrets", "openai_env_key"))
			if err != nil {
				return fmt.Errorf("environment key: %w", err)
			}
			m, _, err := a.runningMode(cmd.Context(), cfg)
			if err != nil || m == "" {
				return fmt.Errorf("the sandbox is not running; start it with `headmesh-sandbox up`")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			logDir := a.path("logs", "agent-runs")
			_ = os.MkdirAll(logDir, 0o750)
			var transcript io.WriteCloser
			ex := &dockerExecutor{container: cfg.ContainerName(m), workdir: workspace, envKey: envKey, pidNS: pidNS, stderr: a.errOut}
			res, err := orchestrator.Run(ctx, &orchestrator.Client{BaseURL: baseURL, AppKey: appKey}, ex, orchestrator.Options{
				Model: model, Instructions: instructions, Workspace: workspace, Prompt: text,
				Output: a.out, Log: a.errOut,
				OnSessionCreated: func(s orchestrator.Session) {
					f, ferr := os.OpenFile(filepath.Join(logDir, s.ID+".jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304: CLI-owned dir
					if ferr == nil {
						transcript = f
					}
				},
				Transcript: writerFunc(func(p []byte) (int, error) {
					if transcript == nil {
						return len(p), nil
					}
					return transcript.Write(p)
				}),
			})
			if transcript != nil {
				_ = transcript.Close()
			}
			a.hostAudit("agent_run", string(m), fmt.Sprintf("openai session=%s repo=%s outcome=%s", res.SessionID, workspace, res.Outcome), err)
			if res.SessionID != "" {
				_, _ = fmt.Fprintf(a.errOut, "session %s: %s; events in %s\n", res.SessionID, res.Outcome, filepath.Join(logDir, res.SessionID+".jsonl"))
			}
			return err
		},
	}
	runCmd.Flags().StringVar(&provider, "provider", "openai", "agent provider")
	runCmd.Flags().StringVar(&repo, "repo", "", "repo from [repos], a bare name, or an absolute path (the workspace)")
	runCmd.Flags().StringVar(&prompt, "prompt", "", "task text")
	runCmd.Flags().StringVar(&promptFile, "prompt-file", "", "file with the task text")
	runCmd.Flags().StringVar(&model, "model", "gpt-6-astra", "model for the session")
	runCmd.Flags().StringVar(&instructions, "instructions", "You work inside the Headmesh agent sandbox. Follow ~/.codex/AGENTS.md: register with `hm agents register`, work on main, commit small, git pull --rebase before pushing, never force-push.", "agent instructions")
	runCmd.Flags().StringVar(&appKeyFile, "app-key-file", "", "Agents API application key (default /etc/headmesh/openai-app.key, then <root>/secrets/openai_app_key)")
	runCmd.Flags().StringVar(&envKeyFile, "env-key-file", "", "restricted environment key (default <root>/secrets/openai_env_key)")
	runCmd.Flags().StringVar(&pidNS, "linux-sandbox-pid-namespace", "inherit", "passed to codex exec-server (spike T-16)")
	runCmd.Flags().StringVar(&baseURL, "base-url", orchestrator.DefaultBaseURL, "API base URL")
	runCmd.Flags().DurationVar(&timeout, "timeout", 2*time.Hour, "give up after")
	_ = runCmd.MarkFlagRequired("repo")
	ag.AddCommand(runCmd)
	return ag
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func readKey(flag string, defaults ...string) (string, error) {
	paths := defaults
	if flag != "" {
		paths = []string{flag}
	}
	var lastErr error
	for _, p := range paths {
		b, err := os.ReadFile(p) //nolint:gosec // G304: key locations
		if err != nil {
			lastErr = err
			continue
		}
		if k := strings.TrimSpace(string(b)); k != "" {
			return k, nil
		}
		lastErr = fmt.Errorf("%s is empty", p)
	}
	return "", fmt.Errorf("none of %s is readable: %w", strings.Join(paths, ", "), lastErr)
}

// dockerExecutor starts codex exec-server inside the sandbox. The
// environment key goes in through stdin; closing stdin stops the server.
type dockerExecutor struct {
	container, workdir, envKey, pidNS string
	stderr                            io.Writer
}

func (d *dockerExecutor) Start(ctx context.Context, remoteURL, envID string) (func() error, error) {
	// Codex's bubblewrap sandbox can't run inside the hardened container, so the
	// container is the boundary (spike T-16): full access, no approval prompts.
	script := `IFS= read -r CODEX_API_KEY; export CODEX_API_KEY; exec codex -c sandbox_mode="danger-full-access" -c approval_policy="never" exec-server --remote "$1" --environment-id "$2" --exit-on-stdin-close --linux-sandbox-pid-namespace "${3:-inherit}"`
	c := exec.CommandContext(ctx, "docker", "exec", "-i", "-u", "agent", "-w", d.workdir, //nolint:gosec // G204: docker with argv
		"-e", "HEADMESH_ENTRYPOINT=codex-exec", d.container, "bash", "-lc", script, "hm-exec", remoteURL, envID, d.pidNS)
	stdin, err := c.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin: %w", err)
	}
	c.Stdout, c.Stderr = d.stderr, d.stderr
	if err := c.Start(); err != nil {
		return nil, fmt.Errorf("docker exec: %w", err)
	}
	if _, err := io.WriteString(stdin, d.envKey+"\n"); err != nil {
		_ = c.Process.Kill()
		return nil, fmt.Errorf("pass environment key: %w", err)
	}
	return func() error {
		_ = stdin.Close()
		done := make(chan error, 1)
		go func() { done <- c.Wait() }()
		select {
		case err := <-done:
			return err
		case <-time.After(20 * time.Second):
			_ = c.Process.Kill()
			return fmt.Errorf("executor did not exit after stdin closed; killed")
		}
	}, nil
}
