package authors

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
)

// Service owns author-profile business rules. It is self-scoped: every write
// targets the CALLER's own row via identity.UserID(), the same pattern as
// users.SetAvatarURL - there is deliberately no cross-user profile-edit path
// (addendum §4–§5).
type Service struct {
	repo *Repository
	log  *slog.Logger
}

func NewService(repo *Repository, log *slog.Logger) *Service {
	return &Service{repo: repo, log: log}
}

func requireUser(identity *auth.Identity) (uuid.UUID, error) {
	if !identity.Authenticated() {
		return uuid.Nil, apierror.Unauthorized("Authentication required.")
	}
	return identity.UserID(), nil
}

func (s *Service) internal(op string, err error) error {
	s.log.Error("authors: "+op+" failed", slog.Any("error", err))
	return apierror.Internal()
}

// GetMine returns the caller's own author-profile view. A user with no author
// profile is NOT an error - they simply have no donation link yet, so an empty
// view is returned rather than a 404.
func (s *Service) GetMine(ctx context.Context, identity *auth.Identity) (ProfileView, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return ProfileView{}, err
	}
	profile, err := s.repo.FindByUserID(ctx, userID)
	if errors.Is(err, ErrNotFound) {
		return ProfileView{}, nil
	}
	if err != nil {
		return ProfileView{}, s.internal("load author profile", err)
	}
	return profile.View(), nil
}

// SetDonationURL validates and stores the caller's external writer-support link
// (brief §6). A nil or empty value clears it. Creating the author_profiles row
// on first save is the documented on-demand behaviour (see package doc).
func (s *Service) SetDonationURL(
	ctx context.Context, identity *auth.Identity, donationURL *string,
) (ProfileView, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return ProfileView{}, err
	}

	clean, err := validateDonationURL(donationURL)
	if err != nil {
		return ProfileView{}, err
	}

	profile, err := s.repo.UpsertDonationURL(ctx, userID, clean)
	if err != nil {
		return ProfileView{}, s.internal("save donation url", err)
	}
	s.log.Info("author donation url updated",
		slog.String("user_id", userID.String()),
		slog.Bool("set", clean != nil))
	return profile.View(), nil
}
