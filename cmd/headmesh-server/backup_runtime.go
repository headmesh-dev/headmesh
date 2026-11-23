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

func backupRuntime(parent context.Context, args []string, out io.Writer) error {
	if len(args) == 0 || (args[0] != "pack" && args[0] != "publish" && args[0] != "stage") {
		return errors.New("usage: headmesh-server backup-runtime pack|publish|stage [flags]")
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	var src recovery.RuntimeSources
	var opt recovery.RuntimeStage
	var owner, standby, manifestPath string
	if args[0] != "pack" {
		fs.StringVar(&opt.Netrc, "netrc", "", "private R2 credential file for hm-keys")
	}
	if args[0] == "stage" {
		fs.StringVar(&manifestPath, "manifest", "", "verified manifest used to restore the standby")
		fs.StringVar(&opt.Identity, "identity", "", "private age identity file")
		fs.StringVar(&opt.Bundle, "bundle", "", "local encrypted runtime snapshot")
		fs.StringVar(&opt.OutputDir, "output-dir", "", "new private runtime staging directory")
	} else {
		fs.StringVar(&src.EtcDir, "etc-dir", "/etc", "root of the deployed configuration files")
		fs.StringVar(&src.NoiseKey, "noise-key", "/var/lib/headscale/noise_private.key", "active Headscale Noise key")
		fs.StringVar(&owner, "recipient", os.Getenv("OWNER_RECIPIENT"), "owner's public age recipient")
		fs.StringVar(&standby, "recovery-recipient", os.Getenv("RECOVERY_RECIPIENT"), "separate public recovery recipient")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return fmt.Errorf("parse runtime backup flags: %w", err)
	}
	if fs.NArg() != 0 {
		return errors.New("backup-runtime accepts flags after the action")
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	opt.Endpoint = os.Getenv("HEADMESH_R2_ENDPOINT")
	message := "Published the encrypted runtime snapshot"
	switch args[0] {
	case "pack":
		return recovery.PackRuntime(ctx, src, []string{owner, standby}, out)
	case "publish":
		if err := recovery.PublishRuntime(ctx, src, []string{owner, standby}, opt.Endpoint, opt.Netrc); err != nil {
			return err
		}
	case "stage":
		manifest, err := readStoredManifest(manifestPath)
		if err != nil {
			return err
		}
		if err := recovery.StageRuntime(ctx, manifest, opt); err != nil {
			return err
		}
		message = "Staged the verified runtime configuration"
	}
	if _, err := fmt.Fprintln(out, message); err != nil {
		return fmt.Errorf("write runtime backup result: %w", err)
	}
	return nil
}
