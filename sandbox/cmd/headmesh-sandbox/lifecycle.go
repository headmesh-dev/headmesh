package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/headmesh-dev/headmesh/sandbox/config"
	"github.com/headmesh-dev/headmesh/sandbox/internal/render"
)

type upOpts struct {
	yolo          bool
	ttl           string
	yes           bool
	forceRecreate bool
	noWait        bool
}

func upCmd(a *app) *cobra.Command {
	var o upOpts
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Start the sandbox (Regular unless --yolo)",
		Long: `Start the sandbox. Regular is always the default.

--yolo first checks for a restic snapshot of your home younger than
mode.yolo.backup_max_age and asks you to confirm you are watching. It then
stops Regular and starts the YOLO container, which halts itself when the TTL
expires; the next "up" starts Regular again.`,
		RunE: func(cmd *cobra.Command, _ []string) error { return a.up(cmd.Context(), o) },
	}
	cmd.Flags().BoolVar(&o.yolo, "yolo", false, "start YOLO mode (host root read-only at /host, writable home)")
	cmd.Flags().StringVar(&o.ttl, "ttl", "", "YOLO lifetime, e.g. 4h (default mode.yolo.default_ttl)")
	cmd.Flags().BoolVar(&o.yes, "yes", false, "skip the YOLO confirmation prompt (the backup check still applies)")
	cmd.Flags().BoolVar(&o.forceRecreate, "force-recreate", false, "recreate the container even if nothing changed")
	cmd.Flags().BoolVar(&o.noWait, "no-wait", false, "don't wait for sandboxd to report healthy")
	return cmd
}

func (a *app) up(ctx context.Context, o upOpts) (err error) {
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	mode := config.ModeRegular
	if o.yolo {
		mode = config.ModeYOLO
	}
	detail := string(mode)
	defer func() { a.hostAudit("up", string(mode), detail, err) }()

	if err := a.ensureDirs(); err != nil {
		return err
	}
	res, changed, err := a.renderFiles(ctx, cfg)
	for _, w := range res.Warnings {
		_, _ = fmt.Fprintln(a.errOut, "warning:", w)
	}
	if err != nil {
		return err
	}
	if changed {
		_, _ = fmt.Fprintf(a.out, "rendered %s\n", a.composeFiles()[1])
	}

	running, _, err := a.runningMode(ctx, cfg)
	if err != nil {
		return err
	}
	env := []string{render.ImageEnv(cfg)}
	sec, missing, err := a.secretEnv(mode)
	if err != nil {
		return err
	}
	env = append(env, sec...)
	if len(missing) > 0 {
		_, _ = fmt.Fprintf(a.errOut, "note: empty secrets (those front doors stay off): %s\n", strings.Join(missing, ", "))
	}

	if mode == config.ModeYOLO {
		ttl, err := cfg.ParseTTL(o.ttl)
		if err != nil {
			return err
		}
		maxAge, _ := time.ParseDuration(cfg.Mode.YOLO.BackupMaxAge)
		age, ok := backupAge(cfg)
		if !ok || age > maxAge {
			have := "no successful backup recorded in " + cfg.Backup.StatusFile
			if ok {
				have = "the last backup is " + age.Round(time.Minute).String() + " old"
			}
			return fmt.Errorf("YOLO needs a restic snapshot of your home younger than %s, and %s; run `headmesh-sandbox backup` first", maxAge, have)
		}
		if !o.yes {
			_, _ = fmt.Fprintf(a.out, `YOLO mode for %s:
  - the host root is visible read-only at /host and %s is writable;
  - prompt injection can do real damage: don't let agents ingest untrusted content
    (web pages, public issues, pasted text from strangers);
  - the container halts at the TTL and the next "up" is Regular.
Type "yolo" if you are watching and want to continue: `, ttl, strings.Join(cfg.RWMounts(config.ModeYOLO), ", "))
			line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
			if strings.TrimSpace(strings.ToLower(line)) != "yolo" {
				return fmt.Errorf("YOLO not confirmed")
			}
		}
		expires := time.Now().Add(ttl).UTC().Truncate(time.Second)
		env = append(env, "HEADMESH_YOLO_EXPIRES_AT="+expires.Format(time.RFC3339))
		detail = fmt.Sprintf("yolo ttl=%s expires=%s backup_age=%s", ttl, expires.Format(time.RFC3339), age.Round(time.Minute))
		if running == config.ModeRegular {
			_, _ = fmt.Fprintln(a.out, "stopping Regular before starting YOLO…")
			if err := a.docker.Run(ctx, env, a.composeArgs([]string{"regular"}, "stop", "--timeout", "90", "sandbox")...); err != nil {
				return err
			}
		}
		o.forceRecreate = true // a new expiry needs a new container environment
	} else if running == config.ModeYOLO {
		return fmt.Errorf("YOLO is running; stop it first with `headmesh-sandbox down --yolo` (or wait for its TTL)")
	}

	profile, service := profileFor(mode)
	args := []string{"up", "--detach", "--remove-orphans"}
	if o.forceRecreate {
		args = append(args, "--force-recreate")
	}
	args = append(args, service)
	if err := a.docker.Run(ctx, env, a.composeArgs([]string{profile}, args...)...); err != nil {
		return err
	}
	if o.noWait {
		return nil
	}
	_, _ = fmt.Fprintln(a.out, "waiting for sandboxd to report healthy…")
	wctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	h, err := a.socket().WaitHealthy(wctx, 3*time.Minute)
	if err != nil {
		return fmt.Errorf("sandbox started but is not healthy yet (%w); see `headmesh-sandbox status` and `headmesh-sandbox logs sandboxd`", err)
	}
	_, _ = fmt.Fprintf(a.out, "sandbox is up in %s mode and healthy\n", h.Mode)
	return nil
}

func downCmd(a *app) *cobra.Command {
	var yolo, all bool
	cmd := &cobra.Command{
		Use:   "down",
		Short: "Stop and remove the sandbox container (state on the host is kept)",
		RunE: func(cmd *cobra.Command, _ []string) (err error) {
			if _, err := a.loadConfig(); err != nil {
				return err
			}
			profiles := []string{"regular"}
			switch {
			case all:
				profiles = []string{"regular", "yolo"}
			case yolo:
				profiles = []string{"yolo"}
			}
			defer func() { a.hostAudit("down", "", strings.Join(profiles, ","), err) }()
			return a.down(cmd.Context(), profiles)
		},
	}
	cmd.Flags().BoolVar(&yolo, "yolo", false, "stop the YOLO container")
	cmd.Flags().BoolVar(&all, "all", false, "stop both")
	return cmd
}

func (a *app) down(ctx context.Context, profiles []string) error {
	env, _, err := a.secretEnv(config.ModeRegular)
	if err != nil {
		return err
	}
	cfg, err := a.loadConfig()
	if err != nil {
		return err
	}
	env = append(env, render.ImageEnv(cfg), "HEADMESH_YOLO_EXPIRES_AT=")
	var services []string
	for _, p := range profiles {
		if p == "yolo" {
			services = append(services, "sandbox-yolo")
		} else {
			services = append(services, "sandbox")
		}
	}
	// stop + rm keeps the shared network for the other mode.
	if err := a.docker.Run(ctx, env, a.composeArgs(profiles, append([]string{"stop", "--timeout", "90"}, services...)...)...); err != nil {
		return err
	}
	return a.docker.Run(ctx, env, a.composeArgs(profiles, append([]string{"rm", "--force"}, services...)...)...)
}

func recreateCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "recreate",
		Short: "down + up --force-recreate (Regular); state, home and tailnet identity persist",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := a.loadConfig(); err != nil {
				return err
			}
			if err := a.down(cmd.Context(), []string{"regular"}); err != nil {
				return err
			}
			return a.up(cmd.Context(), upOpts{forceRecreate: true})
		},
	}
}
