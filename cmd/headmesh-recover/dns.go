package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/headmesh-dev/headmesh/deploy/ha/recovery"
)

func runDNS(ctx context.Context, command string, args []string) error {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	var target recovery.DNSCutover
	bindProviderPins(flags, &target.Primary, &target.Standby.RunID, &target.Standby.FirewallID)
	flags.Int64Var(&target.Standby.ServerID, "standby-id", 0, "allocated Hetzner standby server ID")
	zoneID := flags.String("zone-id", "", "Cloudflare zone ID for headmesh.dev")
	var manifestPath string
	if command == "promote-dns" {
		flags.StringVar(&manifestPath, "manifest", "", "verified recovery manifest used to restore the standby")
	}
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse DNS cutover flags: %w", err)
	}
	if flags.NArg() != 0 || !validProviderPins(target.Primary, target.Standby.RunID, target.Standby.FirewallID) ||
		target.Standby.ServerID <= 0 || target.Primary.ServerID == target.Standby.ServerID {
		return fmt.Errorf("%s requires pinned primary, standby and firewall IDs and a recovery run ID", command)
	}
	var fingerprint string
	if command == "promote-dns" {
		if manifestPath == "" {
			return fmt.Errorf("promote-dns requires the verified recovery manifest")
		}
		manifest, err := readRecoveryManifest(manifestPath)
		if err != nil {
			return err
		}
		fingerprint = manifest.Headscale.NoiseKeySHA256
	}
	dns, err := recovery.NewCloudflareDNS(*zoneID, os.Getenv("CLOUDFLARE_DNS_API_TOKEN"))
	if err != nil {
		return err
	}
	provider, err := hetznerClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	var changes []recovery.DNSChange
	if command == "plan-dns" {
		changes, err = recovery.PlanDNSCutover(ctx, provider, dns, target)
	} else {
		changes, err = recovery.PromoteDNS(ctx, provider, dns, target, fingerprint)
	}
	if err != nil {
		return err
	}
	if changes == nil {
		changes = []recovery.DNSChange{}
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(changes); err != nil {
		return fmt.Errorf("write DNS cutover record: %w", err)
	}
	return nil
}
