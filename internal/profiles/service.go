package profiles

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
)

// Service is the authorization boundary for the public profile read
// (docs/10 §27). There is exactly one rule and the repository predicate
// carries it: an account that is deleted or banned has no public profile, and
// says so with the same 404 an account that never existed gets.
type Service struct {
	repo *Repository
	log  *slog.Logger
}

func NewService(repo *Repository, log *slog.Logger) *Service {
	return &Service{repo: repo, log: log}
}

func notFound() *apierror.Error {
	return apierror.New(http.StatusNotFound, "USER_NOT_FOUND", "User not found.")
}

// Get returns one public profile.
//
// It takes no identity at all. That is the design, not an omission: the
// response is identical for a guest, a stranger, and the person themselves, so
// it can be cached once and served to everyone (docs/14 §7). Anything personal
// - whether the caller follows this person, their email, their drafts - is
// reached through an endpoint that requires a session.
func (s *Service) Get(ctx context.Context, raw string) (*Profile, error) {
	ref, err := ParseRef(raw)
	if err != nil {
		return nil, notFound()
	}

	profile, err := s.repo.ByRef(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		return nil, notFound()
	}
	if err != nil {
		s.log.Error("profile service failure",
			slog.String("op", "load profile"), slog.Any("error", err))
		return nil, apierror.Internal()
	}
	return profile, nil
}

// Update edits the CALLER's own profile and returns the public view of the
// result - what the next visitor will see, not a private echo of the request.
//
// Self-scoped by construction: the row is chosen by identity.UserID(), never
// by anything in the request body, so there is no cross-user edit path to
// authorize or to get wrong (the users.SetAvatarURL pattern).
func (s *Service) Update(
	ctx context.Context, identity *auth.Identity, edit *Edit,
) (*Profile, error) {
	if !identity.Authenticated() {
		return nil, apierror.Unauthorized("Authentication required.")
	}
	userID := identity.UserID()

	clean, err := edit.clean()
	if err != nil {
		return nil, err
	}
	if err := s.repo.Update(ctx, userID, clean); err != nil {
		s.log.Error("profile service failure",
			slog.String("op", "update profile"), slog.Any("error", err))
		return nil, apierror.Internal()
	}

	profile, err := s.repo.ByRef(ctx, Ref{ID: userID})
	if err != nil {
		s.log.Error("profile service failure",
			slog.String("op", "reload profile"), slog.Any("error", err))
		return nil, apierror.Internal()
	}
	return profile, nil
}

// SearchAuthors answers the "นักเขียน" group of the site search.
//
// Public, like every other kind of discovery on this platform (docs/03 §27):
// finding a writer must not require an account, or a reader cannot follow the
// person whose story they just finished without signing up first.
//
// An empty query returns an empty list rather than everyone. A suggestion box
// that answers "" with the first page of the user table is a directory of the
// platform's members, which is not a thing this endpoint is for.
func (s *Service) SearchAuthors(ctx context.Context, query string) ([]Author, error) {
	authors, err := s.repo.SearchAuthors(ctx, query, SearchLimit)
	if err != nil {
		s.log.Error("profile service failure",
			slog.String("op", "search authors"), slog.Any("error", err))
		return nil, apierror.Internal()
	}
	return authors, nil
}
