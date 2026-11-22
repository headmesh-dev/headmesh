package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/headmesh-dev/headmesh/internal/recovery"
)

func backupKeys(parent context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: headmesh-server backup-keys pack|publish|stage [flags]")
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	config := fs.String("config", "/etc/headscale/config.yaml", "active Headscale configuration")
	noise := fs.String("noise-key", "/var/lib/headscale/noise_private.key", "Headscale Noise key")
	derp := fs.String("derp-key", "/var/lib/headscale/derp_server_private.key", "Headscale DERP key")
	owner := fs.String("recipient", "", "owner's public age recipient")
	standby := fs.String("recovery-recipient", "", "separate public recovery recipient")
	format := fs.String("format", "age", "age or sops envelope")
	netrc := fs.String("netrc", "", "private R2 credential file for hm-keys")
	bundle := fs.String("bundle", "", "local encrypted bundle to stage")
	manifest := fs.String("manifest", "", "published manifest to match at staging")
	identity := fs.String("identity", "", "private age identity file for staging")
	output := fs.String("output-dir", "", "new private staging directory")
	if err := fs.Parse(args[1:]); err != nil {
		return fmt.Errorf("parse backup key flags: %w", err)
	}
	if fs.NArg() != 0 {
		return errors.New("backup-keys accepts flags after the action")
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	endpoint := os.Getenv("HEADMESH_R2_ENDPOINT")
	recipients := []string{*owner, *standby}
	source := recovery.KeyBundleSources{NoiseKey: *noise, DERPKey: *derp, Configuration: *config}

	switch args[0] {
	case "pack":
		return recovery.PackKeyBundle(ctx, source, recipients, *format, stdout)
	case "publish":
		if *format != "age" || *netrc == "" {
			return errors.New("R2 publication requires an age bundle and -netrc FILE")
		}
		if err := recovery.PublishKeyBundle(ctx, source, recipients, endpoint, *netrc); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(stdout, "Published encrypted Headscale key bundle"); err != nil {
			return fmt.Errorf("write key bundle publication result: %w", err)
		}
	case "stage":
		expected, err := readStoredManifest(*manifest)
		if err != nil {
			return err
		}
		if err := recovery.StageKeyBundle(ctx, expected, recovery.KeyBundleStage{
			BundledFile: *bundle, IdentityFile: *identity, Format: *format, OutputDir: *output,
			Endpoint: endpoint, Netrc: *netrc,
		}); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(stdout, "Staged verified Headscale keys and configuration"); err != nil {
			return fmt.Errorf("write key bundle staging result: %w", err)
		}
	default:
		return errors.New("usage: headmesh-server backup-keys pack|publish|stage [flags]")
	}
	return nil
}
