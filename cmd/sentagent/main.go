package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/busurmedia/sentnodes-agent/internal/runner"
)

// version is set at build time via -ldflags "-X main.version=vX.Y.Z".
var version = "dev"

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	log.Printf("SentNodes Agent %s starting", version)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := runner.Run(ctx, version); err != nil {
		log.Printf("fatal: %v", err)
		os.Exit(1)
	}
}
