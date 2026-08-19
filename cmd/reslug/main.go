// Command reslug migrates existing rows onto the token address scheme
// (docs/SLUGS.md, address review 2026-08): every fiction and chapter whose
// slug is not already a bare random token gets one.
//
// Title-based addresses froze the title as it stood at creation, so every
// rename left the URL asserting a stale name. New rows have been token-only
// since the review; this brings the rows from before it onto the same scheme,
// so the platform has ONE era of URL. Addresses this rewrites stop resolving -
// which is why printing is the default and writing takes a flag.
//
// Usage:
//
//	go run ./cmd/reslug           # dry run - prints what WOULD change
//	go run ./cmd/reslug --apply   # writes
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/fictionthai/fictionthai/backend/internal/config"
	"github.com/fictionthai/fictionthai/backend/internal/platform/database"
	"github.com/fictionthai/fictionthai/backend/pkg/slug"
)

func main() {
	apply := flag.Bool("apply", false, "write the new slugs instead of printing them")
	flag.Parse()

	if err := run(context.Background(), *apply); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, apply bool) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	db, err := database.ConnectForMigrations(ctx, cfg.Database)
	if err != nil {
		return fmt.Errorf("connect to postgres: %w", err)
	}
	defer func() { _ = db.Close() }()

	changed, err := reslugNovels(ctx, db.DB, apply)
	if err != nil {
		return err
	}
	chapters, err := reslugChapters(ctx, db.DB, apply)
	if err != nil {
		return err
	}

	verb := "would change"
	if apply {
		verb = "changed"
	}
	fmt.Printf("\n%s %d fiction slugs and %d chapter slugs\n", verb, changed, chapters)
	if !apply {
		fmt.Println("(dry run - re-run with --apply to write)")
	}
	return nil
}

func reslugNovels(ctx context.Context, db *sql.DB, apply bool) (int, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, slug, title FROM novels
		 WHERE deleted_at IS NULL ORDER BY created_at`)
	if err != nil {
		return 0, fmt.Errorf("list novels: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type work struct{ id, old, title string }
	var todo []work
	for rows.Next() {
		var item work
		if err := rows.Scan(&item.id, &item.old, &item.title); err != nil {
			return 0, err
		}
		// Token-era rows are already permanent addresses - never touched, so
		// running this twice changes nothing the second time.
		if !slug.IsToken(item.old) {
			todo = append(todo, item)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	count := 0
	for _, item := range todo {
		next, err := uniqueToken(ctx, func(candidate string) (bool, error) {
			var taken bool
			err := db.QueryRowContext(ctx,
				`SELECT EXISTS (SELECT 1 FROM novels WHERE slug = $1 AND id <> $2)`,
				candidate, item.id).Scan(&taken)
			return taken, err
		})
		if err != nil {
			return count, err
		}
		fmt.Printf("fiction %-40s %s\n         -> %s\n", trim(item.title, 40), item.old, next)
		if apply {
			if _, err := db.ExecContext(ctx,
				`UPDATE novels SET slug = $1 WHERE id = $2`, next, item.id); err != nil {
				return count, fmt.Errorf("update novel %s: %w", item.id, err)
			}
		}
		count++
	}
	return count, nil
}

func reslugChapters(ctx context.Context, db *sql.DB, apply bool) (int, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, novel_id, slug, COALESCE(title, '')
		FROM chapters WHERE deleted_at IS NULL ORDER BY created_at`)
	if err != nil {
		return 0, fmt.Errorf("list chapters: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type work struct{ id, novelID, old, title string }
	var todo []work
	for rows.Next() {
		var item work
		if err := rows.Scan(&item.id, &item.novelID, &item.old, &item.title); err != nil {
			return 0, err
		}
		if !slug.IsToken(item.old) {
			todo = append(todo, item)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	count := 0
	for _, item := range todo {
		next, err := uniqueToken(ctx, func(candidate string) (bool, error) {
			var taken bool
			err := db.QueryRowContext(ctx, `
				SELECT EXISTS (
					SELECT 1 FROM chapters
					WHERE novel_id = $1 AND slug = $2 AND id <> $3 AND deleted_at IS NULL
				)`, item.novelID, candidate, item.id).Scan(&taken)
			return taken, err
		})
		if err != nil {
			return count, err
		}
		fmt.Printf("chapter %-40s %s\n        -> %s\n", trim(item.title, 40), item.old, next)
		if apply {
			if _, err := db.ExecContext(ctx,
				`UPDATE chapters SET slug = $1 WHERE id = $2`, next, item.id); err != nil {
				return count, fmt.Errorf("update chapter %s: %w", item.id, err)
			}
		}
		count++
	}
	return count, nil
}

// uniqueToken draws fresh tokens until one is free - the same collision
// strategy the services use at create.
func uniqueToken(
	_ context.Context, taken func(string) (bool, error),
) (string, error) {
	for attempt := 0; attempt < 8; attempt++ {
		candidate, err := slug.NewToken()
		if err != nil {
			return "", err
		}
		clash, err := taken(candidate)
		if err != nil {
			return "", err
		}
		if !clash {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no free token after 8 attempts")
}

func trim(value string, max int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= max {
		return string(runes)
	}
	return string(runes[:max-1]) + "…"
}
