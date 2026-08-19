// Package migrations embeds the SQL schema migrations into the binary.
//
// Embedding means a deployed artifact carries exactly the migrations it was
// built with, so there is no way to run a different migration directory against
// a database - that is what docs/14 §36 means by version-controlled,
// deterministic migrations.
//
// File format:
//
//	<utc timestamp>_<description>.sql
//
//	-- +migrate Up
//	<statements applied when migrating forward>
//
//	-- +migrate Down
//	<statements applied when rolling back>
//
// Add `-- +migrate NoTransaction` for statements PostgreSQL refuses to run
// inside a transaction (for example CREATE INDEX CONCURRENTLY); such migrations
// must be written idempotently, because they cannot be rolled back atomically.
//
// Never edit a migration that has been applied anywhere - add a new one.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS

// Dir is the path within FS that holds the migration files.
const Dir = "."
