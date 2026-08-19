package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/fictionthai/fictionthai/backend/migrations"
)

// migrationsTable is the ledger of applied migrations.
const migrationsTable = "schema_migrations"

// advisoryLockKey serialises migration runs across processes. Two instances
// deploying at once must not apply the same migration twice
// (docs/14 §36: migrations are a deliberate, ordered deployment step).
const advisoryLockKey int64 = 8_150_723_641_001

// Directives recognised inside a migration file.
const (
	directiveUp            = "-- +migrate Up"
	directiveDown          = "-- +migrate Down"
	directiveNoTransaction = "-- +migrate NoTransaction"
)

// Migration is one versioned schema change.
type Migration struct {
	Version int64
	Name    string
	Up      string
	Down    string
	// NoTransaction marks statements that PostgreSQL refuses to run inside a
	// transaction, such as CREATE INDEX CONCURRENTLY.
	NoTransaction bool
}

// Migrator applies embedded SQL migrations.
//
// This is deliberately a small in-repo runner rather than a migration library:
// the available libraries pull driver support for databases this project does
// not use (MongoDB, SQLite, ClickHouse) into the build graph, which conflicts
// with the dependency and supply-chain guidance in docs/11 §65.
//
// Guarantees:
//   - deterministic order (numeric version, validated unique)
//   - one transaction per migration, unless the file opts out
//   - a PostgreSQL advisory lock, so concurrent deploys serialise
//   - the ledger row is written in the same transaction as the DDL, so a
//     failed migration is never recorded as applied
type Migrator struct {
	db  *sql.DB
	set []Migration
}

// NewMigrator loads and validates the embedded migrations.
func NewMigrator(db *sql.DB) (*Migrator, error) {
	set, err := load(migrations.FS, migrations.Dir)
	if err != nil {
		return nil, err
	}
	return &Migrator{db: db, set: set}, nil
}

// Up applies every pending migration in order.
func (m *Migrator) Up(ctx context.Context) ([]Migration, error) {
	unlock, err := m.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()

	if err := m.ensureLedger(ctx); err != nil {
		return nil, err
	}

	applied, err := m.appliedVersions(ctx)
	if err != nil {
		return nil, err
	}

	var ran []Migration
	for _, migration := range m.set {
		if _, done := applied[migration.Version]; done {
			continue
		}
		if migration.Up == "" {
			return ran, fmt.Errorf("migration %d (%s) has no Up section", migration.Version, migration.Name)
		}
		if err := m.apply(ctx, migration); err != nil {
			return ran, err
		}
		ran = append(ran, migration)
	}
	return ran, nil
}

// Down rolls back the most recently applied migration. Intended for development
// and staging; docs/14 §48 warns that not every production change is reversible.
func (m *Migrator) Down(ctx context.Context) (*Migration, error) {
	unlock, err := m.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()

	if err := m.ensureLedger(ctx); err != nil {
		return nil, err
	}

	current, err := m.Version(ctx)
	if err != nil {
		return nil, err
	}
	if current == 0 {
		return nil, nil
	}

	migration, ok := m.find(current)
	if !ok {
		return nil, fmt.Errorf("applied version %d has no migration file; refusing to roll back blindly", current)
	}
	if strings.TrimSpace(migration.Down) == "" {
		return nil, fmt.Errorf("migration %d (%s) has no Down section", migration.Version, migration.Name)
	}

	if err := m.revert(ctx, migration); err != nil {
		return nil, err
	}
	return &migration, nil
}

// Status describes each known migration and whether it has been applied.
type Status struct {
	Version int64
	Name    string
	Applied bool
}

// Status reports the state of every migration.
func (m *Migrator) Status(ctx context.Context) ([]Status, error) {
	if err := m.ensureLedger(ctx); err != nil {
		return nil, err
	}
	applied, err := m.appliedVersions(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]Status, 0, len(m.set))
	for _, migration := range m.set {
		_, done := applied[migration.Version]
		out = append(out, Status{Version: migration.Version, Name: migration.Name, Applied: done})
	}
	return out, nil
}

// Version returns the highest applied version, or 0 when none are applied.
func (m *Migrator) Version(ctx context.Context) (int64, error) {
	if err := m.ensureLedger(ctx); err != nil {
		return 0, err
	}

	var version sql.NullInt64
	query := fmt.Sprintf("SELECT MAX(version) FROM %s", migrationsTable)
	if err := m.db.QueryRowContext(ctx, query).Scan(&version); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	if !version.Valid {
		return 0, nil
	}
	return version.Int64, nil
}

// Pending reports how many migrations have not yet been applied.
func (m *Migrator) Pending(ctx context.Context) (int, error) {
	statuses, err := m.Status(ctx)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, s := range statuses {
		if !s.Applied {
			count++
		}
	}
	return count, nil
}

func (m *Migrator) find(version int64) (Migration, bool) {
	for _, migration := range m.set {
		if migration.Version == version {
			return migration, true
		}
	}
	return Migration{}, false
}

func (m *Migrator) ensureLedger(ctx context.Context) error {
	query := fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			version     BIGINT      PRIMARY KEY,
			name        TEXT        NOT NULL,
			applied_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		)`, migrationsTable)

	if _, err := m.db.ExecContext(ctx, query); err != nil {
		return fmt.Errorf("create %s: %w", migrationsTable, err)
	}
	return nil
}

func (m *Migrator) appliedVersions(ctx context.Context) (map[int64]struct{}, error) {
	query := fmt.Sprintf("SELECT version FROM %s", migrationsTable)
	rows, err := m.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	applied := map[int64]struct{}{}
	for rows.Next() {
		var version int64
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("scan applied migration: %w", err)
		}
		applied[version] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}
	return applied, nil
}

// apply runs one migration's Up section and records it.
func (m *Migrator) apply(ctx context.Context, migration Migration) error {
	record := fmt.Sprintf("INSERT INTO %s (version, name) VALUES ($1, $2)", migrationsTable)

	if migration.NoTransaction {
		// Outside a transaction there is a window where the DDL succeeded but
		// the ledger row has not been written. That is inherent to statements
		// PostgreSQL will not run transactionally; such migrations must be
		// written to be idempotent (IF NOT EXISTS).
		if _, err := m.db.ExecContext(ctx, migration.Up); err != nil {
			return fmt.Errorf("migration %d (%s): %w", migration.Version, migration.Name, err)
		}
		if _, err := m.db.ExecContext(ctx, record, migration.Version, migration.Name); err != nil {
			return fmt.Errorf("record migration %d: %w", migration.Version, err)
		}
		return nil
	}

	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration %d: %w", migration.Version, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, migration.Up); err != nil {
		return fmt.Errorf("migration %d (%s): %w", migration.Version, migration.Name, err)
	}
	if _, err := tx.ExecContext(ctx, record, migration.Version, migration.Name); err != nil {
		return fmt.Errorf("record migration %d: %w", migration.Version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %d: %w", migration.Version, err)
	}
	return nil
}

// revert runs one migration's Down section and removes its ledger row.
func (m *Migrator) revert(ctx context.Context, migration Migration) error {
	remove := fmt.Sprintf("DELETE FROM %s WHERE version = $1", migrationsTable)

	if migration.NoTransaction {
		if _, err := m.db.ExecContext(ctx, migration.Down); err != nil {
			return fmt.Errorf("roll back migration %d: %w", migration.Version, err)
		}
		_, err := m.db.ExecContext(ctx, remove, migration.Version)
		return err
	}

	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin rollback of %d: %w", migration.Version, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, migration.Down); err != nil {
		return fmt.Errorf("roll back migration %d: %w", migration.Version, err)
	}
	if _, err := tx.ExecContext(ctx, remove, migration.Version); err != nil {
		return fmt.Errorf("remove ledger row for %d: %w", migration.Version, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit rollback of %d: %w", migration.Version, err)
	}
	return nil
}

// lock takes a session-level advisory lock and returns its release function.
func (m *Migrator) lock(ctx context.Context) (func(), error) {
	conn, err := m.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire migration connection: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", advisoryLockKey); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("acquire migration lock: %w", err)
	}

	return func() {
		// Best effort: closing the connection releases a session lock anyway.
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", advisoryLockKey)
		_ = conn.Close()
	}, nil
}

// load reads, parses, and validates every migration in fsys.
func load(fsys fs.FS, dir string) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("read migrations directory: %w", err)
	}

	var set []Migration
	seen := map[int64]string{}

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}

		version, label, err := parseFilename(name)
		if err != nil {
			return nil, err
		}
		if previous, clash := seen[version]; clash {
			return nil, fmt.Errorf("migrations %q and %q share version %d", previous, name, version)
		}
		seen[version] = name

		raw, err := fs.ReadFile(fsys, path.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", name, err)
		}

		up, down, noTx, err := parseSections(string(raw))
		if err != nil {
			return nil, fmt.Errorf("migration %s: %w", name, err)
		}

		set = append(set, Migration{
			Version:       version,
			Name:          label,
			Up:            up,
			Down:          down,
			NoTransaction: noTx,
		})
	}

	sort.Slice(set, func(i, j int) bool { return set[i].Version < set[j].Version })
	return set, nil
}

// parseFilename splits `20260809000001_init_extensions.sql` into its version and
// label.
func parseFilename(name string) (int64, string, error) {
	base := strings.TrimSuffix(name, ".sql")
	prefix, label, found := strings.Cut(base, "_")
	if !found {
		return 0, "", fmt.Errorf("migration %q must be named <version>_<description>.sql", name)
	}
	version, err := strconv.ParseInt(prefix, 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("migration %q has a non-numeric version prefix", name)
	}
	if version <= 0 {
		return 0, "", fmt.Errorf("migration %q must have a positive version", name)
	}
	return version, label, nil
}

// parseSections splits a migration file on its Up/Down directives.
func parseSections(content string) (up, down string, noTransaction bool, err error) {
	var (
		upLines   []string
		downLines []string
		current   *[]string
	)

	for _, line := range strings.Split(content, "\n") {
		switch strings.TrimSpace(strings.TrimRight(line, "\r")) {
		case directiveUp:
			current = &upLines
			continue
		case directiveDown:
			current = &downLines
			continue
		case directiveNoTransaction:
			noTransaction = true
			continue
		}
		if current != nil {
			*current = append(*current, line)
		}
	}

	if len(upLines) == 0 {
		return "", "", false, errors.New(`missing "-- +migrate Up" directive`)
	}

	up = strings.TrimSpace(strings.Join(upLines, "\n"))
	down = strings.TrimSpace(strings.Join(downLines, "\n"))
	if up == "" {
		return "", "", false, errors.New("the Up section is empty")
	}
	return up, down, noTransaction, nil
}
