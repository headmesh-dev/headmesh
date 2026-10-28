package main

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/headmesh-dev/headmesh/sandbox/config"
	"github.com/headmesh-dev/headmesh/sandbox/internal/render"
	"github.com/headmesh-dev/headmesh/sandbox/sandboxd/authn"
)

func initCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Create the sandbox root, a starter sandbox.toml and empty secret files",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.ensureDirs(); err != nil {
				return err
			}
			cfgFile := a.configFile()
			if _, err := os.Stat(cfgFile); errors.Is(err, os.ErrNotExist) {
				ex := string(config.Example())
				ex = strings.Replace(ex, `root = "/mnt/storage/headmesh/sandbox"`, fmt.Sprintf("root = %q", a.root), 1)
				if home, err := os.UserHomeDir(); err == nil {
					ex = strings.Replace(ex, `host_home = "/home/aaryn"`, fmt.Sprintf("host_home = %q", home), 1)
					ex = strings.Replace(ex, `rw = ["/home/aaryn"]`, fmt.Sprintf("rw = [%q]", home), 1)
				}
				ex = strings.Replace(ex, "uid = 1000", "uid = "+strconv.Itoa(os.Getuid()), 1)
				ex = strings.Replace(ex, "gid = 1000", "gid = "+strconv.Itoa(os.Getgid()), 1)
				ex = strings.Replace(ex, `status_file = "/mnt/storage/headmesh/sandbox/state/backup-status.json"`,
					fmt.Sprintf("status_file = %q", a.path("state", "backup-status.json")), 1)
				if err := os.WriteFile(cfgFile, []byte(ex), 0o640); err != nil { //nolint:gosec // G306: config holds no secrets
					return fmt.Errorf("write %s: %w", cfgFile, err)
				}
				_, _ = fmt.Fprintf(a.out, "wrote %s (review mounts, repos and masks)\n", cfgFile)
			} else {
				_, _ = fmt.Fprintf(a.out, "kept existing %s\n", cfgFile)
			}
			for _, n := range secretNames {
				p := a.path("secrets", n)
				if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
					if err := os.WriteFile(p, nil, 0o600); err != nil {
						return fmt.Errorf("create %s: %w", p, err)
					}
				}
			}
			if u, err := user.Current(); err == nil && u.Uid != "1000" {
				_, _ = fmt.Fprintf(a.errOut, "note: you are uid %s; the image's agent user is uid 1000 unless you build with AGENT_UID=%s\n", u.Uid, u.Uid)
			}
			_, _ = fmt.Fprintf(a.out, `
Next steps (see sandbox/README.md):
  1. Edit %s.
  2. Put secrets in %s (mode 0600; empty = front door off):
       ts_authkey         single-use tagged Headscale pre-auth key
       github_token       fine-grained PAT for the repos in [repos]
       claude_env_secret  Claude self-hosted runner environment secret
       openai_tunnel_key  OpenAI tunnel runtime key (Read+Use)
  3. headmesh-sandbox token create claude-code-mac
  4. headmesh-sandbox render --diff && headmesh-sandbox up
`, cfgFile, a.path("secrets"))
			return nil
		},
	}
}

func renderCmd(a *app) *cobra.Command {
	var diff, quiet bool
	cmd := &cobra.Command{
		Use:   "render",
		Short: "Validate sandbox.toml, refuse denylisted mounts, and write compose.override.generated.yaml",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := a.loadConfig()
			if err != nil {
				return err
			}
			old, _ := os.ReadFile(a.composeFiles()[1])
			res, changed, err := a.renderFiles(cmd.Context(), cfg)
			for _, w := range res.Warnings {
				_, _ = fmt.Fprintln(a.errOut, "warning:", w)
			}
			if err != nil {
				return err
			}
			if !quiet {
				for _, s := range res.Services {
					printService(a, s)
				}
			}
			switch {
			case !changed:
				_, _ = fmt.Fprintf(a.out, "%s is up to date\n", a.composeFiles()[1])
			case diff:
				_, _ = fmt.Fprint(a.out, lineDiff(string(old), string(res.Override)))
				_, _ = fmt.Fprintf(a.out, "wrote %s; run `headmesh-sandbox up` to apply\n", a.composeFiles()[1])
			default:
				_, _ = fmt.Fprintf(a.out, "wrote %s; run `headmesh-sandbox up` to apply\n", a.composeFiles()[1])
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&diff, "diff", false, "show what changed in the generated override")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "don't print the mount table")
	return cmd
}

func printService(a *app, s render.Service) {
	_, _ = fmt.Fprintf(a.out, "\n%s (%s, hostname %s)\n", s.Name, s.Mode, s.Hostname)
	tw := tabwriter.NewWriter(a.out, 0, 2, 2, ' ', 0)
	for _, m := range s.Mounts {
		mode := "rw"
		if m.ReadOnly {
			mode = "ro"
		}
		src := m.Source
		if m.Type == "tmpfs" {
			src = "(empty tmpfs)"
		}
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t←\t%s\t%s\n", mode, m.Target, src, m.Purpose)
	}
	_ = tw.Flush()
}

// lineDiff is a small LCS line diff for previews.
func lineDiff(a, b string) string {
	x, y := strings.Split(a, "\n"), strings.Split(b, "\n")
	n, m := len(x), len(y)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out strings.Builder
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && x[i] == y[j]:
			i++
			j++
		case j < m && (i == n || lcs[i][j+1] >= lcs[i+1][j]):
			out.WriteString("+ " + y[j] + "\n")
			j++
		default:
			out.WriteString("- " + x[i] + "\n")
			i++
		}
	}
	return out.String()
}

func tokenCmd(a *app) *cobra.Command {
	t := &cobra.Command{Use: "token", Short: "Manage sandboxd bearer tokens (argon2id hashes in config/tokens.json)"}
	var scopes []string
	var route string
	create := &cobra.Command{
		Use:   "create NAME",
		Short: "Create a token for one client and print it once",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := a.path("config", "tokens.json")
			f, err := authn.LoadFile(path)
			if err != nil {
				return err
			}
			tok, rec, err := f.Create(args[0], scopes, route, time.Now())
			if err != nil {
				return err
			}
			if err := f.Save(path); err != nil {
				return err
			}
			a.hostAudit("token_create", "", rec.Name+" "+strings.Join(rec.Scopes, ","), nil)
			_, _ = fmt.Fprintf(a.errOut, "token %s (%s) for %s, scopes %s, route %s.\nStore it in the client now; it is not shown again:\n",
				rec.ID, rec.Prefix+"…", rec.Name, strings.Join(rec.Scopes, " "), rec.Route)
			_, _ = fmt.Fprintln(a.out, tok)
			return nil
		},
	}
	create.Flags().StringSliceVar(&scopes, "scope", nil, "scopes (default sandbox:read,sandbox:exec,sandbox:write)")
	create.Flags().StringVar(&route, "route", authn.RouteMCP, "mcp or tunnel (tunnel caps exec at 110 s and responses at 8 MiB)")

	list := &cobra.Command{
		Use:   "list",
		Short: "List tokens (never the values)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			f, err := authn.LoadFile(a.path("config", "tokens.json"))
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(a.out, 0, 2, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "ID\tNAME\tPREFIX\tROUTE\tSCOPES\tCREATED\tSTATE")
			for _, r := range f.Tokens {
				state := "active"
				if !r.Active() {
					state = "revoked " + r.RevokedAt.Local().Format("2006-01-02")
				}
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s…\t%s\t%s\t%s\t%s\n", r.ID, r.Name, r.Prefix, r.Route, strings.Join(r.Scopes, ","), r.CreatedAt.Local().Format("2006-01-02"), state)
			}
			return tw.Flush()
		},
	}
	revoke := &cobra.Command{
		Use:   "revoke NAME|ID",
		Short: "Revoke a token; sandboxd stops accepting it within seconds",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := a.path("config", "tokens.json")
			f, err := authn.LoadFile(path)
			if err != nil {
				return err
			}
			n, err := f.Revoke(args[0], time.Now())
			if err != nil {
				return err
			}
			if err := f.Save(path); err != nil {
				return err
			}
			a.hostAudit("token_revoke", "", args[0], nil)
			_, _ = fmt.Fprintf(a.out, "revoked %d token(s) named %s\n", n, args[0])
			return nil
		},
	}
	t.AddCommand(create, list, revoke)
	return t
}
