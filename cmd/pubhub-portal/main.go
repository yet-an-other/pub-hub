// Command pubhub-portal runs the Portal on hub.
package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/yet-an-other/pub-hub/internal/portal"
)

func main() {
	configPath := flag.String("config", "/etc/pubhub/portal.toml", "path to portal.toml")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := portal.Run(ctx, *configPath, os.Stdout); err != nil {
		os.Exit(1)
	}
}
