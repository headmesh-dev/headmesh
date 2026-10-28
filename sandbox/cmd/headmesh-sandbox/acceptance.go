package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/headmesh-dev/headmesh/sandbox/config"
	"github.com/headmesh-dev/headmesh/sandbox/internal/sockclient"
	"github.com/headmesh-dev/headmesh/sandbox/tests/acceptance"
)

// inContainer reports whether the CLI runs inside the sandbox.
func inContainer() bool {
	_, err := os.Stat(sockclient.DefaultSocket)
	return err == nil && os.Getenv("HEADMESH_MODE") != ""
}

func acceptanceCmd(a *app) *cobra.Command {
	ac := &cobra.Command{Use: "acceptance", Short: "The five-check acceptance harness (plan §6.15)"}

	var route, phase, url, tokenFile, outDir, stateFile string
	runC := &cobra.Command{
		Use:   "run",
		Short: "Run the five checks through a front door and write a JSON result",
		Long: `Inside the sandbox (SSH, Claude runner, exec-server or tunnel sessions) this
talks to sandboxd over its unix socket. On the host, --route mcp talks MCP over
--url with a bearer token (the tailnet path), or over the bind-mounted socket
when --url is empty.

Check 5 needs a real disconnect: run --phase 1, end the session or connection,
reconnect through the same front door, and run --phase 2. --phase all does both
with a fresh client in one process.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !slices.Contains(acceptance.Routes, route) {
				return fmt.Errorf("--route must be one of %s", strings.Join(acceptance.Routes, ", "))
			}
			cfgPath := a.configFile()
			if inContainer() && a.cfgPath == "" {
				cfgPath = config.DefaultPath
			}
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			p := acceptance.Params{
				Route: route, AgentUID: cfg.Agent.UID, Project: cfg.Acceptance.Project, File: cfg.Acceptance.File,
				DependencyCommand: cfg.Acceptance.DependencyCommand, MagicDNSName: cfg.Acceptance.MagicDNSName,
				AllowedTarget: cfg.Acceptance.AllowedTarget, DeniedTarget: cfg.Acceptance.DeniedTarget, Home: "/home/agent",
			}
			var dial acceptance.Dialer
			switch {
			case url != "":
				tok, err := readKey(tokenFile, filepath.Join(os.Getenv("HOME"), ".config", "headmesh", "sandbox-token"))
				if err != nil {
					return fmt.Errorf("token: %w", err)
				}
				dial = acceptance.HTTPDialer(url, tok)
			case inContainer():
				dial = acceptance.SocketDialer(sockclient.DefaultSocket)
			default:
				a.root = cfg.Root
				dial = acceptance.SocketDialer(a.path("run", "sandboxd.sock"))
				if sum, err := acceptance.FileSHA256(filepath.Join(cfg.Acceptance.Project, cfg.Acceptance.File)); err == nil {
					p.ExpectedFileSHA256 = sum
				}
			}
			if outDir == "" {
				if inContainer() {
					outDir = "/var/lib/headmesh/artifacts/acceptance"
				} else {
					outDir = filepath.Join(cfg.Root, "artifacts", "acceptance")
				}
			}
			if stateFile == "" {
				stateFile = filepath.Join(outDir, ".state-"+route+".json")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Minute)
			defer cancel()
			res, err := acceptance.Run(ctx, dial, p, phase, stateFile)
			if err != nil {
				return err
			}
			path, err := acceptance.Save(outDir, res)
			if err != nil {
				return err
			}
			printResult(a, res)
			_, _ = fmt.Fprintf(a.out, "result: %s\n", path)
			if phase == "1" {
				_, _ = fmt.Fprintf(a.out, "now disconnect (end this session or connection), reconnect through the same front door and run:\n  headmesh-sandbox acceptance run --route %s --phase 2\n", route)
			}
			if !res.Pass {
				return exitCode(1)
			}
			return nil
		},
	}
	runC.Flags().StringVar(&route, "route", "", "front door: "+strings.Join(acceptance.Routes, ", "))
	runC.Flags().StringVar(&phase, "phase", "all", "all, 1 (checks 1–4 + start the marker session) or 2 (verify after reconnecting)")
	runC.Flags().StringVar(&url, "url", "", "MCP URL (route mcp from another machine), e.g. http://asus-sandbox.mesh.xcelsior.ca:8780/mcp")
	runC.Flags().StringVar(&tokenFile, "token-file", "", "bearer token file for --url (default ~/.config/headmesh/sandbox-token)")
	runC.Flags().StringVar(&outDir, "out-dir", "", "where results go (default the artifacts dir)")
	runC.Flags().StringVar(&stateFile, "state-file", "", "phase 1 → 2 state (default in out-dir)")

	var vRoute string
	verify := &cobra.Command{
		Use:   "verify",
		Short: "Cross-check the latest results on the host (file hash, all five checks)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := a.loadConfig()
			if err != nil {
				return err
			}
			routes := acceptance.Routes
			if vRoute != "" {
				routes = []string{vRoute}
			}
			dir := a.path("artifacts", "acceptance")
			failed := 0
			for _, r := range routes {
				res, problems, err := acceptance.Verify(dir, r, filepath.Join(cfg.Acceptance.Project, cfg.Acceptance.File))
				switch {
				case err != nil:
					_, _ = fmt.Fprintf(a.out, "%-12s no result (%v)\n", r, err)
					if vRoute != "" {
						failed++
					}
				case len(problems) == 0:
					_, _ = fmt.Fprintf(a.out, "%-12s PASS %d/%d (%s)\n", r, res.Passed, res.Total, res.StartedAt.Local().Format("2006-01-02 15:04"))
				default:
					failed++
					_, _ = fmt.Fprintf(a.out, "%-12s FAIL %d/%d\n", r, res.Passed, res.Total)
					for _, p := range problems {
						_, _ = fmt.Fprintln(a.out, "    - "+p)
					}
				}
			}
			if failed > 0 {
				return exitCode(1)
			}
			return nil
		},
	}
	verify.Flags().StringVar(&vRoute, "route", "", "only this route")

	var reportOut string
	report := &cobra.Command{
		Use:   "report",
		Short: "Write a Markdown pass/fail table for every route (docs/acceptance/<date>.md)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := a.loadConfig()
			if err != nil {
				return err
			}
			dir := a.path("artifacts", "acceptance")
			var b strings.Builder
			_, _ = fmt.Fprintf(&b, "# Sandbox acceptance — %s\n\n| Route | Result | 1 command | 2 file | 3 dependency | 4 MagicDNS | 5 reconnect | When |\n|---|---|---|---|---|---|---|---|\n", time.Now().Format("2006-01-02"))
			for _, r := range append(slices.Clone(acceptance.Routes), "recreate") {
				res, problems, err := acceptance.Verify(dir, r, filepath.Join(cfg.Acceptance.Project, cfg.Acceptance.File))
				if err != nil {
					_, _ = fmt.Fprintf(&b, "| %s | not run | | | | | | |\n", r)
					continue
				}
				marks := map[int]string{}
				for _, c := range res.Checks {
					marks[c.ID] = map[bool]string{true: "✓", false: "✗"}[c.Pass]
				}
				verdict := "PASS"
				if len(problems) > 0 {
					verdict = "FAIL"
				}
				_, _ = fmt.Fprintf(&b, "| %s | %s %d/%d | %s | %s | %s | %s | %s | %s |\n", r, verdict, res.Passed, res.Total,
					marks[1], marks[2], marks[3], marks[4], marks[5], res.StartedAt.Local().Format("2006-01-02 15:04"))
			}
			if reportOut == "" {
				_, err := fmt.Fprint(a.out, b.String())
				return err //nolint:wrapcheck // stdout
			}
			if err := os.MkdirAll(filepath.Dir(reportOut), 0o750); err != nil {
				return fmt.Errorf("report dir: %w", err)
			}
			return writeIfChanged(reportOut, []byte(b.String()))
		},
	}
	report.Flags().StringVar(&reportOut, "out", "", "write the report here instead of stdout")

	recreate := &cobra.Command{
		Use:   "recreate",
		Short: "Bonus check: down + up --force-recreate keeps files, tailnet identity and history; sessions turn lost",
		RunE:  func(cmd *cobra.Command, _ []string) error { return a.recreateCheck(cmd.Context()) },
	}
	ac.AddCommand(runC, verify, report, recreate)
	return ac
}

func printResult(a *app, r acceptance.Result) {
	_, _ = fmt.Fprintf(a.out, "route %s, phase %s: %d/%d passed\n", r.Route, r.Phase, r.Passed, r.Total)
	for _, c := range r.Checks {
		mark := "✗"
		if c.Pass {
			mark = "✓"
		}
		_, _ = fmt.Fprintf(a.out, "  %s %d %-40s %s\n", mark, c.ID, c.Name, c.Details)
	}
}

// recreateCheck is the §6.15 bonus, run from the host.
func (a *app) recreateCheck(ctx context.Context) error {
	if _, err := a.loadConfig(); err != nil {
		return err
	}
	dial := acceptance.SocketDialer(a.path("run", "sandboxd.sock"))
	stamp := time.Now().UTC().Format("20060102T150405")
	marker := "# hm-recreate-" + stamp
	before, err := acceptance.Snapshot(ctx, dial, marker)
	if err != nil {
		return fmt.Errorf("before recreate: %w", err)
	}
	_, _ = fmt.Fprintf(a.out, "before: node %q %v, boot %s, session %s\n", before.Node.NodeID, before.Node.IPs, before.Node.BootKey, before.SessionID)
	if err := a.down(ctx, []string{"regular"}); err != nil {
		return err
	}
	if err := a.up(ctx, upOpts{forceRecreate: true}); err != nil {
		return err
	}
	res, err := acceptance.CheckAfterRecreate(ctx, dial, before, marker)
	if err != nil {
		return err
	}
	path, err := acceptance.Save(a.path("artifacts", "acceptance"), res)
	if err != nil {
		return err
	}
	printResult(a, res)
	_, _ = fmt.Fprintf(a.out, "result: %s\n", path)
	if !res.Pass {
		return exitCode(1)
	}
	return nil
}
