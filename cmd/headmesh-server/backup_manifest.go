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
	if len(args) == 0 || (args[0] != "capture" && args[0] != "verify") {
		return errors.New("usage: headmesh-server backup-manifest capture|verify [flags]")
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	sqlite := fs.String("sqlite", "/var/lib/headscale/db.sqlite", "Headscale SQLite database")
	noise := fs.String("noise-key", "/var/lib/headscale/noise_private.key", "Headscale Noise private key")
	derp := fs.String("derp-key", "/var/lib/headscale/derp_server_private.key", "restored DERP private key")
	manifest := fs.String("manifest", "", "published manifest file for verification")
	if err := fs.Parse(args[1:]); err != nil {
		return fmt.Errorf("parse backup manifest flags: %w", err)
	}
	if fs.NArg() != 0 || (args[0] == "verify" && *manifest == "") || (args[0] == "capture" && *manifest != "") {
		return errors.New("verify requires -manifest FILE, and capture writes the manifest to stdout")
	}
	var expected recovery.Manifest
	if args[0] == "verify" {
		f, err := os.Open(*manifest) // #nosec G304 G703 -- Operator explicitly selects the local manifest file.
		if err != nil {
			return fmt.Errorf("open backup manifest: %w", err)
		}
		defer func() { _ = f.Close() }()
		raw, err := io.ReadAll(io.LimitReader(f, 16385))
		if err != nil || len(raw) > 16384 {
			return errors.New("could not read backup manifest or it is too large")
		}
		expected, err = recovery.ReadManifest(bytes.NewReader(raw))
		if err != nil {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(parent, 60*time.Second)
	defer cancel()
	db, err := pgx.Connect(ctx, os.Getenv("HEADMESH_DATABASE_URL"))
	if err != nil {
		return errors.New("connect to PostgreSQL for backup evidence")
	}
	defer func() { _ = db.Close(ctx) }()
	if args[0] == "capture" {
		evidence, err := recovery.Capture(ctx, db, *sqlite, *noise)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(evidence); err != nil {
			return fmt.Errorf("write backup manifest: %w", err)
		}
		return nil
	}
	if err := recovery.Verify(ctx, db, *sqlite, *noise, *derp, expected); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(stdout, "Backup manifest verified"); err != nil {
		return fmt.Errorf("write backup verification result: %w", err)
	}
	return nil
}
