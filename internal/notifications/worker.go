package notifications

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/google/uuid"
)

// Worker consumes the event queue and writes notification rows - the
// "Notification Worker" of docs/07 §37 and docs/09 §23.
//
// It runs INSIDE the API process: the system is a modular monolith
// (docs/07 §7), and docs/07 §27 asks for background processing, not a second
// deployable. With the Redis queue the loop can later move to its own binary
// without changing anything here.
type Worker struct {
	queue Queue
	repo  *Repository
	log   *slog.Logger

	done chan struct{}
	once sync.Once
}

func NewWorker(queue Queue, repo *Repository, log *slog.Logger) *Worker {
	return &Worker{queue: queue, repo: repo, log: log, done: make(chan struct{})}
}

// Start launches the delivery loop and returns a wait function for graceful
// shutdown: cancel ctx, then call it to let an in-flight delivery finish.
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
		event, err := w.queue.Dequeue(ctx)
		if errors.Is(err, ErrQueueClosed) {
			return
		}
		if err != nil {
			w.log.Error("notification dequeue failed", slog.Any("error", err))
			continue
		}

		// Delivery uses its own context: the loop context cancels to STOP THE
		// LOOP, not to abandon a delivery already popped from the queue.
		if err := w.deliver(context.WithoutCancel(ctx), event); err != nil {
			// Best-effort by design: the event is logged and dropped, never
			// retried into an infinite loop. A notification is a courtesy,
			// not a ledger entry.
			w.log.Error("notification delivery failed",
				slog.String("kind", event.Kind),
				slog.String("actor_id", event.ActorID.String()),
				slog.Any("error", err),
			)
		}
	}
}

// deliver routes one event to its recipients.
func (w *Worker) deliver(ctx context.Context, event Event) error {
	switch event.Kind {
	case KindChapterPublished:
		delivered, err := w.repo.DeliverChapterPublished(
			ctx, event.ActorID, event.NovelID, event.ChapterID)
		if err != nil {
			return err
		}
		w.log.Info("chapter publish notifications delivered",
			slog.String("chapter_id", event.ChapterID.String()),
			slog.Int64("recipients", delivered),
		)
		return nil

	case KindCommentCreated:
		return w.deliverComment(ctx, event)

	case KindCommunityCommentCreated:
		return w.deliverCommunityComment(ctx, event)

	case KindCommunityReactionAdded:
		return w.deliverCommunityReaction(ctx, event)

	case KindFollowerAdded:
		if event.RecipientID == event.ActorID || event.RecipientID == uuid.Nil {
			return nil
		}
		return w.repo.Insert(ctx,
			event.RecipientID, event.ActorID, TypeNewFollower, EntityUser, event.ActorID)

	case KindModerationAction:
		// docs/01 §26 "Moderation notification". No re-check against current
		// state: the action itself is the fact being reported, and it has
		// already been committed and audited. Moderators are never notified of
		// their own actions, and the stored row carries NO actor (docs/11 §39).
		if event.RecipientID == event.ActorID || event.RecipientID == uuid.Nil {
			return nil
		}
		return w.repo.InsertSystem(ctx,
			event.RecipientID, TypeModeration, event.EntityType, event.EntityID)

	case KindAICompleted:
		// A writer's own async AI job finished (docs/12 §27). A self
		// notification with no actor - the recipient IS the requester, so the
		// never-notify-yourself guard the other kinds use is deliberately absent.
		if event.RecipientID == uuid.Nil {
			return nil
		}
		return w.repo.InsertSystem(ctx,
			event.RecipientID, TypeAI, event.EntityType, event.EntityID)

	case KindSubscriptionActivated:
		// The reader's own Premium payment was verified (Phase 11). Self, no actor.
		if event.RecipientID == uuid.Nil {
			return nil
		}
		return w.repo.InsertSystem(ctx,
			event.RecipientID, TypeSubscriptionActive, event.EntityType, event.EntityID)

	case KindSubscriptionPaymentRejected:
		// The reader's own Premium payment was rejected (Phase 11). Self, no actor.
		if event.RecipientID == uuid.Nil {
			return nil
		}
		return w.repo.InsertSystem(ctx,
			event.RecipientID, TypeSubscriptionPaymentFailed, event.EntityType, event.EntityID)

	default:
		return fmt.Errorf("unknown event kind %q", event.Kind)
	}
}

// deliverComment notifies up to two people about one comment (docs/08 §23.1
// new_comment / comment_reply):
//
//	the parent comment's author  - comment_reply, for a reply to them
//	the fiction's author         - new_comment, for any discussion on their work
//
// Nobody is ever notified about their own comment, and when the fiction's
// author IS the parent author, comment_reply wins - one event, one
// notification.
func (w *Worker) deliverComment(ctx context.Context, event Event) error {
	info, err := w.repo.CommentInfo(ctx, event.CommentID)
	if errors.Is(err, ErrNotFound) {
		return nil // deleted before delivery; notify nobody
	}
	if err != nil {
		return err
	}
	// Re-check CURRENT state: a comment taken back, moderated, still waiting in
	// the author's review queue, or on a fiction that has since gone private
	// notifies nobody.
	//
	// A guest comment notifies nobody either: there is no account to name as
	// the actor, and a notification whose subject is a display string anyone
	// could type is not a notification worth sending (§13D).
	if !info.Live || !info.Readable || info.Anonymous {
		return nil
	}

	notifiedParent := uuid.Nil
	if info.ParentAuthor.Valid && info.ParentAuthor.UUID != info.AuthorID {
		if err := w.repo.Insert(ctx,
			info.ParentAuthor.UUID, info.AuthorID,
			TypeCommentReply, EntityComment, event.CommentID); err != nil {
			return err
		}
		notifiedParent = info.ParentAuthor.UUID
	}

	if info.NovelAuthorID != info.AuthorID && info.NovelAuthorID != notifiedParent {
		if err := w.repo.Insert(ctx,
			info.NovelAuthorID, info.AuthorID,
			TypeNewComment, EntityComment, event.CommentID); err != nil {
			return err
		}
	}
	return nil
}

// deliverCommunityComment mirrors deliverComment for the community domain
// (docs/01 §20.1): the post's author gets new_comment, a reply's parent
// author gets comment_reply, reply wins on collision, and nobody is notified
// about their own comment. Entity type community_comment tells clients which
// surface the id opens.
func (w *Worker) deliverCommunityComment(ctx context.Context, event Event) error {
	info, err := w.repo.CommunityCommentInfo(ctx, event.CommunityCommentID)
	if errors.Is(err, ErrNotFound) {
		return nil // deleted before delivery; notify nobody
	}
	if err != nil {
		return err
	}
	if !info.Live || !info.Readable {
		return nil
	}

	notifiedParent := uuid.Nil
	if info.ParentAuthor.Valid && info.ParentAuthor.UUID != info.AuthorID {
		if err := w.repo.Insert(ctx,
			info.ParentAuthor.UUID, info.AuthorID,
			TypeCommentReply, EntityCommunityComment, event.CommunityCommentID); err != nil {
			return err
		}
		notifiedParent = info.ParentAuthor.UUID
	}

	if info.NovelAuthorID != info.AuthorID && info.NovelAuthorID != notifiedParent {
		if err := w.repo.Insert(ctx,
			info.NovelAuthorID, info.AuthorID,
			TypeNewComment, EntityCommunityComment, event.CommunityCommentID); err != nil {
			return err
		}
	}
	return nil
}

// deliverCommunityReaction notifies a post's author of a reaction -
// docs/08 §23.1's reserved community_reaction type.
//
// Two spam guards compose (docs/01 §20.2 "should not encourage spam"): the
// service only emits when a reaction row was newly INSERTED, and this side
// skips delivery when the same (recipient, actor, post) notification already
// exists - so react → unreact → react cycles deliver at most one
// notification, ever.
func (w *Worker) deliverCommunityReaction(ctx context.Context, event Event) error {
	info, err := w.repo.ReactionInfo(ctx, event.PostID, event.ActorID)
	if errors.Is(err, ErrNotFound) {
		return nil // post hard-deleted before delivery
	}
	if err != nil {
		return err
	}
	// Re-check CURRENT state: a post taken back or moderated, or a reaction
	// already withdrawn, notifies nobody.
	if !info.Live || !info.Reacted || info.PostAuthorID == event.ActorID {
		return nil
	}

	already, err := w.repo.HasNotification(ctx,
		info.PostAuthorID, event.ActorID,
		TypeCommunityReaction, EntityCommunityPost, event.PostID)
	if err != nil {
		return err
	}
	if already {
		return nil
	}

	return w.repo.Insert(ctx,
		info.PostAuthorID, event.ActorID,
		TypeCommunityReaction, EntityCommunityPost, event.PostID)
}
