// Command migrate applies, rolls back, and inspects database migrations.
//
// It is a separate binary from the server so a deployment can run migrations as
// an explicit, ordered step before starting the application (docs/14 §36),
// rather than having every booting instance race to migrate the same database.
//
// Usage:
//
//	go run ./cmd/migrate up
//	go run ./cmd/migrate down
//	go run ./cmd/migrate status
//	go run ./cmd/migrate version
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/fictionthai/fictionthai/backend/internal/config"
	"github.com/fictionthai/fictionthai/backend/internal/platform/database"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return fmt.Errorf("usage: migrate <up|down|status|version>")
	}
	command := os.Args[1]

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx := context.Background()

	db, err := database.ConnectForMigrations(ctx, cfg.Database)
	if err != nil {
		return fmt.Errorf("connect to postgres: %w", err)
	}
	defer func() { _ = db.Close() }()

	migrator, err := database.NewMigrator(db.DB)
	if err != nil {
		return err
	}

	switch command {
	case "up":
		applied, err := migrator.Up(ctx)
		if err != nil {
			return err
		}
		if len(applied) == 0 {
			fmt.Println("no pending migrations")
			return nil
		}
		for _, m := range applied {
			fmt.Printf("applied  %d  %s\n", m.Version, m.Name)
		}

	case "down":
		reverted, err := migrator.Down(ctx)
		if err != nil {
			return err
		}
		if reverted == nil {
			fmt.Println("nothing to roll back")
			return nil
		}
		fmt.Printf("reverted %d  %s\n", reverted.Version, reverted.Name)

	case "status":
		statuses, err := migrator.Status(ctx)
		if err != nil {
			return err
		}
		if len(statuses) == 0 {
			fmt.Println("no migrations found")
			return nil
		}
		for _, s := range statuses {
			state := "pending"
			if s.Applied {
				state = "applied"
			}
			fmt.Printf("%-8s %d  %s\n", state, s.Version, s.Name)
		}

	case "version":
		v, err := migrator.Version(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("schema version: %d\n", v)

	default:
		return fmt.Errorf("unknown command %q: expected up, down, status, or version", command)
	}

	return nil
}
