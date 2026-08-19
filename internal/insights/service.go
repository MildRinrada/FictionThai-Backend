package insights

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/internal/comments"
	"github.com/fictionthai/fictionthai/backend/internal/community"
	"github.com/fictionthai/fictionthai/backend/internal/novels"
)

// activityLimit bounds one feed. It is a glance at what is happening, not a
// log: a writer who wants everything opens the thread or the community.
const activityLimit = 8

// NovelAccess is the ownership gate. ForWriter is the novels service's own
// rule, so this package never re-implements "may this caller see this fiction"
// and cannot get it subtly different.
type NovelAccess interface {
	ForEditor(ctx context.Context, identity *auth.Identity, ref novels.Ref) (*novels.Novel, error)
}

// ViewSource is the daily view counters (13R's novel_view_days).
type ViewSource interface {
	ViewsSince(ctx context.Context, novelID uuid.UUID, days int) (int64, error)
	// ViewsByDay is one entry per day, oldest first, today last, zeros filled.
	ViewsByDay(ctx context.Context, novelID uuid.UUID, days int) ([]int64, error)
}

// CommentSource is the fiction's own thread.
type CommentSource interface {
	CountSince(ctx context.Context, novelID uuid.UUID, days int) (int64, error)
	CountsByDay(ctx context.Context, novelID uuid.UUID, days int) ([]int64, error)
	RecentForNovel(ctx context.Context, novelID uuid.UUID, limit int) ([]comments.RecentComment, error)
}

// PostSource is the community, read as the CALLER - never widened for the
// author of the fiction being discussed.
type PostSource interface {
	RecentPostsForNovel(
		ctx context.Context, viewerID, novelID uuid.UUID, limit int,
	) ([]community.RecentPost, error)
}

// Service composes the overview.
type Service struct {
	novels   NovelAccess
	views    ViewSource
	comments CommentSource
	posts    PostSource
	log      *slog.Logger
}

func NewService(
	novelAccess NovelAccess,
	views ViewSource,
	commentSource CommentSource,
	posts PostSource,
	log *slog.Logger,
) *Service {
	return &Service{
		novels:   novelAccess,
		views:    views,
		comments: commentSource,
		posts:    posts,
		log:      log,
	}
}

// For builds the overview for one fiction.
//
// Every source below the ownership check degrades to nothing rather than
// failing the request. The overview is a dashboard: a writer whose Redis is
// down or whose community query timed out should see the rest of their studio,
// not an error page in front of their own work.
func (s *Service) For(
	ctx context.Context, identity *auth.Identity, ref novels.Ref,
) (*View, error) {
	novel, err := s.novels.ForEditor(ctx, identity, ref)
	if err != nil {
		return nil, err
	}

	view := &View{
		WindowDays:    Window,
		Activity:      []Activity{},
		ViewsByDay:    make([]int64, Window),
		CommentsByDay: make([]int64, Window),
	}

	if s.views != nil {
		total, err := s.views.ViewsSince(ctx, novel.ID, Window)
		if err != nil {
			s.log.Warn("insights: weekly views failed", slog.Any("error", err))
		} else {
			view.WeeklyViews = total
		}

		// Last week = the fortnight minus this week. One extra sum instead of a
		// date-ranged query, so the two windows can never overlap or gap.
		fortnight, err := s.views.ViewsSince(ctx, novel.ID, 2*Window)
		if err != nil {
			s.log.Warn("insights: fortnight views failed", slog.Any("error", err))
		} else {
			view.PrevWeeklyViews = fortnight - view.WeeklyViews
		}

		daily, err := s.views.ViewsByDay(ctx, novel.ID, Window)
		if err != nil {
			s.log.Warn("insights: daily views failed", slog.Any("error", err))
		} else {
			view.ViewsByDay = daily
		}
	}

	if s.comments != nil {
		total, err := s.comments.CountSince(ctx, novel.ID, Window)
		if err != nil {
			s.log.Warn("insights: weekly comments failed", slog.Any("error", err))
		} else {
			view.WeeklyComments = total
		}

		fortnight, err := s.comments.CountSince(ctx, novel.ID, 2*Window)
		if err != nil {
			s.log.Warn("insights: fortnight comments failed", slog.Any("error", err))
		} else {
			view.PrevWeeklyComments = fortnight - view.WeeklyComments
		}

		daily, err := s.comments.CountsByDay(ctx, novel.ID, Window)
		if err != nil {
			s.log.Warn("insights: daily comments failed", slog.Any("error", err))
		} else {
			view.CommentsByDay = daily
		}

		recent, err := s.comments.RecentForNovel(ctx, novel.ID, activityLimit)
		if err != nil {
			s.log.Warn("insights: recent comments failed", slog.Any("error", err))
		} else {
			for _, item := range recent {
				view.Activity = append(view.Activity, commentActivity(item))
			}
		}
	}

	if s.posts != nil {
		recent, err := s.posts.RecentPostsForNovel(
			ctx, identity.UserID(), novel.ID, activityLimit)
		if err != nil {
			s.log.Warn("insights: recent posts failed", slog.Any("error", err))
		} else {
			for _, item := range recent {
				view.Activity = append(view.Activity, Activity{
					Kind:      KindPost,
					Actor:     item.AuthorName,
					Excerpt:   item.Excerpt,
					PostID:    item.ID.String(),
					CreatedAt: item.CreatedAt,
				})
			}
		}
	}

	// One feed, newest first. The two sources are each already ordered; merging
	// them is what makes the panel a timeline rather than two lists stacked.
	sort.SliceStable(view.Activity, func(i, j int) bool {
		return view.Activity[i].CreatedAt.After(view.Activity[j].CreatedAt)
	})
	if len(view.Activity) > activityLimit {
		view.Activity = view.Activity[:activityLimit]
	}

	return view, nil
}

// commentActivity names where a comment landed.
//
// A chapter's TITLE when it has one and its number when it does not, because
// that is what the writer calls it - and neither when the comment is on the
// fiction itself, which is a real place a comment can be.
func commentActivity(item comments.RecentComment) Activity {
	activity := Activity{
		Kind:      KindComment,
		Actor:     item.AuthorName,
		Excerpt:   item.Excerpt,
		CreatedAt: item.CreatedAt,
	}
	if item.ChapterSlug != nil {
		activity.ChapterSlug = *item.ChapterSlug
	}
	switch {
	case item.ChapterTitle != nil && *item.ChapterTitle != "":
		activity.ChapterLabel = *item.ChapterTitle
	case item.ChapterNumber != nil:
		activity.ChapterLabel = fmt.Sprintf("ตอนที่ %d", *item.ChapterNumber)
	}
	return activity
}
