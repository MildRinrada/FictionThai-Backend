package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// Event is the queued unit of work: WHAT HAPPENED, not who to tell. The
// worker resolves recipients at delivery time, so a fiction retracted between
// enqueue and delivery notifies nobody, and the producing request never pays
// for fan-out (docs/07 §27, §37).
type Event struct {
	Kind    string    `json:"kind"`
	ActorID uuid.UUID `json:"actor_id"`

	NovelID   uuid.UUID `json:"novel_id,omitzero"`
	ChapterID uuid.UUID `json:"chapter_id,omitzero"`
	CommentID uuid.UUID `json:"comment_id,omitzero"`

	// Community entities (Phase 7) - distinct fields, because a community
	// comment id and a fiction comment id live in different tables.
	PostID             uuid.UUID `json:"post_id,omitzero"`
	CommunityCommentID uuid.UUID `json:"community_comment_id,omitzero"`

	// RecipientID is set only for events whose recipient is known at emit time
	// (a new follower notifies exactly the followed author).
	RecipientID uuid.UUID `json:"recipient_id,omitzero"`

	// EntityType/EntityID carry a polymorphic target for events that span
	// domains (Phase 8 moderation): the moderated object may be any of the six
	// reportable types, so the event names it the same way the notifications
	// table does.
	EntityType string    `json:"entity_type,omitzero"`
	EntityID   uuid.UUID `json:"entity_id,omitzero"`
}

// Event kinds - the internal event vocabulary of docs/07 §38.
const (
	KindChapterPublished        = "chapter_published"
	KindCommentCreated          = "comment_created"
	KindFollowerAdded           = "follower_added"
	KindCommunityCommentCreated = "community_comment_created"
	KindCommunityReactionAdded  = "community_reaction_added"
	KindModerationAction        = "moderation_action"
	KindAICompleted             = "ai_completed"

	KindSubscriptionActivated       = "subscription_activated"
	KindSubscriptionPaymentRejected = "subscription_payment_rejected"
)

// ErrQueueClosed reports Dequeue on a queue whose context ended.
var ErrQueueClosed = errors.New("notification queue closed")

// Queue carries events from API requests to the worker.
//
// Two implementations, mirroring the rate limiter (docs/07 §18: Redis is
// optional infrastructure):
//
//	Redis  - a Redis list; survives restarts and is shared across instances.
//	Memory - an in-process channel; correct for a single instance and the
//	         documented development fallback.
type Queue interface {
	// Enqueue never blocks the producing request beyond a bounded push.
	Enqueue(ctx context.Context, event Event) error

	// Dequeue blocks until an event arrives or ctx is cancelled, in which case
	// it returns ErrQueueClosed.
	Dequeue(ctx context.Context) (Event, error)
}

// ---------------------------------------------------------------------------
// Memory queue
// ---------------------------------------------------------------------------

// memoryCapacity bounds the in-process queue. Notification loss under absurd
// backlog is preferable to unbounded memory growth; the enqueue error is
// logged by the producer.
const memoryCapacity = 1024

type memoryQueue struct {
	ch chan Event
}

// NewMemoryQueue builds the in-process queue.
func NewMemoryQueue() Queue {
	return &memoryQueue{ch: make(chan Event, memoryCapacity)}
}

func (q *memoryQueue) Enqueue(_ context.Context, event Event) error {
	select {
	case q.ch <- event:
		return nil
	default:
		return fmt.Errorf("notification queue full (%d pending)", memoryCapacity)
	}
}

func (q *memoryQueue) Dequeue(ctx context.Context) (Event, error) {
	select {
	case event := <-q.ch:
		return event, nil
	case <-ctx.Done():
		return Event{}, ErrQueueClosed
	}
}

// ---------------------------------------------------------------------------
// Redis queue
// ---------------------------------------------------------------------------

// redisQueueKey is the list the API pushes to and the worker pops from
// (docs/09 §46 "Go API → Redis Queue → Worker").
const redisQueueKey = "fictionthai:notifications:events"

// redisPopTimeout bounds each BRPOP so the worker can notice a cancelled
// context between blocks.
const redisPopTimeout = 5 * time.Second

type redisQueue struct {
	rdb *redis.Client
}

// NewRedisQueue builds the Redis-backed queue.
func NewRedisQueue(rdb *redis.Client) Queue {
	return &redisQueue{rdb: rdb}
}

func (q *redisQueue) Enqueue(ctx context.Context, event Event) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode notification event: %w", err)
	}
	if err := q.rdb.LPush(ctx, redisQueueKey, payload).Err(); err != nil {
		return fmt.Errorf("enqueue notification event: %w", err)
	}
	return nil
}

func (q *redisQueue) Dequeue(ctx context.Context) (Event, error) {
	for {
		values, err := q.rdb.BRPop(ctx, redisPopTimeout, redisQueueKey).Result()
		if errors.Is(err, redis.Nil) {
			// Timed out with nothing queued; check the context and block again.
			if ctx.Err() != nil {
				return Event{}, ErrQueueClosed
			}
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return Event{}, ErrQueueClosed
			}
			return Event{}, fmt.Errorf("dequeue notification event: %w", err)
		}

		// BRPop returns [key, value].
		var event Event
		if err := json.Unmarshal([]byte(values[1]), &event); err != nil {
			return Event{}, fmt.Errorf("decode notification event: %w", err)
		}
		return event, nil
	}
}
