// Command headmesh is the Headmesh CLI.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"charm.land/fang/v2"

	"github.com/headmesh-dev/headmesh/cli"
)

var version = "dev"

func main() { os.Exit(run()) }

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := fang.Execute(ctx, cli.NewRoot(version, cli.Deps{}), fang.WithVersion(version)); err != nil {
		return 1 // fang has already printed the error
	}
	return 0
}
