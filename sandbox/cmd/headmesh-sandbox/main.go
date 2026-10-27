// Command headmesh-sandbox is the host CLI for the agent sandbox. This is a
// placeholder while the full CLI is written.
package main

import (
	"fmt"
	"os"
)

var version = "dev"

func main() {
	fmt.Fprintln(os.Stderr, "headmesh-sandbox", version, ": CLI under construction")
	os.Exit(2)
}
