package notifications

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/pagination"
)

// Service owns the recipient-facing API and the event EMIT side. It satisfies
// chapters.Notifier, comments.Notifier, and library.Notifier - the consumer
// packages define those interfaces; this is the one implementation.
type Service struct {
	repo  *Repository
	queue Queue
	log   *slog.Logger
}

func NewService(repo *Repository, queue Queue, log *slog.Logger) *Service {
	return &Service{repo: repo, queue: queue, log: log}
}

func notFound() *apierror.Error {
	return apierror.New(http.StatusNotFound, "NOTIFICATION_NOT_FOUND", "Notification not found.")
}

func requireUser(identity *auth.Identity) (uuid.UUID, error) {
	if !identity.Authenticated() {
		return uuid.Nil, apierror.Unauthorized("Authentication required.")
	}
	return identity.UserID(), nil
}

// ---------------------------------------------------------------------------
// Recipient-facing API (docs/09 §23)
// ---------------------------------------------------------------------------

// List returns one page of the caller's notifications, newest first.
func (s *Service) List(
	ctx context.Context, identity *auth.Identity, page pagination.Params,
) ([]View, pagination.Meta, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return nil, pagination.Meta{}, err
	}

	items, total, err := s.repo.List(ctx, userID, page)
	if err != nil {
		return nil, pagination.Meta{}, s.internal("list notifications", err)
	}

	views := make([]View, 0, len(items))
	for i := range items {
		views = append(views, items[i].Render())
	}
	return views, page.MetaFor(total), nil
}

// UnreadCount answers the badge.
func (s *Service) UnreadCount(ctx context.Context, identity *auth.Identity) (int64, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return 0, err
	}
	count, err := s.repo.UnreadCount(ctx, userID)
	if err != nil {
		return 0, s.internal("count unread", err)
	}
	return count, nil
}

// MarkRead stamps one of the CALLER'S notifications read. Someone else's
// notification id gets the same 404 as a missing one (docs/11 §31).
func (s *Service) MarkRead(ctx context.Context, identity *auth.Identity, id uuid.UUID) error {
	userID, err := requireUser(identity)
	if err != nil {
		return err
	}
	err = s.repo.MarkRead(ctx, userID, id)
	if errors.Is(err, ErrNotFound) {
		return notFound()
	}
	if err != nil {
		return s.internal("mark read", err)
	}
	return nil
}

// MarkAllRead stamps everything unread.
func (s *Service) MarkAllRead(ctx context.Context, identity *auth.Identity) error {
	userID, err := requireUser(identity)
	if err != nil {
		return err
	}
	if _, err := s.repo.MarkAllRead(ctx, userID); err != nil {
		return s.internal("mark all read", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Emit side - called by other domains through their Notifier interfaces.
//
// All of these are fire-and-forget BY CONTRACT: the domain action has already
// committed, so a queue failure is logged and swallowed - it must never turn
// a successful publish, comment, or follow into a user-facing error
// (docs/07 §27 "The API should remain responsive").
// ---------------------------------------------------------------------------

// ChapterPublished satisfies chapters.Notifier.
func (s *Service) ChapterPublished(ctx context.Context, actorID, novelID, chapterID uuid.UUID) {
	s.emit(ctx, Event{
		Kind: KindChapterPublished, ActorID: actorID,
		NovelID: novelID, ChapterID: chapterID,
	})
}

// CommentCreated satisfies comments.Notifier.
func (s *Service) CommentCreated(ctx context.Context, actorID, commentID uuid.UUID) {
	s.emit(ctx, Event{Kind: KindCommentCreated, ActorID: actorID, CommentID: commentID})
}

// FollowerAdded satisfies library.Notifier.
func (s *Service) FollowerAdded(ctx context.Context, followerID, followingID uuid.UUID) {
	s.emit(ctx, Event{Kind: KindFollowerAdded, ActorID: followerID, RecipientID: followingID})
}

// CommunityCommentCreated satisfies community.Notifier.
func (s *Service) CommunityCommentCreated(ctx context.Context, actorID, commentID uuid.UUID) {
	s.emit(ctx, Event{
		Kind: KindCommunityCommentCreated, ActorID: actorID, CommunityCommentID: commentID,
	})
}

// CommunityReactionAdded satisfies community.Notifier (docs/08 §23.1's
// reserved community_reaction type).
func (s *Service) CommunityReactionAdded(ctx context.Context, actorID, postID uuid.UUID) {
	s.emit(ctx, Event{Kind: KindCommunityReactionAdded, ActorID: actorID, PostID: postID})
}

// ModerationActionTaken satisfies moderation.Notifier (docs/01 §26
// "Moderation notification"). actorID travels in the event ONLY for the
// never-notify-yourself rule; the stored notification carries no actor
// (docs/11 §39 - individual moderators are not exposed).
func (s *Service) ModerationActionTaken(
	ctx context.Context, actorID, recipientID uuid.UUID, targetType string, targetID uuid.UUID,
) {
	s.emit(ctx, Event{
		Kind: KindModerationAction, ActorID: actorID, RecipientID: recipientID,
		EntityType: targetType, EntityID: targetID,
	})
}

// AIRequestCompleted satisfies ai.Notifier: a writer's asynchronous AI job
// reached a terminal state (docs/12 §27 "Notify frontend"). Unlike moderation
// this is a SELF notification - the requester is told about their OWN job - so
// there is deliberately no never-notify-yourself guard. Like moderation it
// carries no actor: it is a system notification (docs/11 §39 pattern).
func (s *Service) AIRequestCompleted(ctx context.Context, recipientID, requestID uuid.UUID) {
	if recipientID == uuid.Nil {
		return
	}
	s.emit(ctx, Event{
		Kind: KindAICompleted, RecipientID: recipientID,
		EntityType: EntityAIRequest, EntityID: requestID,
	})
}

// SubscriptionActivated satisfies subscriptions.Notifier: a reader's Premium
// payment was verified and their subscription is active (Phase 11). A SELF
// notification with no actor - the recipient is told about their own
// subscription - so, like AI, there is no never-notify-yourself guard.
func (s *Service) SubscriptionActivated(ctx context.Context, recipientID, subscriptionID uuid.UUID) {
	if recipientID == uuid.Nil {
		return
	}
	s.emit(ctx, Event{
		Kind: KindSubscriptionActivated, RecipientID: recipientID,
		EntityType: EntitySubscription, EntityID: subscriptionID,
	})
}

// SubscriptionPaymentRejected satisfies subscriptions.Notifier: a reader's
// Premium payment was rejected and they may try again. Also a self, actor-less
// system notification.
func (s *Service) SubscriptionPaymentRejected(ctx context.Context, recipientID, subscriptionID uuid.UUID) {
	if recipientID == uuid.Nil {
		return
	}
	s.emit(ctx, Event{
		Kind: KindSubscriptionPaymentRejected, RecipientID: recipientID,
		EntityType: EntitySubscription, EntityID: subscriptionID,
	})
}

func (s *Service) emit(ctx context.Context, event Event) {
	// The request context may be cancelled the moment the response is written;
	// the enqueue must not be lost with it.
	if err := s.queue.Enqueue(context.WithoutCancel(ctx), event); err != nil {
		s.log.Error("could not enqueue notification event",
			slog.String("kind", event.Kind),
			slog.Any("error", err),
		)
	}
}

// internal logs the real failure and returns the opaque error (docs/11 §67).
func (s *Service) internal(op string, err error) error {
	s.log.Error("notifications service failure", slog.String("op", op), slog.Any("error", err))
	return apierror.Internal()
}
