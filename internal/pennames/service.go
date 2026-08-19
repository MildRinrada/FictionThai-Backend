package pennames

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
)

// Service owns pen-name business rules and is the authorization boundary for
// everything under /me/pen-names.
//
// There is exactly one authorization rule, and it is structural rather than
// checked: every repository call takes identity.UserID() as part of its
// predicate, so a request naming someone else's pen name matches no row. That
// is why there is no "is this mine" branch anywhere below - the alternative,
// loading a row and then comparing owners, is the shape that eventually forgets
// to compare (docs/10 §27).
type Service struct {
	repo *Repository
	log  *slog.Logger
}

func NewService(repo *Repository, log *slog.Logger) *Service {
	return &Service{repo: repo, log: log}
}

// notFound is the response for a pen name the caller does not own.
//
// Identical whether the id is absent or belongs to another account, so the
// endpoint cannot be used to probe for the existence of someone else's
// identities (docs/11 §3.4).
func notFound() *apierror.Error {
	return apierror.New(http.StatusNotFound, "PEN_NAME_NOT_FOUND", "Pen name not found.")
}

func requireUser(identity *auth.Identity) (uuid.UUID, error) {
	if !identity.Authenticated() {
		return uuid.Nil, apierror.Unauthorized("Authentication required.")
	}
	return identity.UserID(), nil
}

func (s *Service) internal(op string, err error) error {
	s.log.Error("pennames: "+op, slog.Any("error", err))
	return apierror.Internal()
}

func nameTaken() error {
	return apierror.Validation(map[string][]string{
		"name": {"You already use this pen name."},
	})
}

// List returns the caller's own identities. Not a paginated collection: this is
// a small, complete set the writer built by hand, and a page boundary that hid
// one of a writer's own names would be absurd.
func (s *Service) List(ctx context.Context, identity *auth.Identity) ([]View, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return nil, err
	}

	list, err := s.repo.ListForUser(ctx, userID)
	if err != nil {
		return nil, s.internal("list", err)
	}

	views := make([]View, 0, len(list))
	for i := range list {
		views = append(views, list[i].View())
	}
	return views, nil
}

// Create adds one identity to the caller's own account.
func (s *Service) Create(
	ctx context.Context, identity *auth.Identity, input CreateInput,
) (*View, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return nil, err
	}

	errs := map[string][]string{}
	name := validateName(input.Name, errs)
	note := normaliseNote(input.Note, errs)
	if len(errs) > 0 {
		return nil, apierror.Validation(errs)
	}

	total, err := s.repo.CountForUser(ctx, userID)
	if err != nil {
		return nil, s.internal("count", err)
	}
	if total >= maxPerUser {
		return nil, apierror.Validation(map[string][]string{
			"name": {"You already have the maximum number of pen names."},
		})
	}

	created, err := s.repo.Create(ctx, userID, name, note, input.IsDefault)
	if errors.Is(err, ErrNameTaken) {
		return nil, nameTaken()
	}
	if err != nil {
		return nil, s.internal("create", err)
	}
	view := created.View()
	return &view, nil
}

// Update renames an identity, re-labels it, or makes it the default.
//
// A rename records the PREVIOUS name in pen_name_history, in the same
// transaction as the rename itself - that record is what lets a profile say
// «เคยใช้ชื่อ …» for thirty days, and it is the platform's whole answer to
// impersonation by name change (docs/PROFILE-AND-ACHIEVEMENTS.md Part 2).
//
// Nothing here touches a fiction. A work published under this identity keeps
// every word it had; only the name on it changes, which is exactly what the
// writer asked for.
func (s *Service) Update(
	ctx context.Context, identity *auth.Identity, id uuid.UUID, input UpdateInput,
) (*View, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return nil, err
	}

	errs := map[string][]string{}
	if input.Name != nil {
		name := validateName(*input.Name, errs)
		input.Name = &name
	}
	if input.Note != nil && *input.Note != nil {
		*input.Note = normaliseNote(*input.Note, errs)
	}
	if len(errs) > 0 {
		return nil, apierror.Validation(errs)
	}

	updated, err := s.repo.Update(ctx, userID, id, input)
	if errors.Is(err, ErrNotFound) {
		return nil, notFound()
	}
	if errors.Is(err, ErrNameTaken) {
		return nil, nameTaken()
	}
	if err != nil {
		return nil, s.internal("update", err)
	}
	view := updated.View()
	return &view, nil
}

// SetDefault makes one identity the account's fallback.
//
// A named method as well as a PATCH field, because "which name goes on a work
// that names none" is a decision worth being able to point at in the code.
func (s *Service) SetDefault(
	ctx context.Context, identity *auth.Identity, id uuid.UUID,
) (*View, error) {
	isDefault := true
	return s.Update(ctx, identity, id, UpdateInput{IsDefault: &isDefault})
}

// Delete removes one of the caller's identities.
//
// It deletes NOTHING ELSE. `novels.pen_name_id` is ON DELETE SET NULL, so every
// fiction published under this name keeps its chapters, its messages, its
// revisions and its publication history, and falls back to the writer's default
// name. This is the same rule a format change follows: a metadata change writes
// no content (CLAUDE.md - Format Changes, Writer-First Principles).
func (s *Service) Delete(ctx context.Context, identity *auth.Identity, id uuid.UUID) error {
	userID, err := requireUser(identity)
	if err != nil {
		return err
	}

	err = s.repo.Delete(ctx, userID, id)
	if errors.Is(err, ErrNotFound) {
		return notFound()
	}
	if err != nil {
		return s.internal("delete", err)
	}
	return nil
}

// Recent returns the names this person used within the last thirty days and no
// longer uses - the «เคยใช้ชื่อ …» line on a profile.
//
// It takes a user id rather than an identity on purpose: the answer is the same
// for everyone who can see the profile, which is what makes it publishable
// beside a viewer-independent profile read (docs/14 §7).
func (s *Service) Recent(ctx context.Context, userID uuid.UUID) ([]string, error) {
	names, err := s.repo.Recent(ctx, userID)
	if err != nil {
		return nil, s.internal("recent", err)
	}
	return names, nil
}
