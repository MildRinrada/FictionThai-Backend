package ai

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// ErrQueueClosed reports Dequeue on a queue whose context ended.
var ErrQueueClosed = errors.New("ai queue closed")

// Queue carries ASYNC AI job ids from the API to the worker (docs/12 §27, §28;
// docs/09 §46 "Go API → Redis Queue → Worker"). It carries only the request id -
// never content - and the authoritative job state lives in the ai_requests row,
// so a lost or duplicated queue message can never corrupt a job: the worker
// re-reads the row and the queued-status guard makes claiming idempotent.
//
// Two implementations, mirroring the notifications queue and the rate limiter
// (docs/07 §18: Redis is optional infrastructure):
//
//	Redis  - a Redis list; survives restarts, shared across instances.
//	Memory - an in-process channel; the documented single-instance fallback.
type Queue interface {
	Enqueue(ctx context.Context, requestID uuid.UUID) error
	Dequeue(ctx context.Context) (uuid.UUID, error)
}

// ---------------------------------------------------------------------------
// Memory queue
// ---------------------------------------------------------------------------

const memoryCapacity = 1024

type memoryQueue struct {
	ch chan uuid.UUID
}

// NewMemoryQueue builds the in-process AI job queue.
func NewMemoryQueue() Queue {
	return &memoryQueue{ch: make(chan uuid.UUID, memoryCapacity)}
}

func (q *memoryQueue) Enqueue(_ context.Context, requestID uuid.UUID) error {
	select {
	case q.ch <- requestID:
		return nil
	default:
		return fmt.Errorf("ai job queue full (%d pending)", memoryCapacity)
	}
}

func (q *memoryQueue) Dequeue(ctx context.Context) (uuid.UUID, error) {
	select {
	case id := <-q.ch:
		return id, nil
	case <-ctx.Done():
		return uuid.Nil, ErrQueueClosed
	}
}

// ---------------------------------------------------------------------------
// Redis queue
// ---------------------------------------------------------------------------

const redisQueueKey = "fictionthai:ai:jobs"

const redisPopTimeout = 5 * time.Second

type redisQueue struct {
	rdb *redis.Client
}

// NewRedisQueue builds the Redis-backed AI job queue.
func NewRedisQueue(rdb *redis.Client) Queue {
	return &redisQueue{rdb: rdb}
}

func (q *redisQueue) Enqueue(ctx context.Context, requestID uuid.UUID) error {
	if err := q.rdb.LPush(ctx, redisQueueKey, requestID.String()).Err(); err != nil {
		return fmt.Errorf("enqueue ai job: %w", err)
	}
	return nil
}

func (q *redisQueue) Dequeue(ctx context.Context) (uuid.UUID, error) {
	for {
		values, err := q.rdb.BRPop(ctx, redisPopTimeout, redisQueueKey).Result()
		if errors.Is(err, redis.Nil) {
			if ctx.Err() != nil {
				return uuid.Nil, ErrQueueClosed
			}
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return uuid.Nil, ErrQueueClosed
			}
			return uuid.Nil, fmt.Errorf("dequeue ai job: %w", err)
		}
		id, err := uuid.Parse(values[1])
		if err != nil {
			// A malformed id cannot be processed; skip it rather than stall.
			continue
		}
		return id, nil
	}
}
