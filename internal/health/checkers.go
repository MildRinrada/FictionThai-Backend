package health

import (
	"context"

	"github.com/fictionthai/fictionthai/backend/internal/platform/cache"
	"github.com/fictionthai/fictionthai/backend/internal/platform/database"
)

// PostgresChecker probes the primary database.
type PostgresChecker struct{ DB *database.DB }

func (c PostgresChecker) Name() string  { return "postgres" }
func (c PostgresChecker) Enabled() bool { return c.DB != nil }
func (c PostgresChecker) Check(ctx context.Context) error {
	return c.DB.Ping(ctx)
}

// RedisChecker probes the cache. Redis is optional, so Enabled reflects whether
// this deployment configured it at all (docs/07 §18).
type RedisChecker struct{ Client *cache.Client }

func (c RedisChecker) Name() string  { return "redis" }
func (c RedisChecker) Enabled() bool { return c.Client.Enabled() }
func (c RedisChecker) Check(ctx context.Context) error {
	return c.Client.Ping(ctx)
}
