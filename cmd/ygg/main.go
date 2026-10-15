// Command ygg is the Yggdrasil factory's command line.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/ouroboric-dev/odin/internal/cli"
)

func main() {
	// SIGHUP too: a closed terminal or a dropped SSH session stops the run cleanly, killing the
	// running node's processes, instead of killing ygg and leaving them behind.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	code := cli.Main(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
