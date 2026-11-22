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
		case "create-standby", "start-standby":
			return runStandby(ctx, args[0], args[1:], os.Stdout, hetznerClient)
		case "prove-ingress":
			return runProveIngress(ctx, args[1:])
		}
	}
	return fmt.Errorf("usage: headmesh-recover {create-standby | start-standby | prove-ingress} [flags]")
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
	file, err := os.Open(*manifestPath)
	if err != nil {
		return fmt.Errorf("open recovery manifest: %w", err)
	}
	defer func() { _ = file.Close() }()
	manifest, err := evidence.ReadManifest(file)
	if err != nil {
		return fmt.Errorf("read recovery manifest: %w", err)
	}
	if err := recovery.ProveIngress(ctx, recovery.StandbyIngress{IPv4: v4, IPv6: v6, NoiseKeySHA256: manifest.Headscale.NoiseKeySHA256}); err != nil {
		return fmt.Errorf("standby ingress proof failed: %w", err)
	}
	fmt.Println("Verified Headscale identity, DERP HTTPS and STUN, API readiness and the site on both standby addresses.")
	return nil
}
