// Package cache owns the Redis client.
//
// Redis is auxiliary infrastructure, never a source of truth
// (docs/07 - System Architecture.md §17, docs/14 - Infrastructure & Deployment.md §17).
// It is also optional: docs/07 §18 says Redis should be introduced when there
// is a concrete use case, so an unset REDIS_URL yields a disabled client and
// the API still serves traffic.
package cache

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fictionthai/fictionthai/backend/internal/config"
)

// Client wraps the Redis connection. A Client with a nil rdb is "disabled" and
// every method is a safe no-op, so callers never need a nil check.
type Client struct {
	rdb *redis.Client
}

// Enabled reports whether Redis is configured for this deployment.
func (c *Client) Enabled() bool { return c != nil && c.rdb != nil }

// Redis exposes the underlying client for packages that need it (rate limiting,
// caching, job queues). Returns nil when Redis is disabled.
func (c *Client) Redis() *redis.Client {
	if !c.Enabled() {
		return nil
	}
	return c.rdb
}

// Connect dials Redis and verifies the connection. When cfg.URL is empty it
// returns a disabled client and no error.
func Connect(ctx context.Context, cfg config.Redis) (*Client, error) {
	if !cfg.Enabled() {
		return &Client{}, nil
	}

	opts, err := redis.ParseURL(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}

	rdb := redis.NewClient(opts)

	pingCtx, cancel := context.WithTimeout(ctx, cfg.ConnectTimeout)
	defer cancel()

	if err := rdb.Ping(pingCtx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}

	return &Client{rdb: rdb}, nil
}

// Ping reports whether Redis is reachable. Returns nil when Redis is disabled,
// because "not configured" is not a failure.
func (c *Client) Ping(ctx context.Context) error {
	if !c.Enabled() {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return c.rdb.Ping(ctx).Err()
}

// Close releases the connection during graceful shutdown.
func (c *Client) Close() error {
	if !c.Enabled() {
		return nil
	}
	return c.rdb.Close()
}
