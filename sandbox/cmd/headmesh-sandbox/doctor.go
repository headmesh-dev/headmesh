package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/headmesh-dev/headmesh/sandbox/config"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/status"
)

// Check is one doctor result.
type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok, warn, fail, skip
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

func doctorCmd(a *app) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check the host and the running sandbox; every failure comes with a fix",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := a.loadConfig()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 3*time.Minute)
			defer cancel()
			checks := a.doctor(ctx, cfg)
			if asJSON {
				return printJSON(a.out, checks)
			}
			fails := 0
			for _, c := range checks {
				mark := map[string]string{"ok": "✓", "warn": "!", "fail": "✗", "skip": "-"}[c.Status]
				_, _ = fmt.Fprintf(a.out, "%s %-22s %s\n", mark, c.Name, c.Detail)
				if c.Fix != "" && c.Status != "ok" {
					_, _ = fmt.Fprintf(a.out, "  → %s\n", c.Fix)
				}
				if c.Status == "fail" {
					fails++
				}
			}
			if fails > 0 {
				return exitCode(1)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput() //nolint:gosec // G204: fixed tools
	return strings.TrimSpace(string(out)), err
}

func (a *app) doctor(ctx context.Context, cfg *config.Config) []Check {
	var cs []Check
	add := func(name, st, detail, fix string) { cs = append(cs, Check{name, st, detail, fix}) }

	// Host.
	if v, err := run(ctx, "docker", "compose", "version", "--short"); err != nil {
		add("docker compose", "fail", err.Error(), "install Docker Engine and the Compose plugin")
	} else {
		add("docker compose", "ok", v, "")
	}
	if _, err := a.dryRender(ctx, cfg); err != nil {
		add("config", "fail", err.Error(), "fix sandbox.toml, then `headmesh-sandbox render --diff`")
	} else if old, rerr := os.ReadFile(a.composeFiles()[1]); rerr != nil {
		add("config", "warn", "valid; override not rendered yet", "headmesh-sandbox render")
	} else if res, _ := a.dryRender(ctx, cfg); string(res) != string(old) {
		add("config", "warn", "valid, but the generated override is stale", "headmesh-sandbox render --diff && headmesh-sandbox up")
	} else {
		add("config", "ok", "valid; override up to date", "")
	}
	if _, missing, err := a.secretEnv(config.ModeRegular); err != nil {
		add("secrets", "fail", err.Error(), "chmod 600 the files in "+a.path("secrets"))
	} else if len(missing) > 0 {
		add("secrets", "warn", "empty: "+strings.Join(missing, ", ")+" (those front doors stay off)", "fill "+a.path("secrets")+"/<name> (mode 0600)")
	} else {
		add("secrets", "ok", "all present, mode 0600", "")
	}
	if _, err := os.Stat("/dev/net/tun"); err != nil {
		add("host TUN", "fail", "/dev/net/tun missing", "sudo modprobe tun")
	} else {
		add("host TUN", "ok", "/dev/net/tun present", "")
	}
	var fw FirewallStatus
	switch {
	case !readStatusFile(firewallStatusPath, &fw):
		add("egress firewall", "warn", "not installed: the sandbox can reach the LAN, host services and 100.64.0.0/10 via the host", "install sandbox/host/firewall (sudo commands in sandbox/host/README.md)")
	case !fw.Applied:
		add("egress firewall", "fail", "status file says the table is not applied", "sudo systemctl restart headmesh-sandbox-firewall.service")
	default:
		add("egress firewall", "ok", fmt.Sprintf("%s on %s (%s), updated %s", fw.Table, fw.Bridge, fw.Subnet, fw.UpdatedAt.Local().Format("15:04")), "")
	}
	maxAge, _ := time.ParseDuration(cfg.Mode.YOLO.BackupMaxAge)
	if age, ok := backupAge(cfg); !ok {
		add("home backup", "warn", "no successful restic backup recorded (YOLO is refused)", "install sandbox/host/backup, then `headmesh-sandbox backup`")
	} else if age > maxAge {
		add("home backup", "warn", "last success "+age.Round(time.Minute).String()+" ago (YOLO needs < "+maxAge.String()+")", "headmesh-sandbox backup")
	} else {
		add("home backup", "ok", "last success "+age.Round(time.Minute).String()+" ago", "")
	}
	if out, _ := run(ctx, "systemctl", "is-enabled", "sleep.target", "suspend.target", "hibernate.target", "hybrid-sleep.target"); strings.Count(out, "masked") == 4 {
		add("sleep masked", "ok", "sleep, suspend, hibernate, hybrid-sleep masked", "")
	} else {
		add("sleep masked", "warn", "sleep targets are not all masked: "+strings.ReplaceAll(out, "\n", ","), "sudo systemctl mask sleep.target suspend.target hibernate.target hybrid-sleep.target (plan §5.3)")
	}
	if out, err := run(ctx, "timedatectl", "show", "-p", "NTPSynchronized", "--value"); err == nil && out == "yes" {
		add("clock", "ok", "NTP synchronized", "")
	} else {
		add("clock", "warn", "NTP not synchronized; runner auth fails beyond 5 minutes of skew", "sudo timedatectl set-ntp true")
	}
	if cfg.GPU == "shared" {
		if _, err := os.Stat("/var/run/cdi/nvidia.yaml"); err != nil {
			if _, err2 := os.Stat("/etc/cdi/nvidia.yaml"); err2 != nil {
				add("GPU CDI", "fail", "gpu = shared but no CDI spec", "sudo nvidia-ctk cdi generate --output=/var/run/cdi/nvidia.yaml")
			}
		} else {
			add("GPU CDI", "ok", "CDI spec present", "")
		}
	}

	// Container.
	m, st, err := a.runningMode(ctx, cfg)
	if err != nil || m == "" {
		add("container", "fail", "not running", "headmesh-sandbox up")
		return cs
	}
	c := cfg.ContainerName(m)
	add("container", map[bool]string{true: "ok", false: "warn"}[st.Health == "healthy"], fmt.Sprintf("%s running (%s mode, health %s)", c, m, st.Health), "headmesh-sandbox logs sandboxd")
	exec := func(user string, script string) (string, int) {
		out, errOut, code, err := a.docker.Exec(ctx, c, user, "sh", "-c", script)
		if err != nil {
			return err.Error(), -1
		}
		return strings.TrimSpace(out + errOut), code
	}
	if out, code := exec("agent", "id -u"); code != 0 || out != fmt.Sprint(cfg.Agent.UID) {
		add("agent user", "fail", "id -u = "+out, "rebuild the image with AGENT_UID="+fmt.Sprint(cfg.Agent.UID))
	} else {
		add("agent user", "ok", "uid "+out, "")
	}
	if _, code := exec("agent", "test -c /dev/net/tun"); code != 0 {
		add("TUN in container", "fail", "/dev/net/tun missing inside", "check `devices` in compose.yaml")
	} else {
		add("TUN in container", "ok", "/dev/net/tun present", "")
	}
	if out, code := exec("agent", "cat /run/secrets/claude_env_secret 2>&1 >/dev/null; echo rc=$?"); code == 0 && strings.Contains(out, "rc=0") {
		add("runner secret", "fail", "agent can read /run/secrets/claude_env_secret", "use `headmesh-sandbox up` (environment-sourced secrets), never file-sourced Compose secrets (spike T-1)")
	} else {
		add("runner secret", "ok", "agent cannot read the runner secret", "")
	}
	raw, err := a.socket().Status(ctx, false)
	if err != nil {
		add("sandboxd", "fail", err.Error(), "headmesh-sandbox logs sandboxd")
		return cs
	}
	var rep status.Report
	_ = json.Unmarshal(raw, &rep)
	add("sandboxd", "ok", fmt.Sprintf("answering; %d token(s); boot %s", rep.TokensActive, short8(rep.BootID)), "")
	switch rep.Tailnet.BackendState {
	case "Running":
		add("tailnet", "ok", fmt.Sprintf("%s %s, DERP %s", rep.Tailnet.DNSName, strings.Join(rep.Tailnet.IPs, " "), rep.Tailnet.DERPRegion), "")
		if out, code := exec("agent", "ip -o link show hm0 | sed -n 's/.* mtu \\([0-9]*\\).*/\\1/p'"); code == 0 && out != "" {
			add("MTU", map[bool]string{true: "ok", false: "warn"}[out == "1280"], "hm0 mtu "+out, "tailscaled sets 1280 by default; check for overrides")
		}
		if out, code := exec("root", "tailscale --socket=/run/tailscale/tailscaled.sock netcheck 2>&1 | grep -E 'UDP|Nearest'"); code == 0 {
			st := "ok"
			if strings.Contains(out, "UDP: false") {
				st = "warn"
			}
			add("UDP reachability", st, strings.ReplaceAll(out, "\n", "; "), "allow UDP 41641 out in the host firewall for direct paths (DERP works without it)")
		}
	case "NeedsLogin", "NoState":
		add("tailnet", "warn", "not enrolled ("+rep.Tailnet.BackendState+")", "put a single-use tagged pre-auth key in "+a.path("secrets", "ts_authkey")+" and run `headmesh-sandbox recreate`")
	default:
		add("tailnet", "fail", rep.Tailnet.BackendState+" "+rep.Tailnet.Error, "headmesh-sandbox logs tailscaled")
	}
	if rep.DNSCheck != nil {
		st := map[string]string{"ok": "ok", "skipped": "skip", "failed": "fail"}[rep.DNSCheck.Status]
		add("MagicDNS", st, rep.DNSCheck.Message, "see the message; then `headmesh-sandbox recreate`")
	}
	// Mounts as declared.
	for _, p := range cfg.RWMounts(m) {
		if _, code := exec("agent", "test -w "+shellQuote(p)); code != 0 {
			add("mount rw", "fail", p+" is not writable inside", "check ownership (uid "+fmt.Sprint(cfg.Agent.UID)+") and the generated override")
		}
	}
	for _, p := range cfg.ROMounts(m) {
		if _, code := exec("agent", "touch "+shellQuote(p)+"/.hm-doctor 2>/dev/null"); code == 0 {
			_, _ = exec("agent", "rm -f "+shellQuote(p)+"/.hm-doctor")
			add("mount ro", "fail", p+" is writable but declared read-only", "headmesh-sandbox render && headmesh-sandbox recreate")
		}
	}
	masked := 0
	for _, p := range cfg.Mounts.Masks {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if out, _ := exec("agent", "wc -c < "+shellQuote(p)+" 2>/dev/null || echo unreadable"); out != "0" && out != "unreadable" {
			add("masks", "fail", p+" is visible inside ("+out+" bytes)", "headmesh-sandbox render && headmesh-sandbox recreate")
		} else {
			masked++
		}
	}
	add("mounts", "ok", fmt.Sprintf("%d rw and %d ro mounts as declared; %d masks in place", len(cfg.RWMounts(m)), len(cfg.ROMounts(m)), masked), "")
	for _, ep := range rep.EntryPoints {
		if !ep.Enabled {
			continue
		}
		st := "ok"
		if !ep.Healthy {
			st = "warn"
		}
		add("entry: "+ep.Name, st, ep.State+" "+ep.Detail, ep.Hint)
	}
	return cs
}

func short8(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
