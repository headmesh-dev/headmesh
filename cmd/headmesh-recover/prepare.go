package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/headmesh-dev/headmesh/internal/recovery"
)

func runPrepareInputs(ctx context.Context, args []string, out io.Writer) error {
	flags := flag.NewFlagSet("prepare-inputs", flag.ContinueOnError)
	var opt recovery.PrepareOptions
	flags.StringVar(&opt.Credentials, "credentials", "", "private recovery directory containing manifest.netrc, keys.netrc and recovery.agekey")
	flags.StringVar(&opt.OutputDir, "output-dir", "", "new directory for the verified manifest and encrypted input files")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse input preparation flags: %w", err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("prepare-inputs accepts flags only")
	}
	opt.Endpoint = os.Getenv("HEADMESH_R2_ENDPOINT")
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	report, err := recovery.PrepareInputs(ctx, opt)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(out).Encode(report); err != nil {
		return fmt.Errorf("write recovery input preparation report: %w", err)
	}
	return nil
}
