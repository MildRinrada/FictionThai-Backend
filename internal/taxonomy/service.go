package taxonomy

import (
	"context"
	"log/slog"
	"strings"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/pagination"
)

// maxTagSearchLength bounds the browse query (docs/09 §36).
const maxTagSearchLength = 100

// Service owns the vocabulary business rules.
type Service struct {
	repo *Repository
	log  *slog.Logger
}

func NewService(repo *Repository, log *slog.Logger) *Service {
	return &Service{repo: repo, log: log}
}

// Genres returns the controlled vocabulary. Public - a guest browsing by
// genre needs it (docs/03 §9).
func (s *Service) Genres(ctx context.Context) ([]Genre, error) {
	genres, err := s.repo.ListGenres(ctx)
	if err != nil {
		return nil, s.internal("list genres", err)
	}
	return genres, nil
}

// Tags returns a page of tags for browsing or typeahead, most-used first.
// Public (docs/01 §6 "Browse tags").
func (s *Service) Tags(
	ctx context.Context, query string, page pagination.Params,
) ([]Tag, pagination.Meta, error) {
	query = strings.TrimSpace(query)
	if len([]rune(query)) > maxTagSearchLength {
		return nil, pagination.Meta{}, apierror.Validation(map[string][]string{
			"q": {"Search must be at most 100 characters."},
		})
	}

	tags, total, err := s.repo.ListTags(ctx, query, page.Limit(), page.Offset())
	if err != nil {
		return nil, pagination.Meta{}, s.internal("list tags", err)
	}
	return tags, page.MetaFor(total), nil
}

// CreateTag resolves a writer-supplied name to a tag, creating it when new.
//
// This is the ONE path a tag enters the vocabulary through (docs/01 §13 "Add
// tags"), so the format-metadata ban of docs/08 §15.2 and the name rules are
// enforced in exactly one place. Idempotent: an existing name returns its row.
func (s *Service) CreateTag(
	ctx context.Context, identity *auth.Identity, rawName string,
) (*Tag, error) {
	if !identity.Authenticated() {
		return nil, apierror.Unauthorized("Authentication required.")
	}

	name := NormalizeTagName(rawName)
	if ok, reason := ValidTagName(name); !ok {
		return nil, apierror.Validation(map[string][]string{"name": {reason}})
	}

	tag, err := s.repo.FindOrCreateTag(ctx, name, TagSlug(name))
	if err != nil {
		return nil, s.internal("create tag", err)
	}
	return tag, nil
}

func (s *Service) internal(op string, err error) error {
	s.log.Error("taxonomy: "+op+" failed", slog.Any("error", err))
	return apierror.Internal()
}
