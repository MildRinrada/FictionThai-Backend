package ratelimit

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// redisKeyPrefix namespaces limiter keys so they never collide with cache keys
// in a shared Redis instance.
const redisKeyPrefix = "ratelimit:"

// RedisLimiter is a fixed-window limiter shared by every API instance.
//
// One round trip performs INCR + (conditional) EXPIRE in a pipeline. The
// counter's TTL is set when it is first created, which makes the window start
// at the first request rather than at a wall-clock boundary.
type RedisLimiter struct {
	rdb *redis.Client
}

// NewRedisLimiter wraps an existing Redis client. The client's lifecycle is
// owned by the caller, so Close here is a no-op.
func NewRedisLimiter(rdb *redis.Client) *RedisLimiter {
	return &RedisLimiter{rdb: rdb}
}

func (l *RedisLimiter) Allow(ctx context.Context, key string, policy Policy) Result {
	redisKey := redisKeyPrefix + policy.Name + ":" + key

	ctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()

	pipe := l.rdb.TxPipeline()
	incr := pipe.Incr(ctx, redisKey)
	// NX so a long-running window is not extended by every subsequent request,
	// which would let a steady stream of traffic keep the window open forever.
	pipe.ExpireNX(ctx, redisKey, policy.Window)

	if _, err := pipe.Exec(ctx); err != nil {
		// Fail open: a limiter outage must not take down reading.
		return allowed(policy, 0)
	}

	count := incr.Val()
	if count > int64(policy.Limit) {
		ttl, err := l.rdb.TTL(ctx, redisKey).Result()
		if err != nil || ttl < 0 {
			ttl = policy.Window
		}
		return denied(policy, ttl)
	}
	return allowed(policy, count)
}

// Peek reads the current window's count without incrementing it.
func (l *RedisLimiter) Peek(ctx context.Context, key string, policy Policy) Result {
	redisKey := redisKeyPrefix + policy.Name + ":" + key

	ctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()

	count, err := l.rdb.Get(ctx, redisKey).Int64()
	if err != nil {
		// Missing key or an outage both read as an untouched window - fail open,
		// like Allow.
		return allowed(policy, 0)
	}
	if count >= int64(policy.Limit) {
		ttl, err := l.rdb.TTL(ctx, redisKey).Result()
		if err != nil || ttl < 0 {
			ttl = policy.Window
		}
		return denied(policy, ttl)
	}
	return allowed(policy, count)
}

// Close is a no-op; the Redis client is owned by the cache package.
func (l *RedisLimiter) Close() error { return nil }
