package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"runtime"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// Deps are the CLI's side effects, replaceable in tests.
type Deps struct {
	Tailscale      Tailscale
	EnsureInstance func(ctx context.Context, inst Instance) error
	Hostname       func() (string, error)
	Getenv         func(string) string
}

// NewRoot builds the command tree.
func NewRoot(version string, deps Deps) *cobra.Command {
	if deps.Tailscale == nil {
		deps.Tailscale = ExecTailscale{}
	}
	if deps.EnsureInstance == nil {
		deps.EnsureInstance = ensureInstance
	}
	if deps.Hostname == nil {
		deps.Hostname = os.Hostname
	}
	if deps.Getenv == nil {
		deps.Getenv = os.Getenv
	}
	var server string
	var asJSON bool
	root := &cobra.Command{
		Use:           "headmesh",
		Short:         "Share machines, not your network",
		Long:          "headmesh joins machines to Headmesh networks and manages devices, invites, rules and time-limited grants.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&server, "server", "", "API base URL (default $HEADMESH_SERVER or https://api.headmesh.dev)")
	root.PersistentFlags().BoolVar(&asJSON, "json", false, "print machine-readable JSON")

	client := func() *Client {
		base := server
		if base == "" {
			base = deps.Getenv("HEADMESH_SERVER")
		}
		if base == "" {
			base = "https://api.headmesh.dev"
		}
		return &Client{BaseURL: base, APIKey: deps.Getenv("HEADMESH_API_KEY"), UserAgent: "headmesh-cli/" + version}
	}
	print := func(w io.Writer, v any, human func(io.Writer)) error {
		if asJSON {
			enc := json.NewEncoder(w)
			enc.SetIndent("", "  ")
			if err := enc.Encode(v); err != nil {
				return fmt.Errorf("write output: %w", err)
			}
			return nil
		}
		human(w)
		return nil
	}

	root.AddCommand(&cobra.Command{
		Use: "version", Short: "Print the version",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), version)
			return err //nolint:wrapcheck // terminal write
		},
	})

	// join -----------------------------------------------------------------------------------------------
	var instName, name string
	var acceptDNS bool
	join := &cobra.Command{
		Use:   "join TOKEN|JOIN-URL",
		Short: "Join this machine to a Headmesh network",
		Long: `Exchange an invite for a single-use key and bring this machine onto the network.

If the machine's tailscaled is already on another tailnet, join refuses to move it; use --instance to run a
separate tailscaled (own state, socket, port and interface) alongside it. Creating an instance needs root.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			token, base, err := parseInvite(args[0])
			if err != nil {
				return err
			}
			c := client()
			if base != "" && server == "" && deps.Getenv("HEADMESH_SERVER") == "" {
				c.BaseURL = base
			}
			inst := Instance{}
			if instName != "" {
				if inst, err = NamedInstance(instName); err != nil {
					return err
				}
				if err := deps.EnsureInstance(ctx, inst); err != nil {
					return err
				}
			}
			host := name
			if host == "" {
				if host, err = deps.Hostname(); err != nil {
					return fmt.Errorf("hostname: %w", err)
				}
			}
			out := cmd.ErrOrStderr()
			_, _ = fmt.Fprintf(out, "Requesting a key from %s…\n", c.BaseURL)
			enr, err := c.Enroll(ctx, token, host, runtime.GOOS, runtime.GOARCH)
			if err != nil {
				return fmt.Errorf("enroll: %w", err)
			}
			if err := checkNotOnOtherTailnet(ctx, deps.Tailscale, inst, enr.LoginServer); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(out, "Connecting to %s as %s…\n", enr.LoginServer, enr.Hostname)
			if err := up(ctx, deps.Tailscale, inst, enr, acceptDNS); err != nil {
				return err
			}
			nodeID, err := selfNodeID(ctx, deps.Tailscale, inst)
			if err != nil {
				return err
			}
			dev, err := c.CompleteEnrollment(ctx, enr.EnrollmentID, enr.CompletionSecret, nodeID)
			if err != nil {
				return fmt.Errorf("complete enrollment: %w", err)
			}
			return print(cmd.OutOrStdout(), dev, func(w io.Writer) {
				_, _ = fmt.Fprintf(w, "Joined as %s (%s)\n", dev.GivenName, dev.IPv4)
			})
		},
	}
	join.Flags().StringVar(&instName, "instance", "", "run a separate tailscaled named NAME (needs root)")
	join.Flags().StringVar(&name, "name", "", "device name (default: this machine's hostname)")
	join.Flags().BoolVar(&acceptDNS, "accept-dns", true, "use MagicDNS for this network")
	root.AddCommand(join)

	// devices --------------------------------------------------------------------------------------------
	devices := &cobra.Command{Use: "devices", Short: "Manage devices"}
	devices.AddCommand(&cobra.Command{
		Use: "ls", Short: "List your org's devices", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ds, err := client().Devices(cmd.Context())
			if err != nil {
				return err
			}
			return print(cmd.OutOrStdout(), ds, func(w io.Writer) {
				tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
				_, _ = fmt.Fprintln(tw, "NAME\tIPV4\tKIND\tROLES\tONLINE\tID")
				for _, d := range ds {
					_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%v\t%s\n", d.GivenName, d.IPv4, d.Kind, strings.Join(d.Roles, ","), d.Online, d.ID)
				}
				_ = tw.Flush()
			})
		},
	})
	root.AddCommand(devices)

	// invite ---------------------------------------------------------------------------------------------
	var roles []string
	var kind string
	var uses int
	var ttl time.Duration
	var ephemeral bool
	invite := &cobra.Command{
		Use: "invite", Short: "Create an invite link and install commands for new devices", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			inv, err := client().CreateInvite(cmd.Context(), roles, kind, uses, ttl, ephemeral)
			if err != nil {
				return err
			}
			return print(cmd.OutOrStdout(), inv, func(w io.Writer) {
				_, _ = fmt.Fprintf(w, "Invite (expires %s, shown once):\n\n  %s\n\nOn the new machine:\n\n  %s\n",
					inv.ExpiresAt.Local().Format(time.RFC1123), inv.JoinURL, inv.Commands["linux"])
			})
		},
	}
	invite.Flags().StringSliceVar(&roles, "role", nil, "role for the joining device (repeatable)")
	invite.Flags().StringVar(&kind, "kind", "server", "device kind: server, gpu, laptop, sandbox, agent, ingress, other")
	invite.Flags().IntVar(&uses, "uses", 1, "how many devices may use the invite")
	invite.Flags().DurationVar(&ttl, "ttl", 24*time.Hour, "invite lifetime")
	invite.Flags().BoolVar(&ephemeral, "ephemeral", false, "remove devices automatically after they go offline")
	root.AddCommand(invite)

	// grants ---------------------------------------------------------------------------------------------
	grant := &cobra.Command{Use: "grant", Short: "Give another org time-limited access to one of your devices"}
	var toOrg, device, srcRole, ref string
	var ports []string
	var forDur time.Duration
	create := &cobra.Command{
		Use: "create", Short: "Create a grant", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if toOrg == "" || device == "" || len(ports) == 0 {
				return errors.New("--to, --device and --port are required")
			}
			src := map[string]string{"kind": "all"}
			if srcRole != "" {
				src = map[string]string{"kind": "role", "role": srcRole}
			}
			g, err := client().CreateGrant(cmd.Context(), map[string]any{
				"grantee_org": toOrg, "src": src, "dst_device_id": device, "ports": ports,
				"expires_at": time.Now().Add(forDur).UTC(), "external_ref": ref,
			})
			if err != nil {
				return err
			}
			return print(cmd.OutOrStdout(), g, func(w io.Writer) {
				_, _ = fmt.Fprintf(w, "Grant %s: %s on %s until %s (enforced: %v)\n", g.ID, strings.Join(g.Ports, ","),
					g.DstDeviceID, g.ExpiresAt.Local().Format(time.RFC1123), g.Enforced)
			})
		},
	}
	create.Flags().StringVar(&toOrg, "to", "", "short id of the org receiving access")
	create.Flags().StringVar(&device, "device", "", "id of your device to share")
	create.Flags().StringSliceVar(&ports, "port", nil, "port to open, e.g. tcp:22 (repeatable)")
	create.Flags().DurationVar(&forDur, "for", time.Hour, "how long the grant lasts")
	create.Flags().StringVar(&srcRole, "from-role", "", "limit access to the receiving org's devices with this role")
	create.Flags().StringVar(&ref, "ref", "", "idempotency key, e.g. a rental id")
	grant.AddCommand(create)
	grant.AddCommand(&cobra.Command{
		Use: "ls", Short: "List active grants", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			gs, err := client().Grants(cmd.Context())
			if err != nil {
				return err
			}
			return print(cmd.OutOrStdout(), gs, func(w io.Writer) {
				tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
				_, _ = fmt.Fprintln(tw, "ID\tDEVICE\tPORTS\tEXPIRES")
				for _, g := range gs {
					_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", g.ID, g.DstDeviceID, strings.Join(g.Ports, ","), g.ExpiresAt.Local().Format(time.RFC1123))
				}
				_ = tw.Flush()
			})
		},
	})
	grant.AddCommand(&cobra.Command{
		Use: "revoke ID", Short: "Revoke a grant (returns once enforced)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			g, err := client().RevokeGrant(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return print(cmd.OutOrStdout(), g, func(w io.Writer) {
				_, _ = fmt.Fprintf(w, "Revoked %s (enforced: %v)\n", g.ID, g.Enforced)
			})
		},
	})
	root.AddCommand(grant)

	// rules ----------------------------------------------------------------------------------------------
	rule := &cobra.Command{Use: "rule", Short: "Manage access between devices in your own org"}
	var ruleFrom, ruleTo string
	var rulePorts []string
	var ruleFor time.Duration
	ruleAdd := &cobra.Command{
		Use: "add", Short: "Let devices in your org reach other devices in your org", Args: cobra.NoArgs,
		Example: "  headmesh rule add --from role:ops --to role:gpu-host --port tcp:22\n" +
			"  headmesh rule add --from device:<id> --to all --port '*' --for 4h",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if ruleFrom == "" || ruleTo == "" || len(rulePorts) == 0 {
				return errors.New("--from, --to and --port are required")
			}
			src, err := ParseSelector(ruleFrom)
			if err != nil {
				return fmt.Errorf("--from: %w", err)
			}
			dst, err := ParseSelector(ruleTo)
			if err != nil {
				return fmt.Errorf("--to: %w", err)
			}
			var expires *time.Time
			if ruleFor > 0 {
				t := time.Now().Add(ruleFor).UTC()
				expires = &t
			}
			r, err := client().CreateRule(cmd.Context(), src, dst, rulePorts, expires)
			if err != nil {
				return err
			}
			return print(cmd.OutOrStdout(), r, func(w io.Writer) {
				_, _ = fmt.Fprintf(w, "Rule %s: %s → %s on %s (enforced: %v)\n", r.ID, r.Src, r.Dst, strings.Join(r.Ports, ","), r.Enforced)
			})
		},
	}
	ruleAdd.Flags().StringVar(&ruleFrom, "from", "", "devices that may connect: all, users, role:NAME or device:ID")
	ruleAdd.Flags().StringVar(&ruleTo, "to", "", "devices they may reach, in the same form")
	ruleAdd.Flags().StringSliceVar(&rulePorts, "port", nil, "port to open, e.g. tcp:22, udp:5000-5100 or * (repeatable)")
	ruleAdd.Flags().DurationVar(&ruleFor, "for", 0, "remove the access after this long (default: until deleted)")
	rule.AddCommand(ruleAdd)
	var setFrom, setTo string
	var setPorts []string
	var setFor time.Duration
	var setForever bool
	ruleSet := &cobra.Command{
		Use: "set ID", Short: "Change a rule; flags you leave out keep their current value", Args: cobra.ExactArgs(1),
		Example: "  headmesh rule set <id> --port tcp:22 --port tcp:2222\n  headmesh rule set <id> --for 2h",
		RunE: func(cmd *cobra.Command, args []string) error {
			cur, err := client().Rule(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			src, dst, ports, expires := cur.Src, cur.Dst, cur.Ports, cur.ExpiresAt
			if setFrom != "" {
				if src, err = ParseSelector(setFrom); err != nil {
					return fmt.Errorf("--from: %w", err)
				}
			}
			if setTo != "" {
				if dst, err = ParseSelector(setTo); err != nil {
					return fmt.Errorf("--to: %w", err)
				}
			}
			if len(setPorts) > 0 {
				ports = setPorts
			}
			switch {
			case setForever:
				expires = nil
			case setFor > 0:
				t := time.Now().Add(setFor).UTC()
				expires = &t
			}
			// The version read above goes back as If-Match, so a change made in between is refused, not overwritten.
			r, err := client().UpdateRule(cmd.Context(), cur.ID, cur.Version, src, dst, ports, cur.StartsAt, expires)
			if err != nil {
				return err
			}
			return print(cmd.OutOrStdout(), r, func(w io.Writer) {
				_, _ = fmt.Fprintf(w, "Rule %s (version %d): %s → %s on %s (enforced: %v)\n", r.ID, r.Version, r.Src, r.Dst,
					strings.Join(r.Ports, ","), r.Enforced)
			})
		},
	}
	ruleSet.Flags().StringVar(&setFrom, "from", "", "new source: all, users, role:NAME or device:ID")
	ruleSet.Flags().StringVar(&setTo, "to", "", "new destination, in the same form")
	ruleSet.Flags().StringSliceVar(&setPorts, "port", nil, "replace the ports (repeatable)")
	ruleSet.Flags().DurationVar(&setFor, "for", 0, "expire this long from now")
	ruleSet.Flags().BoolVar(&setForever, "forever", false, "remove the expiry")
	ruleSet.MarkFlagsMutuallyExclusive("for", "forever")
	rule.AddCommand(ruleSet)
	rule.AddCommand(&cobra.Command{
		Use: "ls", Short: "List your org's rules", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rs, err := client().Rules(cmd.Context())
			if err != nil {
				return err
			}
			return print(cmd.OutOrStdout(), rs, func(w io.Writer) {
				tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
				_, _ = fmt.Fprintln(tw, "ID\tFROM\tTO\tPORTS\tEXPIRES")
				for _, r := range rs {
					expires := "never"
					if r.ExpiresAt != nil {
						expires = r.ExpiresAt.Local().Format(time.RFC1123)
					}
					_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.ID, r.Src, r.Dst, strings.Join(r.Ports, ","), expires)
				}
				_ = tw.Flush()
			})
		},
	})
	rule.AddCommand(&cobra.Command{
		Use: "rm ID", Short: "Delete a rule (returns once enforced)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := client().DeleteRule(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return print(cmd.OutOrStdout(), r, func(w io.Writer) {
				_, _ = fmt.Fprintf(w, "Deleted %s (enforced: %v)\n", r.ID, r.Enforced)
			})
		},
	})
	root.AddCommand(rule)

	// access ---------------------------------------------------------------------------------------------
	access := &cobra.Command{Use: "access", Short: "Check what your network lets through"}
	access.AddCommand(&cobra.Command{
		Use: "check SRC DST PORT", Short: "Whether device SRC may connect to device DST on PORT, and why",
		Example: "  headmesh access check <laptop id> <gpu id> tcp:22", Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client().CheckAccess(cmd.Context(), args[0], args[1], args[2])
			if err != nil {
				return err
			}
			return print(cmd.OutOrStdout(), c, func(w io.Writer) {
				if !c.Allowed {
					_, _ = fmt.Fprintf(w, "Not allowed on %s: %s\n", c.Port, c.Detail)
					return
				}
				reasons := make([]string, 0, len(c.Because))
				for _, r := range c.Because {
					if r.ID != "" {
						reasons = append(reasons, r.Kind+" "+r.ID)
					} else {
						reasons = append(reasons, r.Kind)
					}
				}
				_, _ = fmt.Fprintf(w, "Allowed on %s by %s\n", c.Port, strings.Join(reasons, ", "))
			})
		},
	})
	root.AddCommand(access)

	// audit ----------------------------------------------------------------------------------------------
	audit := &cobra.Command{Use: "audit", Short: "Read and verify your org's tamper-evident audit log"}
	var after int64
	var limit int
	tail := &cobra.Command{
		Use: "tail", Short: "List audit events, oldest first (page with --after)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			events, next, err := client().AuditEvents(cmd.Context(), after, limit)
			if err != nil {
				return err
			}
			return print(cmd.OutOrStdout(), events, func(w io.Writer) {
				tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
				_, _ = fmt.Fprintln(tw, "ID\tTIME\tACTOR\tACTION\tTARGET")
				for _, e := range events {
					_, _ = fmt.Fprintf(tw, "%d\t%s\t%s:%s\t%s\t%s:%s\n", e.ID, e.OccurredAt.Format(time.RFC3339), e.ActorType, e.ActorID,
						e.Action, e.TargetType, e.TargetID)
				}
				_ = tw.Flush()
				if len(events) == limit {
					_, _ = fmt.Fprintf(w, "More: headmesh audit tail --after %d\n", next)
				}
			})
		},
	}
	tail.Flags().Int64Var(&after, "after", 0, "show events after this id")
	tail.Flags().IntVar(&limit, "limit", 100, "events per page (1-500)")
	audit.AddCommand(tail)
	var exportAfter int64
	var exportOut string
	export := &cobra.Command{
		Use: "export", Short: "Download the audit log as NDJSON (verifiable offline with audit verify --file)",
		Example: "  headmesh audit export -o audit.ndjson && headmesh audit verify --file audit.ndjson", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			w := cmd.OutOrStdout()
			if exportOut != "" && exportOut != "-" {
				f, err := os.OpenFile(exportOut, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // the user names the file to write
				if err != nil {
					return fmt.Errorf("open %s: %w", exportOut, err)
				}
				defer func() { _ = f.Close() }()
				w = f
			}
			return client().ExportAudit(cmd.Context(), exportAfter, w)
		},
	}
	export.Flags().Int64Var(&exportAfter, "after", 0, "start after this event id (default: the whole log)")
	export.Flags().StringVarP(&exportOut, "output", "o", "", "write to this file instead of stdout")
	audit.AddCommand(export)
	var verifyFile string
	verify := &cobra.Command{
		Use: "verify", Short: "Check that the audit log's hash chain is intact", Args: cobra.NoArgs,
		Long: "Without --file, the server recomputes the chain. With --file, this machine recomputes it from an export, " +
			"so nobody has to trust the server's word.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if verifyFile != "" {
				return verifyAuditFile(cmd, verifyFile, print)
			}
			v, err := client().VerifyAudit(cmd.Context())
			if err != nil {
				return err
			}
			if err := print(cmd.OutOrStdout(), v, func(w io.Writer) {
				if v.Verified {
					_, _ = fmt.Fprintf(w, "Audit log intact: %d events verified\n", v.Checked)
				} else {
					_, _ = fmt.Fprintf(w, "Audit log BROKEN after %d events: %s\n", v.Checked, v.Problem)
				}
			}); err != nil {
				return err
			}
			if !v.Verified {
				return errAuditBroken
			}
			return nil
		},
	}
	verify.Flags().StringVar(&verifyFile, "file", "", "verify an NDJSON export offline (- for stdin)")
	audit.AddCommand(verify)
	root.AddCommand(audit)
	return root
}

// errAuditBroken makes `headmesh audit verify` exit non-zero when the chain doesn't verify.
var errAuditBroken = errors.New("audit chain verification failed")

// verifyAuditFile checks an export offline; show is the command's --json-aware printer.
func verifyAuditFile(cmd *cobra.Command, path string, show func(io.Writer, any, func(io.Writer)) error) error {
	r := cmd.InOrStdin()
	if path != "-" {
		f, err := os.Open(path) //nolint:gosec // the user names the file to read
		if err != nil {
			return fmt.Errorf("open %s: %w", path, err)
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	v, err := VerifyAuditExport(r)
	if err != nil {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Audit export BROKEN after %d events: %v\n", v.Checked, err)
		return errAuditBroken
	}
	return show(cmd.OutOrStdout(), v, func(w io.Writer) {
		scope := "from the first event"
		if !v.FromGenesis {
			scope = fmt.Sprintf("from event %d (the file doesn't start at the beginning of the log)", v.FirstID)
		}
		_, _ = fmt.Fprintf(w, "Audit export intact: %d events verified %s to event %d\nLast hash: %s\n", v.Checked, scope, v.LastID, v.LastHash)
	})
}

// parseInvite accepts a raw token or a join URL (https://api.headmesh.dev/join/hmj_…) and returns the token and,
// for URLs, the API base URL.
func parseInvite(arg string) (token, base string, err error) {
	if strings.HasPrefix(arg, "hmj_") {
		return arg, "", nil
	}
	u, err := url.Parse(arg)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", "", errors.New("expected an invite token (hmj_…) or an https join URL")
	}
	i := strings.LastIndex(u.Path, "/join/")
	if i < 0 || !strings.HasPrefix(u.Path[i+len("/join/"):], "hmj_") {
		return "", "", errors.New("the URL is not a Headmesh join link")
	}
	return u.Path[i+len("/join/"):], u.Scheme + "://" + u.Host + u.Path[:i], nil
}
