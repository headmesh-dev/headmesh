package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/headmesh-dev/headmesh/internal/recovery"
)

func backupManifest(parent context.Context, args []string, stdout io.Writer) error {
	const usage = "usage: headmesh-server backup-manifest capture|verify|publish|fetch|audit [flags]"
	if len(args) == 0 {
		return errors.New(usage)
	}
	action := args[0]
	switch action {
	case "capture", "verify", "publish", "fetch", "audit":
	default:
		return errors.New(usage)
	}
	fs := flag.NewFlagSet(action, flag.ContinueOnError)
	sqlite := fs.String("sqlite", "/var/lib/headscale/db.sqlite", "Headscale SQLite database")
	noise := fs.String("noise-key", "/var/lib/headscale/noise_private.key", "Headscale Noise private key")
	derp := fs.String("derp-key", "/var/lib/headscale/derp_server_private.key", "restored DERP private key")
	manifest := fs.String("manifest", "", "published manifest file for verification")
	litestream := fs.String("litestream-config", "/etc/headmesh/backup/litestream.yml", "Litestream configuration for publishing")
	netrc := fs.String("netrc", "", "private netrc credential file for hm-pg")
	if err := fs.Parse(args[1:]); err != nil {
		return fmt.Errorf("parse backup manifest flags: %w", err)
	}
	if fs.NArg() != 0 || (action == "verify") != (*manifest != "") || (action == "publish" || action == "fetch") != (*netrc != "") {
		return errors.New("verify requires -manifest FILE; publish and fetch require -netrc FILE")
	}
	var expected recovery.Manifest
	if action == "verify" {
		var err error
		expected, err = readStoredManifest(*manifest)
		if err != nil {
			return err
		}
	}
	timeout := 60 * time.Second
	switch action {
	case "publish":
		timeout = 2 * time.Minute
	case "audit":
		timeout = 15 * time.Minute
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	if action == "fetch" {
		evidence, err := recovery.Fetch(ctx, os.Getenv("HEADMESH_R2_ENDPOINT"), *netrc)
		if err != nil {
			return err
		}
		return writeManifest(stdout, evidence)
	}
	db, err := pgx.Connect(ctx, os.Getenv("HEADMESH_DATABASE_URL"))
	if err != nil {
		return errors.New("connect to PostgreSQL for backup evidence")
	}
	defer func() { _ = db.Close(ctx) }()
	if action == "audit" {
		orgs, events, err := recovery.VerifyAudit(ctx, db)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(stdout, "Verified %d audit events across %d organizations and the tenant-less chain\n", events, orgs); err != nil {
			return fmt.Errorf("write audit verification result: %w", err)
		}
		return nil
	}
	if action == "publish" {
		if err := recovery.Publish(ctx, db, recovery.PublishOptions{
			SQLite: *sqlite, NoiseKey: *noise, LitestreamConfig: *litestream,
			Endpoint: os.Getenv("HEADMESH_R2_ENDPOINT"), Netrc: *netrc,
		}); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(stdout, "Published recovery manifest"); err != nil {
			return fmt.Errorf("write publication result: %w", err)
		}
		return nil
	}
	if action == "capture" {
		evidence, err := recovery.Capture(ctx, db, *sqlite, *noise)
		if err != nil {
			return err
		}
		return writeManifest(stdout, evidence)
	}
	if err := recovery.Verify(ctx, db, *sqlite, *noise, *derp, expected); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(stdout, "Backup manifest verified"); err != nil {
		return fmt.Errorf("write backup verification result: %w", err)
	}
	return nil
}

func writeManifest(out io.Writer, evidence recovery.Manifest) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(evidence); err != nil {
		return fmt.Errorf("write backup manifest: %w", err)
	}
	return nil
}

func readStoredManifest(path string) (recovery.Manifest, error) {
	if path == "" {
		return recovery.Manifest{}, errors.New("recovery requires -manifest FILE")
	}
	f, err := os.Open(path) // #nosec G304 G703 -- Operator explicitly selects the local manifest file.
	if err != nil {
		return recovery.Manifest{}, fmt.Errorf("open backup manifest: %w", err)
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, recovery.MaxManifestBytes+1))
	if err != nil || len(raw) > recovery.MaxManifestBytes {
		return recovery.Manifest{}, errors.New("could not read backup manifest or it is too large")
	}
	return recovery.ReadManifest(bytes.NewReader(raw))
}
