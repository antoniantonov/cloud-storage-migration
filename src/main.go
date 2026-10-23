package main

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/antoniantonov/cloud-storage-migration/src/internal/app"
	"github.com/antoniantonov/cloud-storage-migration/src/internal/config"
	"github.com/antoniantonov/cloud-storage-migration/src/internal/gdrive"
	"github.com/antoniantonov/cloud-storage-migration/src/internal/migration"
	"github.com/antoniantonov/cloud-storage-migration/src/internal/onedrive"
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return app.Run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr, app.Dependencies{
		Source: func(ctx context.Context, cfg config.Config, stderr io.Writer) (migration.Source, error) {
			return onedrive.New(ctx, cfg, stderr)
		},
		Destination: func(ctx context.Context, cfg config.Config, stderr io.Writer) (migration.Destination, error) {
			return gdrive.New(ctx, cfg, stderr)
		},
	})
}
