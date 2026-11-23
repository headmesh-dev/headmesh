package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/netip"
	"os"
	"strconv"
	"strings"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"

	"github.com/headmesh-dev/headmesh/deploy/ha/recovery"
)

func hetznerClient() (*hcloud.Client, error) {
	token := os.Getenv("HCLOUD_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("HCLOUD_TOKEN is required")
	}
	// A mutation that returned an ambiguous error is reconciled explicitly
	// before retrying a recovery command.
	return hcloud.NewClient(hcloud.WithToken(token), hcloud.WithRetryOpts(hcloud.RetryOpts{MaxRetries: 0})), nil
}

func bindProviderPins(flags *flag.FlagSet, primary *recovery.Primary, runID *string, firewallID *int64) {
	flags.Int64Var(&primary.ServerID, "primary-id", 0, "original Hetzner server ID")
	flags.StringVar(&primary.ServerName, "primary-name", "", "original Hetzner server name")
	flags.Int64Var(&primary.IPv4ID, "primary-ipv4-id", 0, "original Primary IPv4 resource ID")
	flags.Int64Var(&primary.IPv6ID, "primary-ipv6-id", 0, "original Primary IPv6 resource ID (0 if absent)")
	flags.StringVar(runID, "run-id", "", "stable ID for this recovery run")
	flags.Int64Var(firewallID, "firewall-id", 0, "approved standby firewall ID")
}

func validProviderPins(primary recovery.Primary, runID string, firewallID int64) bool {
	return primary.ServerID > 0 && primary.ServerName != "" && primary.IPv4ID > 0 &&
		primary.IPv6ID >= 0 && primary.IPv4ID != primary.IPv6ID && strings.TrimSpace(runID) != "" && firewallID > 0
}

// runStandby accepts a client factory so command-level verification can run
// against the Hetzner API contract without production credentials.
func runStandby(ctx context.Context, command string, args []string, out io.Writer, newClient func() (*hcloud.Client, error)) error {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	var primary recovery.Primary
	var config recovery.StandbyConfig
	var standbyID int64
	var keyIDs, cloudInitPath, runnerIP string
	bindProviderPins(flags, &primary, &config.RunID, &config.FirewallID)
	if command != "start-standby" {
		flags.IntVar(&config.ServerLimit, "server-limit", 0, "maximum servers allowed in the project")
		flags.StringVar(&config.Image, "image", "ubuntu-26.04", "Hetzner system image name")
		flags.StringVar(&keyIDs, "ssh-key-ids", "", "comma-separated Hetzner SSH key IDs")
	}
	switch command {
	case "check-standby":
		flags.StringVar(&runnerIP, "runner-ip", "", "SSH source address as seen by the standby firewall")
	case "create-standby":
		flags.StringVar(&cloudInitPath, "cloud-init", "", "restore-mode cloud-init file with no secrets")
	case "start-standby":
		flags.Int64Var(&standbyID, "standby-id", 0, "allocated Hetzner standby server ID")
	}
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse standby flags: %w", err)
	}
	if flags.NArg() != 0 || !validProviderPins(primary, config.RunID, config.FirewallID) {
		return fmt.Errorf("%s requires primary pins, a run ID, and the standby firewall ID", command)
	}
	if command != "start-standby" {
		if config.ServerLimit <= 0 || config.Image == "" {
			return fmt.Errorf("%s requires a server limit and image", command)
		}
		for value := range strings.SplitSeq(keyIDs, ",") {
			id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if err != nil || id <= 0 {
				return fmt.Errorf("%s requires positive comma-separated SSH key IDs", command)
			}
			config.SSHKeyIDs = append(config.SSHKeyIDs, id)
		}
	}
	var runner netip.Addr
	if command == "check-standby" {
		var err error
		runner, err = netip.ParseAddr(runnerIP)
		if err != nil {
			return fmt.Errorf("check-standby requires a runner SSH source IP")
		}
	}
	if command == "create-standby" {
		if cloudInitPath == "" {
			return fmt.Errorf("create-standby requires a cloud-init file")
		}
		cloudInit, err := os.ReadFile(cloudInitPath) // #nosec G304 -- The operator explicitly selects the local cloud-init file.
		if err != nil {
			return fmt.Errorf("read standby cloud-init: %w", err)
		}
		config.CloudConfig = string(cloudInit)
		if !strings.HasPrefix(config.CloudConfig, "#cloud-config\n") {
			return fmt.Errorf("standby cloud-init must start with #cloud-config")
		}
	} else if command == "start-standby" && (standbyID <= 0 || standbyID == primary.ServerID) {
		return fmt.Errorf("start-standby requires a distinct standby server ID")
	}

	client, err := newClient()
	if err != nil {
		return err
	}
	if command == "check-standby" {
		report, err := recovery.CheckStandbyPrerequisites(ctx, client, primary, config, runner)
		if err != nil {
			return err
		}
		if err := json.NewEncoder(out).Encode(report); err != nil {
			return fmt.Errorf("write standby preflight report: %w", err)
		}
		return nil
	}
	var server *hcloud.Server
	if command == "create-standby" {
		server, err = recovery.CreateStandby(ctx, client, primary, config)
	} else {
		server, err = recovery.StartStandby(ctx, client, primary, recovery.StandbyTarget{
			RunID: config.RunID, ServerID: standbyID, FirewallID: config.FirewallID,
		})
	}
	if err != nil {
		return err
	}
	if err := writeStandby(out, server); err != nil {
		return fmt.Errorf("report standby %d (%s): %w", server.ID, server.Name, err)
	}
	return nil
}

type standbyOutput struct {
	ServerID     int64  `json:"server_id"`
	Name         string `json:"name"`
	Status       string `json:"status"`
	Architecture string `json:"architecture"`
	IPv4         string `json:"ipv4"`
	IPv6         string `json:"ipv6"`
}

func writeStandby(out io.Writer, server *hcloud.Server) error {
	if server.ServerType == nil {
		return fmt.Errorf("provider did not report the standby architecture")
	}
	var arch string
	switch server.ServerType.Architecture {
	case hcloud.ArchitectureX86:
		arch = "amd64"
	case hcloud.ArchitectureARM:
		arch = "arm64"
	default:
		return fmt.Errorf("provider reported an unsupported standby architecture")
	}
	// Hetzner configures the first address of a newly assigned IPv6 /64.
	prefix, err := netip.ParsePrefix(server.PublicNet.IPv6.Network.String())
	if err != nil {
		return fmt.Errorf("read standby IPv6 network: %w", err)
	}
	if err := json.NewEncoder(out).Encode(standbyOutput{
		ServerID: server.ID, Name: server.Name, Status: string(server.Status), Architecture: arch,
		IPv4: server.PublicNet.IPv4.IP.String(), IPv6: prefix.Masked().Addr().Next().String(),
	}); err != nil {
		return fmt.Errorf("write standby record: %w", err)
	}
	return nil
}
