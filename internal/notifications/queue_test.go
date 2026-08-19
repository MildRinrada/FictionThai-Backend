package notifications

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestMemoryQueue_RoundTrip(t *testing.T) {
	q := NewMemoryQueue()
	ctx := context.Background()

	sent := Event{Kind: KindFollowerAdded, ActorID: uuid.New(), RecipientID: uuid.New()}
	if err := q.Enqueue(ctx, sent); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	got, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if got != sent {
		t.Fatalf("event mangled in transit: %+v != %+v", got, sent)
	}
}

func TestMemoryQueue_DequeueUnblocksOnCancel(t *testing.T) {
	q := NewMemoryQueue()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		_, err := q.Dequeue(ctx)
		done <- err
	}()

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, ErrQueueClosed) {
			t.Fatalf("expected ErrQueueClosed, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Dequeue did not unblock on context cancel")
	}
}

func TestMemoryQueue_FullQueueErrorsInsteadOfBlocking(t *testing.T) {
	q := NewMemoryQueue()
	ctx := context.Background()

	for i := 0; i < memoryCapacity; i++ {
		if err := q.Enqueue(ctx, Event{Kind: KindCommentCreated}); err != nil {
			t.Fatalf("enqueue %d within capacity: %v", i, err)
		}
	}

	// The producer is an API request; it must get an error, never a stall.
	start := time.Now()
	if err := q.Enqueue(ctx, Event{Kind: KindCommentCreated}); err == nil {
		t.Fatal("expected an error once the queue is full")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("full-queue enqueue took %v; it must not block", elapsed)
	}
}
