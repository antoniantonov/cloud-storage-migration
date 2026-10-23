package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"

	"github.com/antoniantonov/cloud-storage-migration/src/internal/config"
	"github.com/antoniantonov/cloud-storage-migration/src/internal/migration"
	"github.com/antoniantonov/cloud-storage-migration/src/internal/state"
)

type Dependencies struct {
	Source      func(context.Context, config.Config, io.Writer) (migration.Source, error)
	Destination func(context.Context, config.Config, io.Writer) (migration.Destination, error)
}

func Run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer, deps Dependencies) (exitCode int) {
	cfg, err := config.Parse(args, getenv)
	if errors.Is(err, flag.ErrHelp) {
		if _, err := io.WriteString(stdout, config.Usage); err != nil {
			fmt.Fprintf(stderr, "write help: %v\n", err)
			return 1
		}
		return 0
	}
	if err != nil {
		fmt.Fprintf(stderr, "configuration: %v\n\n%s", err, config.Usage)
		return 2
	}
	unlock, err := state.Lock(cfg.AuthDir)
	if err != nil {
		fmt.Fprintf(stderr, "state: %v\n", err)
		return 1
	}
	defer func() {
		if err := unlock(); err != nil {
			fmt.Fprintf(stderr, "release state lock: %v\n", err)
			exitCode = 1
		}
	}()
	source, err := deps.Source(ctx, cfg, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "OneDrive: %v\n", err)
		return 1
	}
	if cfg.Mode == config.List {
		items, err := source.Inventory(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "OneDrive inventory failed: %v\n", err)
			return 1
		}
		if err := ctx.Err(); err != nil {
			fmt.Fprintf(stderr, "listing interrupted: %v\n", err)
			return 1
		}
		sort.Slice(items, func(i, j int) bool {
			if items[i].Path == items[j].Path {
				return items[i].ID < items[j].ID
			}
			return items[i].Path < items[j].Path
		})
		encoder := json.NewEncoder(stdout)
		for _, item := range items {
			if err := ctx.Err(); err != nil {
				fmt.Fprintf(stderr, "listing interrupted: %v\n", err)
				return 1
			}
			if err := encoder.Encode(item); err != nil {
				fmt.Fprintf(stderr, "write inventory: %v\n", err)
				return 1
			}
		}
		return 0
	}
	newDestination := func(ctx context.Context) (migration.Destination, error) {
		return deps.Destination(ctx, cfg, stderr)
	}
	report := func(result migration.Result) error {
		if _, err := fmt.Fprintf(stderr, "%s: %q (%q)", result.Status, result.Name, result.ID); err != nil {
			return err
		}
		if result.Err != nil {
			if _, err := fmt.Fprintf(stderr, ": %v", result.Err); err != nil {
				return err
			}
		}
		if result.TempPath != "" {
			if _, err := fmt.Fprintf(stderr, "; retained local file: %q", result.TempPath); err != nil {
				return err
			}
		}
		_, err := fmt.Fprintln(stderr)
		return err
	}
	summary, err := migration.Run(ctx, cfg.ItemIDs, source, newDestination, cfg.TempCopyDir, report)
	_, outputErr := fmt.Fprintf(stderr, "Copy summary: %d copied, %d skipped, %d failed\n", summary.Copied, summary.Skipped, summary.Failed)
	if err != nil {
		fmt.Fprintf(stderr, "copy stopped: %v\n", err)
	}
	if err != nil || outputErr != nil || summary.Failed > 0 {
		return 1
	}
	return 0
}
