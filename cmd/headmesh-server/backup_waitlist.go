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

func backupWaitlist(parent context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 || (args[0] != "publish" && args[0] != "stage") {
		return errors.New("usage: headmesh-server backup-waitlist publish|stage [flags]")
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	netrc := fs.String("netrc", "", "private R2 credential file for hm-keys")
	var db, owner, standby, identity, output string
	if args[0] == "publish" {
		fs.StringVar(&db, "sqlite", "/var/lib/headmesh/prod/waitlist/waitlist.db", "live production waitlist SQLite database")
		fs.StringVar(&owner, "recipient", os.Getenv("OWNER_RECIPIENT"), "owner's public age recipient")
		fs.StringVar(&standby, "recovery-recipient", os.Getenv("RECOVERY_RECIPIENT"), "separate public recovery recipient")
	} else {
		fs.StringVar(&identity, "identity", "", "private age identity file")
		fs.StringVar(&output, "output", "", "new private snapshot path")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return fmt.Errorf("parse waitlist backup flags: %w", err)
	}
	if fs.NArg() != 0 || *netrc == "" {
		return errors.New("backup-waitlist requires a private -netrc file and accepts flags only")
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	endpoint := os.Getenv("HEADMESH_R2_ENDPOINT")
	message := "Published the encrypted waitlist snapshot"
	if args[0] == "publish" {
		if err := recovery.PublishWaitlist(ctx, db, []string{owner, standby}, endpoint, *netrc); err != nil {
			return err
		}
	} else {
		if err := recovery.StageWaitlist(ctx, output, identity, endpoint, *netrc); err != nil {
			return err
		}
		message = "Staged the verified waitlist snapshot"
	}
	if _, err := fmt.Fprintln(stdout, message); err != nil {
		return fmt.Errorf("write waitlist backup result: %w", err)
	}
	return nil
}
