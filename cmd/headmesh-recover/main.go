package main

import (
	"context"
	"flag"
	"fmt"
	"net/netip"
	"os"
	"os/signal"

	"github.com/headmesh-dev/headmesh/deploy/ha/recovery"
	evidence "github.com/headmesh-dev/headmesh/internal/recovery"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	err := run(ctx, os.Args[1:])
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) != 0 {
		switch args[0] {
		case "prepare-inputs":
			return runPrepareInputs(ctx, args[1:], os.Stdout)
		case "create-standby", "start-standby":
			return runStandby(ctx, args[0], args[1:], os.Stdout, hetznerClient)
		case "plan-dns", "promote-dns", "verify-cutover":
			return runDNS(ctx, args[0], args[1:])
		case "prove-ingress":
			return runProveIngress(ctx, args[1:])
		}
	}
	return fmt.Errorf("usage: headmesh-recover {prepare-inputs | create-standby | start-standby | prove-ingress | plan-dns | promote-dns | verify-cutover} [flags]")
}

func runProveIngress(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("prove-ingress", flag.ContinueOnError)
	ipv4 := flags.String("ipv4", "", "standby's assigned public IPv4 address")
	ipv6 := flags.String("ipv6", "", "standby's assigned public IPv6 address")
	manifestPath := flags.String("manifest", "", "verified recovery manifest")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse ingress flags: %w", err)
	}
	if flags.NArg() != 0 || *manifestPath == "" {
		return fmt.Errorf("prove-ingress requires a manifest and accepts flags only")
	}
	v4, err := netip.ParseAddr(*ipv4)
	if err != nil {
		return fmt.Errorf("parse standby IPv4: %w", err)
	}
	v6, err := netip.ParseAddr(*ipv6)
	if err != nil {
		return fmt.Errorf("parse standby IPv6: %w", err)
	}
	manifest, err := readRecoveryManifest(*manifestPath)
	if err != nil {
		return err
	}
	if err := recovery.ProveIngress(ctx, recovery.StandbyIngress{IPv4: v4, IPv6: v6, NoiseKeySHA256: manifest.Headscale.NoiseKeySHA256}); err != nil {
		return fmt.Errorf("standby ingress proof failed: %w", err)
	}
	fmt.Println("Verified Headscale, DERP HTTPS and STUN, API readiness, the site and the waitlist on both standby addresses.")
	return nil
}

func readRecoveryManifest(path string) (evidence.Manifest, error) {
	file, err := os.Open(path) // #nosec G304 G703 -- The operator explicitly selects the local manifest file.
	if err != nil {
		return evidence.Manifest{}, fmt.Errorf("open recovery manifest: %w", err)
	}
	defer func() { _ = file.Close() }()
	manifest, err := evidence.ReadManifest(file)
	if err != nil {
		return evidence.Manifest{}, fmt.Errorf("read recovery manifest: %w", err)
	}
	return manifest, nil
}
