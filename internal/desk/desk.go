// Package desk answers the writer's shell.
//
// Every page a signed-in writer opens draws the same header, and that header
// wants four small facts: how much unfinished work is waiting, what they wrote
// today, which fictions they touched last, and where they stopped typing. Each
// fact already exists in a domain that owns it; what this package adds is
// asking for all of them under one session check, in one request, so the shell
// does not open four.
//
// It is the account-level twin of package insights, and follows the same two
// rules:
//
//   - It owns no table. Each source is a consumer-defined interface satisfied
//     by the owning domain's own repository, so nothing here reaches across a
//     domain boundary into another package's SQL.
//
//   - It is about the CALLER and nobody else. There is no id parameter and no
//     way to ask it about another writer - the identity comes from the session
//     and the queries are keyed by it. A writer's unfinished drafts are the
//     most private thing on the platform, and this endpoint cannot be pointed
//     at anyone.
package desk

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/internal/chapters"
	"github.com/fictionthai/fictionthai/backend/internal/novels"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
)

// RecentLimit is how many fictions the create menu offers.
//
// Three, because the menu exists to save a writer the trip through the studio
// for the fiction they are obviously working on - and a list long enough to
// need reading is a list that has stopped being a shortcut.
const RecentLimit = 3

// SearchLimit is how many of the writer's own pieces a suggestion box shows.
const SearchLimit = 5

// ChapterSource is the chapters domain, as this package needs it.
type ChapterSource interface {
	UnfinishedForAuthor(ctx context.Context, userID uuid.UUID) (int, error)
	UnfinishedByNovel(ctx context.Context, novelIDs []uuid.UUID) (map[uuid.UUID]int, error)
	WordsWrittenOn(ctx context.Context, userID uuid.UUID, day time.Time) (int, error)
	LastEditedForAuthor(ctx context.Context, userID uuid.UUID) (*chapters.Resume, error)
	SearchOwn(ctx context.Context, userID uuid.UUID, query string, limit int) ([]chapters.DeskHit, error)
}

// NovelSource is the novels domain, as this package needs it.
type NovelSource interface {
	RecentForAuthor(ctx context.Context, authorID uuid.UUID, limit int) ([]novels.Desk, error)
}

// Work is one of the writer's fictions in the create menu.
type Work struct {
	Slug  string `json:"slug"`
	Title string `json:"title"`
	// Unfinished is that fiction's share of the badge, so the menu can say
	// which of the three is the one with work waiting in it.
	Unfinished int       `json:"unfinished"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Resume is "เขียนต่อจากที่ค้าง" - one link, straight back into the editor.
type Resume struct {
	NovelSlug    string    `json:"novel_slug"`
	NovelTitle   string    `json:"novel_title"`
	ChapterSlug  string    `json:"chapter_slug"`
	ChapterLabel string    `json:"chapter_label"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// View is the whole answer.
type View struct {
	// Unfinished is the studio badge: drafts WITH WORDS IN THEM that nobody
	// can read yet. Never a count of empty chapters - a number a writer cannot
	// clear is a number they learn to ignore.
	Unfinished int `json:"unfinished"`
	// WordsToday is the quiet counter. Zero is a real answer and is sent as
	// one; the client decides whether a zero is worth drawing.
	WordsToday int     `json:"words_today"`
	Recent     []Work  `json:"recent"`
	Resume     *Resume `json:"resume,omitempty"`
}

// Service composes the answer.
type Service struct {
	chapters ChapterSource
	novels   NovelSource
	// now is injectable so a test can decide what "today" means without
	// waiting for midnight.
	now func() time.Time
}

// NewService wires the sources.
func NewService(chapterSource ChapterSource, novelSource NovelSource) *Service {
	return &Service{chapters: chapterSource, novels: novelSource, now: time.Now}
}

// ErrUnauthenticated is returned when there is no session to answer about.
var ErrUnauthenticated = errors.New("desk requires a session")

// Mine answers for the caller.
//
// Failures of the individual reads are NOT fatal to the whole response. The
// desk decorates a header that every page draws; a slow or broken counter must
// not take the navigation down with it, so a source that errors contributes its
// zero value and the rest of the shell still renders.
func (s *Service) Mine(ctx context.Context, identity *auth.Identity) (*View, error) {
	if identity == nil {
		return nil, apierror.New(
			http.StatusUnauthorized, "UNAUTHENTICATED", "Sign in to continue.")
	}
	userID := identity.UserID()

	view := &View{Recent: []Work{}}

	if count, err := s.chapters.UnfinishedForAuthor(ctx, userID); err == nil {
		view.Unfinished = count
	}
	if words, err := s.chapters.WordsWrittenOn(ctx, userID, s.now()); err == nil {
		view.WordsToday = words
	}
	if resume, err := s.chapters.LastEditedForAuthor(ctx, userID); err == nil && resume != nil {
		view.Resume = &Resume{
			NovelSlug:    resume.NovelSlug,
			NovelTitle:   resume.NovelTitle,
			ChapterSlug:  resume.ChapterSlug,
			ChapterLabel: resume.ChapterLabel,
			UpdatedAt:    resume.UpdatedAt,
		}
	}

	recent, err := s.novels.RecentForAuthor(ctx, userID, RecentLimit)
	if err != nil || len(recent) == 0 {
		return view, nil
	}

	ids := make([]uuid.UUID, 0, len(recent))
	for _, item := range recent {
		ids = append(ids, item.ID)
	}
	unfinished, err := s.chapters.UnfinishedByNovel(ctx, ids)
	if err != nil {
		unfinished = map[uuid.UUID]int{}
	}
	for _, item := range recent {
		view.Recent = append(view.Recent, Work{
			Slug:       item.Slug,
			Title:      item.Title,
			Unfinished: unfinished[item.ID],
			UpdatedAt:  item.UpdatedAt,
		})
	}
	return view, nil
}

// Hit is one of the writer's own pieces of work, found by name.
type Hit struct {
	NovelSlug    string `json:"novel_slug"`
	NovelTitle   string `json:"novel_title"`
	ChapterSlug  string `json:"chapter_slug"`
	ChapterLabel string `json:"chapter_label"`
	Draft        bool   `json:"draft"`
}

// Search finds the CALLER's own work by title, drafts included.
//
// This is the half of search that public search cannot do by definition: a
// writer's unpublished chapters are invisible to every search index on the
// platform, so without this the thing a writer looks for most often is the one
// thing the search box could never find.
//
// Scoped to the session, like the rest of this package. There is no id to pass
// and no way to search another writer's drafts.
func (s *Service) Search(
	ctx context.Context, identity *auth.Identity, query string,
) ([]Hit, error) {
	if identity == nil {
		return nil, apierror.New(
			http.StatusUnauthorized, "UNAUTHENTICATED", "Sign in to continue.")
	}
	found, err := s.chapters.SearchOwn(ctx, identity.UserID(), query, SearchLimit)
	if err != nil {
		// A failing suggestion list is an empty suggestion list. The search box
		// must keep working for everything else it can find.
		return []Hit{}, nil
	}
	hits := make([]Hit, 0, len(found))
	for _, item := range found {
		hits = append(hits, Hit{
			NovelSlug:    item.NovelSlug,
			NovelTitle:   item.NovelTitle,
			ChapterSlug:  item.ChapterSlug,
			ChapterLabel: item.ChapterLabel,
			Draft:        item.Draft,
		})
	}
	return hits, nil
}
