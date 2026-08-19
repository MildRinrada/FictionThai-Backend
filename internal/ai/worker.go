package ai

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// Worker consumes the AI job queue and drives asynchronous requests to
// completion (docs/12 §27 async flow; docs/09 §46). Like the notifications
// worker it runs INSIDE the API process - one deployable, per the modular
// monolith (docs/07 §7) - and with the Redis queue it can later move to its own
// binary without touching this code.
//
// All the actual work (claim, authorize, provider call, output validation,
// persistence, notification) lives on the Service, so the provider and
// authorization boundaries are enforced in exactly one place whether a feature
// runs synchronously or here. The worker is only the loop.
type Worker struct {
	queue   Queue
	service *Service
	log     *slog.Logger

	done chan struct{}
	once sync.Once
}

func NewWorker(queue Queue, service *Service, log *slog.Logger) *Worker {
	return &Worker{queue: queue, service: service, log: log, done: make(chan struct{})}
}

// Recover re-drives durable work left behind by a previous run: it resets
// orphaned 'processing' rows and re-enqueues everything queued (docs/12 §28
// lifecycle). It is a STARTUP step, called once by the process owner (main)
// before Start - deliberately NOT inside Start, because its scan is GLOBAL
// across all requests, which is correct for the single running instance but
// would let concurrent workers over a shared database steal one another's work.
func (w *Worker) Recover(ctx context.Context) {
	w.service.RecoverPending(ctx)
}

// Start launches the processing loop and returns a wait function for graceful
// shutdown: cancel ctx, then call it to let an in-flight job finish.
func (w *Worker) Start(ctx context.Context) (wait func()) {
	w.once.Do(func() {
		go func() {
			defer close(w.done)
			w.run(ctx)
		}()
	})
	return func() { <-w.done }
}

func (w *Worker) run(ctx context.Context) {
	for {
		id, err := w.queue.Dequeue(ctx)
		if errors.Is(err, ErrQueueClosed) {
			return
		}
		if err != nil {
			w.log.Error("ai dequeue failed", slog.Any("error", err))
			continue
		}
		// Processing uses its own context: the loop context cancels to STOP THE
		// LOOP, not to abandon a job already claimed.
		w.service.Process(context.WithoutCancel(ctx), id)
	}
}

// ---------------------------------------------------------------------------
// Service-side async processing (the worker's entry points)
// ---------------------------------------------------------------------------

// RecoverPending resets orphaned 'processing' rows back to queued and
// re-enqueues every queued request, so a restart re-drives durable work that the
// in-memory queue would otherwise lose (docs/12 §28). Single-instance by design,
// matching the current deployment; multi-instance recovery would need leases.
func (s *Service) RecoverPending(ctx context.Context) {
	ids, err := s.repo.RecoverQueued(ctx)
	if err != nil {
		s.log.Error("ai: recover queued jobs failed", slog.Any("error", err))
		return
	}
	for _, id := range ids {
		if err := s.queue.Enqueue(ctx, id); err != nil {
			s.log.Error("ai: re-enqueue on recovery failed",
				slog.String("request_id", id.String()), slog.Any("error", err))
		}
	}
	if len(ids) > 0 {
		s.log.Info("ai worker recovered queued jobs", slog.Int("count", len(ids)))
	}
}

// Process runs one asynchronous request through its lifecycle. It is safe to
// call for an id that was cancelled or already handled - the claim simply finds
// no queued row and returns.
func (s *Service) Process(ctx context.Context, requestID uuid.UUID) {
	req, claimed, err := s.repo.Claim(ctx, requestID)
	if err != nil {
		s.log.Error("ai: claim job failed",
			slog.String("request_id", requestID.String()), slog.Any("error", err))
		return
	}
	if !claimed {
		// Cancelled between enqueue and claim, or already processed - nothing to
		// do. This is the concurrency guarantee, not an error (docs/12 §28).
		return
	}
	if req.ChapterID == nil {
		s.failJob(ctx, req, string(FailInternal))
		return
	}

	// Re-authorize at PROCESSING time by owner id: a chapter deleted or a
	// fiction transferred since the request was queued fails the job cleanly
	// rather than leaking (docs/12 §33). content_unavailable is non-retryable.
	content, err := s.chapters.ContentForOwnerID(ctx, req.UserID, *req.ChapterID)
	if err != nil {
		s.log.Warn("ai: job content unavailable",
			slog.String("request_id", req.ID.String()))
		s.failJob(ctx, req, "content_unavailable")
		return
	}

	text := content.Content
	if len([]rune(text)) > s.cfg.MaxInputRunes {
		text = string([]rune(text)[:s.cfg.MaxInputRunes])
	}

	switch req.Feature {
	case FeatureSummary:
		s.processSummary(ctx, req, text)
	default:
		s.processAnalyze(ctx, req, text)
	}
}

func (s *Service) processSummary(ctx context.Context, req *Request, text string) {
	result, err := s.provider.Summarize(ctx, SummarizeInput{Text: text})
	if err != nil {
		s.failJob(ctx, req, string(failureKindOf(err)))
		return
	}
	summary := strings.TrimSpace(result.Summary)
	if summary == "" || len([]rune(summary)) > maxSummaryRunes {
		// Empty or oversize output is invalid and never persisted (docs/12 §35).
		s.failJob(ctx, req, string(FailMalformed))
		return
	}
	sug := NewSuggestion{
		ChapterID:     *req.ChapterID,
		Type:          SuggestionSummary,
		OriginalText:  "",
		SuggestedText: &summary,
	}
	if err := s.repo.Complete(ctx, req.ID, result.Model, []NewSuggestion{sug}); err != nil {
		// The row is left processing; RecoverQueued re-drives it on restart.
		s.log.Error("ai: complete summary failed",
			slog.String("request_id", req.ID.String()), slog.Any("error", err))
		return
	}
	s.log.Info("ai summary completed",
		slog.String("request_id", req.ID.String()), slog.String("model", result.Model))
	s.notifyDone(ctx, req)
}

func (s *Service) processAnalyze(ctx context.Context, req *Request, text string) {
	result, err := s.provider.Analyze(ctx, AnalyzeInput{Text: text, Kinds: req.Feature.analyzeKinds()})
	if err != nil {
		s.failJob(ctx, req, string(failureKindOf(err)))
		return
	}
	inline := s.validateInline(result.Suggestions, len([]rune(text)))
	news := toNewSuggestions(*req.ChapterID, inline)
	if err := s.repo.Complete(ctx, req.ID, result.Model, news); err != nil {
		s.log.Error("ai: complete analyze failed",
			slog.String("request_id", req.ID.String()), slog.Any("error", err))
		return
	}
	s.log.Info("ai async analyze completed",
		slog.String("request_id", req.ID.String()),
		slog.String("feature", string(req.Feature)),
		slog.Int("suggestions", len(news)))
	s.notifyDone(ctx, req)
}

// failJob marks a job failed with a safe classification and notifies its owner.
func (s *Service) failJob(ctx context.Context, req *Request, code string) {
	if err := s.repo.Fail(ctx, req.ID, code); err != nil {
		s.log.Error("ai: mark job failed",
			slog.String("request_id", req.ID.String()), slog.Any("error", err))
		return
	}
	s.log.Warn("ai job failed",
		slog.String("request_id", req.ID.String()),
		slog.String("feature", string(req.Feature)),
		slog.String("error_code", code))
	s.notifyDone(ctx, req)
}

// notifyDone tells the request's owner their async job reached a terminal state
// (docs/12 §27 "Notify frontend"). Fire-and-forget: a notification failure never
// changes the job outcome.
func (s *Service) notifyDone(ctx context.Context, req *Request) {
	if s.notifier == nil {
		return
	}
	s.notifier.AIRequestCompleted(context.WithoutCancel(ctx), req.UserID, req.ID)
}
