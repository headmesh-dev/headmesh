// Command headmesh-sandbox is the host CLI for the agent sandbox (plan
// §6.11, §6.14, §6.15, §7.6): render the Compose override from
// sandbox.toml, start and stop the Regular or YOLO container, inspect and
// diagnose it, manage sandboxd tokens, scan for secrets, run the acceptance
// harness, and drive Codex sessions through the Agents API with the sandbox
// as the self-hosted environment.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/headmesh-dev/headmesh/sandbox/compose"
	"github.com/headmesh-dev/headmesh/sandbox/config"
	"github.com/headmesh-dev/headmesh/sandbox/internal/dockerx"
	"github.com/headmesh-dev/headmesh/sandbox/internal/render"
	"github.com/headmesh-dev/headmesh/sandbox/internal/sockclient"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/audit"
)

// version is set with -ldflags "-X main.version=…".
var version = "dev"

// DefaultRoot is the sandbox root on the ASUS.
const DefaultRoot = "/mnt/storage/headmesh/sandbox"

// Secret names (files in ${root}/secrets, env HM_SECRET_<UPPER>).
var secretNames = []string{"ts_authkey", "github_token", "claude_env_secret", "openai_tunnel_key", "github_ssh_key"}

// yoloOnlySecrets are passed to the container only in YOLO mode.
var yoloOnlySecrets = map[string]bool{"github_ssh_key": true}

// rootDirs are created by init and up.
var rootDirs = []string{"config", "home", "state/tailscale-regular", "state/tailscale-yolo", "sessions", "artifacts", "logs", "run", "runner", "runner-work", "compose"}

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

type exitCode int

func (e exitCode) Error() string { return "exit " + strconv.Itoa(int(e)) }

// app holds the global flags and shared helpers.
type app struct {
	root    string
	cfgPath string
	out     io.Writer
	errOut  io.Writer
	docker  dockerx.Docker
}

func newRoot() *cobra.Command {
	a := &app{}
	root := &cobra.Command{
		Use:           "headmesh-sandbox",
		Short:         "Run and inspect the Headmesh agent sandbox on this host",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRun: func(cmd *cobra.Command, _ []string) {
			a.out, a.errOut = cmd.OutOrStdout(), cmd.ErrOrStderr()
			a.docker = dockerx.Docker{Stdout: a.out, Stderr: a.errOut}
			if a.root == "" {
				a.root = envOr("HEADMESH_SANDBOX_ROOT", DefaultRoot)
			}
		},
	}
	root.PersistentFlags().StringVar(&a.root, "root", "", "sandbox root (default $HEADMESH_SANDBOX_ROOT or "+DefaultRoot+")")
	root.PersistentFlags().StringVar(&a.cfgPath, "config", "", "sandbox.toml (default <root>/config/sandbox.toml)")
	root.AddCommand(
		initCmd(a), renderCmd(a), upCmd(a), downCmd(a), recreateCmd(a),
		statusCmd(a), doctorCmd(a), logsCmd(a), shellCmd(a), attachCmd(a),
		backupCmd(a), scanSecretsCmd(a), tokenCmd(a), acceptanceCmd(a), agentCmd(a),
		&cobra.Command{Use: "version", Short: "Print the version", Run: func(cmd *cobra.Command, _ []string) {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), version)
		}},
	)
	return root
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func (a *app) path(parts ...string) string {
	return filepath.Join(append([]string{a.root}, parts...)...)
}

func (a *app) configFile() string {
	if a.cfgPath != "" {
		return a.cfgPath
	}
	return a.path("config", "sandbox.toml")
}

func (a *app) loadConfig() (*config.Config, error) {
	cfg, err := config.Load(a.configFile())
	if err != nil {
		return nil, err
	}
	if cfg.Root != a.root && a.cfgPath == "" {
		return nil, fmt.Errorf("sandbox.toml root = %q but the CLI uses %q; pass --root %s or fix the file", cfg.Root, a.root, cfg.Root)
	}
	a.root = cfg.Root
	return cfg, nil
}

func (a *app) socket() *sockclient.Client { return sockclient.New(a.path("run", "sandboxd.sock")) }

func (a *app) composeFiles() []string {
	return []string{a.path("compose", "compose.yaml"), a.path("compose", "compose.override.generated.yaml")}
}

func profileFor(m config.Mode) (profile, service string) {
	if m == config.ModeYOLO {
		return "yolo", "sandbox-yolo"
	}
	return "regular", "sandbox"
}

// composeArgs prefixes a compose subcommand with the project and files.
func (a *app) composeArgs(profiles []string, args ...string) []string {
	out := []string{"compose", "--project-name", "headmesh-sandbox", "--project-directory", a.path("compose")}
	for _, f := range a.composeFiles() {
		out = append(out, "-f", f)
	}
	for _, p := range profiles {
		out = append(out, "--profile", p)
	}
	return append(out, args...)
}

// secretEnv reads ${root}/secrets/* for the docker compose process only.
func (a *app) secretEnv(mode config.Mode) ([]string, []string, error) {
	var env, missing []string
	for _, n := range secretNames {
		if yoloOnlySecrets[n] && mode != config.ModeYOLO {
			env = append(env, "HM_SECRET_"+strings.ToUpper(n)+"=")
			continue
		}
		p := a.path("secrets", n)
		fi, err := os.Stat(p)
		if errors.Is(err, os.ErrNotExist) {
			missing = append(missing, n)
			env = append(env, "HM_SECRET_"+strings.ToUpper(n)+"=")
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("secret %s: %w", n, err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			return nil, nil, fmt.Errorf("%s is readable by group or others (mode %04o); run chmod 600 %s", p, fi.Mode().Perm(), p)
		}
		b, err := os.ReadFile(p) //nolint:gosec // G304: ${root}/secrets
		if err != nil {
			return nil, nil, fmt.Errorf("read secret %s: %w", n, err)
		}
		v := strings.TrimSpace(string(b))
		if n == "github_ssh_key" && v != "" {
			v += "\n" // OpenSSH wants the trailing newline
		}
		if v == "" {
			missing = append(missing, n)
		}
		env = append(env, "HM_SECRET_"+strings.ToUpper(n)+"="+v)
	}
	return env, missing, nil
}

// renderFiles writes compose.yaml and the generated override and returns
// whether the override changed.
func (a *app) renderFiles(ctx context.Context, cfg *config.Config) (render.Result, bool, error) {
	res, err := render.Render(cfg, render.OS(a.dataRoots(ctx)))
	if err != nil {
		return res, false, err
	}
	if err := os.MkdirAll(a.path("compose"), 0o750); err != nil {
		return res, false, fmt.Errorf("compose dir: %w", err)
	}
	if err := writeIfChanged(a.path("compose", "compose.yaml"), compose.File); err != nil {
		return res, false, err
	}
	old, _ := os.ReadFile(a.composeFiles()[1])
	changed := string(old) != string(res.Override)
	if changed {
		if err := writeIfChanged(a.composeFiles()[1], res.Override); err != nil {
			return res, false, err
		}
	}
	return res, changed, nil
}

// dataRoots lists Docker and containerd roots, which no mount may touch.
func (a *app) dataRoots(ctx context.Context) []string {
	var roots []string
	if d := a.docker.DataRoot(ctx); d != "" {
		roots = append(roots, d)
	}
	for _, d := range []string{"/var/lib/docker", "/var/lib/containerd"} {
		if _, err := os.Stat(d); err == nil && !containsStr(roots, d) {
			roots = append(roots, d)
		}
	}
	return roots
}

func containsStr(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func writeIfChanged(path string, data []byte) error {
	if old, err := os.ReadFile(path); err == nil && string(old) == string(data) { //nolint:gosec // G304: CLI-owned path
		return nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o640); err != nil { //nolint:gosec // G306: compose files are not secret
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

// ensureDirs creates the root layout (idempotent).
func (a *app) ensureDirs() error {
	for _, d := range rootDirs {
		if err := os.MkdirAll(a.path(d), 0o750); err != nil {
			return fmt.Errorf("create %s: %w", a.path(d), err)
		}
	}
	if err := os.MkdirAll(a.path("secrets"), 0o700); err != nil {
		return fmt.Errorf("create secrets dir: %w", err)
	}
	return os.Chmod(a.path("secrets"), 0o700) //nolint:wrapcheck,gosec // G302: tightens the secrets dir to 0700
}

// hostAudit records CLI actions next to sandboxd's audit log.
func (a *app) hostAudit(tool, mode, detail string, err error) {
	l, oerr := audit.Open(a.path("logs", "host-audit.jsonl"), mode)
	if oerr != nil {
		return
	}
	defer func() { _ = l.Close() }()
	e := audit.Entry{Client: "host-cli:" + envOr("USER", strconv.Itoa(os.Getuid())), Tool: tool, Result: audit.ResultOK, Detail: detail}
	if err != nil {
		e.Result, e.Error = audit.ResultError, err.Error()
	}
	_ = l.Write(e)
}

// runningMode reports which mode's container is running, if any.
func (a *app) runningMode(ctx context.Context, cfg *config.Config) (config.Mode, dockerx.State, error) {
	for _, m := range []config.Mode{config.ModeRegular, config.ModeYOLO} {
		st, err := a.docker.Inspect(ctx, cfg.ContainerName(m))
		if err != nil {
			return "", st, err
		}
		if st.Running {
			return m, st, nil
		}
	}
	return "", dockerx.State{}, nil
}

func printJSON(w io.Writer, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	_, err = fmt.Fprintln(w, string(b))
	return err //nolint:wrapcheck // stdout write
}

// readStatusFile decodes a small JSON status file; missing files are not errors.
func readStatusFile(path string, v any) bool {
	b, err := os.ReadFile(path) //nolint:gosec // G304: known status paths
	if err != nil {
		return false
	}
	return json.Unmarshal(b, v) == nil
}

// backupAge returns the age of the last successful restic backup.
func backupAge(cfg *config.Config) (time.Duration, bool) {
	var st struct {
		LastSuccessAt time.Time `json:"last_success_at"`
	}
	if !readStatusFile(cfg.Backup.StatusFile, &st) || st.LastSuccessAt.IsZero() {
		return 0, false
	}
	return time.Since(st.LastSuccessAt), true
}

// FirewallStatus is written by headmesh-sandbox-firewall.
type FirewallStatus struct {
	Table        string    `json:"table"`
	Applied      bool      `json:"applied"`
	Hash         string    `json:"hash"`
	Bridge       string    `json:"bridge"`
	Subnet       string    `json:"subnet"`
	YOLOAllowLAN bool      `json:"yolo_allow_lan"`
	UpdatedAt    time.Time `json:"updated_at"`
}

const firewallStatusPath = "/run/headmesh-sandbox-firewall.json"
