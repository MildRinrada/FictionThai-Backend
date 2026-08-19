package wall

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/internal/novels"
	"github.com/fictionthai/fictionthai/backend/internal/profiles"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/pagination"
)

// MaxPerAuthor bounds how many live messages one account may have on one wall.
//
// The rate limiter caps how FAST someone writes; this caps how much of another
// person's page one voice can occupy, which is a different problem and the one
// a wall actually has.
const MaxPerAuthor = 20

// Service owns the wall's rules and is the authorization boundary for every
// wall endpoint (docs/10 §27).
type Service struct {
	repo *Repository
	log  *slog.Logger
}

func NewService(repo *Repository, log *slog.Logger) *Service {
	return &Service{repo: repo, log: log}
}

func userNotFound() *apierror.Error {
	return apierror.New(http.StatusNotFound, "USER_NOT_FOUND", "User not found.")
}

// wallOff is what a closed wall answers. A 404 rather than a 403 because there
// is nothing there to be forbidden from - and the code says which 404 it is, so
// a client renders nothing instead of an error. The state is not a secret:
// wall_enabled is on the public profile, which is how the page knows not to ask
// in the first place.
func wallOff() *apierror.Error {
	return apierror.New(http.StatusNotFound, "WALL_DISABLED",
		"This person has turned their profile wall off.")
}

func entryNotFound() *apierror.Error {
	return apierror.New(http.StatusNotFound, "WALL_ENTRY_NOT_FOUND", "Message not found.")
}

func requireUser(identity *auth.Identity) (uuid.UUID, error) {
	if !identity.Authenticated() {
		return uuid.Nil, apierror.Unauthorized("Authentication required.")
	}
	return identity.UserID(), nil
}

// viewerID is uuid.Nil for guests; used only to mark is_owner / can_delete.
func viewerID(identity *auth.Identity) uuid.UUID {
	if !identity.Authenticated() {
		return uuid.Nil
	}
	return identity.UserID()
}

// target resolves the wall being addressed, refusing a closed one.
func (s *Service) target(ctx context.Context, raw string) (Target, error) {
	ref, err := profiles.ParseRef(raw)
	if err != nil {
		return Target{}, userNotFound()
	}
	target, err := s.repo.Target(ctx, ref)
	if errors.Is(err, ErrNotFound) {
		return Target{}, userNotFound()
	}
	if err != nil {
		return Target{}, s.internal("resolve wall target", err)
	}
	if !target.Enabled {
		return Target{}, wallOff()
	}
	return target, nil
}

// List returns one page of somebody's wall.
//
// Guest-first: reading a wall needs no account, exactly as reading a fiction's
// discussion needs none (docs/03 §27). The identity is used only to mark which
// entries the caller may remove.
func (s *Service) List(
	ctx context.Context, identity *auth.Identity, raw string, page pagination.Params,
) ([]View, pagination.Meta, error) {
	target, err := s.target(ctx, raw)
	if err != nil {
		return nil, pagination.Meta{}, err
	}

	entries, total, err := s.repo.List(ctx, target.UserID, page)
	if err != nil {
		return nil, pagination.Meta{}, s.internal("list wall", err)
	}

	viewer := viewerID(identity)
	views := make([]View, 0, len(entries))
	for i := range entries {
		views = append(views, entries[i].Render(viewer))
	}
	return views, page.MetaFor(total), nil
}

// Post leaves a message on somebody's wall.
//
// Authenticated always - there is no guest wall (see the package doc). NOT
// verification-gated: email verification gates PUBLISHING FICTION, never
// ordinary account use (docs/AUTHENTICATION.md §9).
//
// Writing on your own wall is allowed. It is where a writer answers what people
// left them, and refusing it would push that conversation somewhere the page
// cannot show.
func (s *Service) Post(
	ctx context.Context, identity *auth.Identity, raw, rawBody string,
) (*View, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return nil, err
	}
	target, err := s.target(ctx, raw)
	if err != nil {
		return nil, err
	}
	body, err := validateBody(rawBody)
	if err != nil {
		return nil, err
	}

	mine, err := s.repo.CountByAuthor(ctx, target.UserID, userID)
	if err != nil {
		return nil, s.internal("count wall entries", err)
	}
	if mine >= MaxPerAuthor {
		return nil, apierror.Validation(map[string][]string{
			"body": {"คุณฝากข้อความไว้ที่หน้านี้มากเกินไปแล้ว ลบข้อความเก่าก่อนได้"},
		})
	}

	entry, err := s.repo.Create(ctx, target.UserID, userID, body)
	if err != nil {
		return nil, s.internal("create wall entry", err)
	}
	view := entry.Render(userID)
	return &view, nil
}

// Delete removes one message.
//
// Either the AUTHOR or the PROFILE OWNER may do it, and staff may too - the
// same owner-or-moderator shape docs/09 §20 gives a comment, widened by exactly
// one person: whoever the page belongs to. Idempotent: deleting an
// already-deleted message is a success (docs/09 §33).
func (s *Service) Delete(
	ctx context.Context, identity *auth.Identity, entryID uuid.UUID,
) error {
	userID, err := requireUser(identity)
	if err != nil {
		return err
	}

	entry, err := s.repo.Find(ctx, entryID)
	if errors.Is(err, ErrNotFound) {
		return entryNotFound()
	}
	if err != nil {
		return s.internal("load wall entry", err)
	}
	if entry.DeletedAt != nil {
		return nil
	}
	if !entry.RemovableBy(userID) && !identity.IsStaff() {
		// A wall entry is public, so 403 confirms nothing the listing did not
		// already show - unlike a private shelf, where a 404 is required.
		return apierror.Forbidden("Only the person who wrote this, or the owner of this page, may remove it.")
	}

	if err := s.repo.SoftDelete(ctx, entry.ID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil // raced with another delete of the same entry
		}
		return s.internal("delete wall entry", err)
	}

	s.log.Info("wall entry deleted",
		slog.String("entry_id", entry.ID.String()),
		slog.String("actor_id", userID.String()),
		slog.Bool("by_author", entry.AuthorID == userID),
		slog.Bool("by_profile_owner", entry.ProfileUserID == userID),
	)
	return nil
}

// validateBody trims and bounds one message. The trimmed form is what gets
// stored - trailing whitespace is never meaningful.
func validateBody(raw string) (string, error) {
	body := strings.TrimSpace(raw)
	switch {
	case body == "":
		return "", apierror.Validation(map[string][]string{
			"body": {"เขียนข้อความก่อนส่ง"},
		})
	case utf8.RuneCountInString(body) > MaxBodyRunes:
		return "", apierror.Validation(map[string][]string{
			"body": {"ข้อความยาวเกินไป (ไม่เกิน 1,000 ตัวอักษร)"},
		})
	case !novels.SafeText(body):
		return "", apierror.Validation(map[string][]string{
			"body": {"ข้อความมีอักขระที่ใช้ไม่ได้"},
		})
	}
	return body, nil
}

// internal logs the real failure and returns the opaque error (docs/11 §67).
func (s *Service) internal(op string, err error) error {
	s.log.Error("wall service failure", slog.String("op", op), slog.Any("error", err))
	return apierror.Internal()
}
