// Package views counts how many people read a fiction (Phase 12C,
// docs/PHASE-12-STORY-DEPTH.md §12C).
//
// Two properties matter more than the number itself:
//
//   - It must not cost the reader anything. Opening a chapter is the hottest
//     path in the product (docs/07 §67), so a view never takes a database write
//     on the request. It takes one Redis round trip, and a background flusher
//     applies the accumulated totals in batches.
//
//   - It must not become a reading history. docs/11 §34 does not permit
//     building one, and the studio's own privacy note to writers
//     ("เราไม่เก็บสถิติการอ่านรายบุคคล") has to stay true. The de-duplication key
//     is therefore a one-way hash that expires within the day, is salted with a
//     value that is never stored, and is never written to PostgreSQL. It can
//     answer "has this viewer been counted today" and nothing else.
//
// With Redis unavailable the whole feature degrades to counting nothing, which
// is the correct failure: a wrong number is worse than no number, and reading
// must continue regardless.
package views

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const (
	// seenPrefix marks "this viewer has been counted for this fiction today".
	seenPrefix = "ft:view:seen:"
	// bufferKey accumulates pending increments as a Redis hash of novel id →
	// count, so a restart loses at most one flush interval rather than a day.
	bufferKey = "ft:view:buffer"

	// seenTTL is how long a viewer stays counted. A day, matching the
	// de-duplication window the counter documents.
	seenTTL = 24 * time.Hour

	// FlushInterval is how often buffered counts are written to PostgreSQL.
	FlushInterval = 30 * time.Second
)

// Store is the slice of persistence this package needs.
type Store interface {
	AddViews(ctx context.Context, counts map[uuid.UUID]int64) error
}

// Recorder counts reads.
type Recorder struct {
	rdb   *redis.Client
	store Store
	log   *slog.Logger

	// salt is generated at startup and never persisted or logged. It makes the
	// de-duplication keys unlinkable across restarts, so even someone holding a
	// Redis dump cannot turn them back into "who read what".
	salt []byte

	mu sync.Mutex
}

// NewRecorder wires a recorder. A nil rdb (Redis disabled) yields a recorder
// that records nothing rather than one that fails.
func NewRecorder(rdb *redis.Client, store Store, log *slog.Logger) *Recorder {
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		// Without a salt the keys would be a plain hash of the viewer id, which
		// is exactly the linkability this design avoids. Refuse to count rather
		// than count unsafely.
		log.Error("views: could not generate a de-duplication salt; view counting is disabled")
		return &Recorder{store: store, log: log}
	}
	return &Recorder{rdb: rdb, store: store, log: log, salt: salt}
}

// Enabled reports whether counting is actually possible.
func (r *Recorder) Enabled() bool {
	return r != nil && r.rdb != nil && r.store != nil && r.salt != nil
}

// viewerKey derives the de-duplication key.
//
// `viewer` is the session id for a member and the client address for a guest.
// Neither is stored: they go through a keyed hash whose salt exists only in this
// process's memory, and the result expires within the day.
func (r *Recorder) viewerKey(novelID uuid.UUID, viewer string, day string) string {
	sum := sha256.Sum256(append(append(r.salt, []byte(viewer)...), []byte(day)...))
	return seenPrefix + novelID.String() + ":" + hex.EncodeToString(sum[:12])
}

// Record counts one read, unless this viewer has already been counted today.
//
// Fire-and-forget by contract: it returns nothing, and every failure is
// swallowed. A view that goes uncounted is invisible; a reader who cannot read
// because a counter failed is not.
func (r *Recorder) Record(ctx context.Context, novelID uuid.UUID, viewer string) {
	if !r.Enabled() || viewer == "" {
		return
	}

	day := time.Now().UTC().Format("2006-01-02")
	key := r.viewerKey(novelID, viewer, day)

	// SetNX is the whole de-duplication: the first read of the day claims the
	// key, every later one finds it taken.
	first, err := r.rdb.SetNX(ctx, key, 1, seenTTL).Result()
	if err != nil || !first {
		return
	}

	if err := r.rdb.HIncrBy(ctx, bufferKey, novelID.String(), 1).Err(); err != nil {
		// The buffer increment failed after the key was claimed, so this view is
		// lost. That is acceptable for a display counter and better than
		// retrying on the reader's request.
		r.log.Debug("views: buffer increment failed", slog.Any("error", err))
	}
}

// Flush moves everything buffered in Redis into PostgreSQL.
//
// The buffer is read and deleted in one transaction so a concurrent flusher
// cannot double-apply the same counts; anything incremented between the read and
// the delete lands in the next flush.
func (r *Recorder) Flush(ctx context.Context) error {
	if !r.Enabled() {
		return nil
	}

	// Only one flush at a time within this process.
	r.mu.Lock()
	defer r.mu.Unlock()

	pipe := r.rdb.TxPipeline()
	read := pipe.HGetAll(ctx, bufferKey)
	pipe.Del(ctx, bufferKey)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("drain view buffer: %w", err)
	}

	raw := read.Val()
	if len(raw) == 0 {
		return nil
	}

	counts := make(map[uuid.UUID]int64, len(raw))
	for id, value := range raw {
		novelID, err := uuid.Parse(id)
		if err != nil {
			continue
		}
		var delta int64
		if _, err := fmt.Sscanf(value, "%d", &delta); err != nil || delta <= 0 {
			continue
		}
		counts[novelID] = delta
	}

	if err := r.store.AddViews(ctx, counts); err != nil {
		return fmt.Errorf("apply view counts: %w", err)
	}
	return nil
}

// Run flushes on an interval until the context is cancelled, then flushes once
// more so a graceful shutdown does not drop the last window's counts.
func (r *Recorder) Run(ctx context.Context) {
	if !r.Enabled() {
		return
	}

	ticker := time.NewTicker(FlushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// A fresh context: the one that just ended cannot be used to write.
			final, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			if err := r.Flush(final); err != nil {
				r.log.Warn("views: final flush failed", slog.Any("error", err))
			}
			cancel()
			return
		case <-ticker.C:
			if err := r.Flush(ctx); err != nil {
				r.log.Warn("views: flush failed", slog.Any("error", err))
			}
		}
	}
}
