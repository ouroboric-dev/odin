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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Main(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
