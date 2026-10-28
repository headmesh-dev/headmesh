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
		Long:          "headmesh joins machines to Headmesh networks and manages devices, invites and time-limited grants.",
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
	return root
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
