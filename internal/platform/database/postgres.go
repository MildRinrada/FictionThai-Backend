// Package database owns the PostgreSQL connection pool and schema migrations.
//
// PostgreSQL is the source of truth for all persistent business data
// (docs/07 - System Architecture.md §17, docs/08 - Database Design.md §1.1).
//
// The pool is a *sql.DB backed by the pgx stdlib driver rather than a raw
// pgxpool. docs/08 records GORM as the planned ORM and GORM builds on
// database/sql, so this keeps that door open without committing to it now while
// leaving plain SQL available for the hot reader queries.
package database

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/fictionthai/fictionthai/backend/internal/config"
)

// DB wraps the connection pool.
type DB struct {
	*sql.DB
}

// Connect opens the application pool and verifies it with a ping, so a
// misconfigured DATABASE_URL fails at startup rather than on the first request.
func Connect(ctx context.Context, cfg config.Database) (*DB, error) {
	return open(ctx, cfg, false)
}

// ConnectForMigrations opens a pool that uses PostgreSQL's simple query
// protocol.
//
// The default extended protocol sends each statement as a prepared statement,
// which cannot contain multiple commands - so a migration file holding several
// statements would fail. Migrations are the only place that needs this, and
// running the application on the simple protocol would give up server-side
// prepared statements, so the two pools are kept separate.
func ConnectForMigrations(ctx context.Context, cfg config.Database) (*DB, error) {
	return open(ctx, cfg, true)
}

func open(ctx context.Context, cfg config.Database, simpleProtocol bool) (*DB, error) {
	connConfig, err := pgx.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	if simpleProtocol {
		connConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	}

	pool := stdlib.OpenDB(*connConfig)
	pool.SetMaxOpenConns(cfg.MaxOpenConns)
	pool.SetMaxIdleConns(cfg.MaxIdleConns)
	pool.SetConnMaxLifetime(cfg.ConnMaxLifetime)

	pingCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()

	if err := pool.PingContext(pingCtx); err != nil {
		// Close the half-open pool so a failed startup leaks no connections.
		_ = pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	return &DB{DB: pool}, nil
}

// Ping reports whether the database is reachable. Used by the readiness probe
// (docs/14 - Infrastructure & Deployment.md §45).
func (db *DB) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return db.PingContext(ctx)
}

// Close releases the pool during graceful shutdown (docs/14 §46).
func (db *DB) Close() error { return db.DB.Close() }
