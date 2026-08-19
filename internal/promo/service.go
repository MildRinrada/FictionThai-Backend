package promo

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
)

// Service owns the queue's business rules.
type Service struct {
	repo *Repository
	log  *slog.Logger
}

func NewService(repo *Repository, log *slog.Logger) *Service {
	return &Service{repo: repo, log: log}
}

func (s *Service) internal(op string, err error) error {
	s.log.Error("promo: "+op+" failed", slog.Any("error", err))
	return apierror.Internal()
}

func notFound() *apierror.Error {
	return apierror.New(http.StatusNotFound, "PROMO_SLIDE_NOT_FOUND", "Slide not found.")
}

// requireStaff re-checks the role the middleware already gated on - the same
// double gate moderation uses (docs/10 §48): the route gets a caller through
// the door, the service still refuses a non-staff identity.
func requireStaff(identity *auth.Identity) error {
	if !identity.Authenticated() {
		return apierror.Unauthorized("Authentication required.")
	}
	if !identity.IsStaff() {
		return apierror.Forbidden("Staff only.")
	}
	return nil
}

// Active is the PUBLIC read: the live queue with the deck rules applied
// (docs/HOME-PROMO.md), each serving counted as an impression. The counters
// are advisory - a counting failure is logged and never fails the read.
func (s *Service) Active(ctx context.Context) ([]View, error) {
	live, err := s.repo.Live(ctx, time.Now())
	if err != nil {
		return nil, s.internal("list live slides", err)
	}
	served := ServeQueue(live)

	views := make([]View, 0, len(served))
	ids := make([]uuid.UUID, 0, len(served))
	for _, slide := range served {
		views = append(views, slide.View())
		ids = append(ids, slide.ID)
	}
	if err := s.repo.CountImpressions(ctx, ids); err != nil {
		s.log.Warn("promo: impression count failed", slog.Any("error", err))
	}
	return views, nil
}

// Click records one click on a live slide. Public and anonymous: guests click
// slides too, and the counter is an indicator, not billing (docs/HOME-PROMO.md
// "Stats"). An unknown id is a silent no-op - a slide can leave the queue
// while somebody's page still shows it.
func (s *Service) Click(ctx context.Context, id uuid.UUID) error {
	if err := s.repo.CountClick(ctx, id); err != nil {
		return s.internal("count click", err)
	}
	return nil
}

// Queue is the staff read: every slide, counters included.
func (s *Service) Queue(ctx context.Context, identity *auth.Identity) ([]AdminView, error) {
	if err := requireStaff(identity); err != nil {
		return nil, err
	}
	slides, err := s.repo.All(ctx)
	if err != nil {
		return nil, s.internal("list slides", err)
	}
	views := make([]AdminView, 0, len(slides))
	for _, slide := range slides {
		views = append(views, slide.AdminView())
	}
	return views, nil
}

// Create validates and appends a slide.
func (s *Service) Create(
	ctx context.Context, identity *auth.Identity, in Input,
) (AdminView, error) {
	if err := requireStaff(identity); err != nil {
		return AdminView{}, err
	}
	slide, err := Validate(in)
	if err != nil {
		return AdminView{}, err
	}
	created, err := s.repo.Create(ctx, slide)
	if err != nil {
		return AdminView{}, s.internal("create slide", err)
	}
	return created.AdminView(), nil
}

// Update replaces a slide's fields.
func (s *Service) Update(
	ctx context.Context, identity *auth.Identity, id uuid.UUID, in Input,
) (AdminView, error) {
	if err := requireStaff(identity); err != nil {
		return AdminView{}, err
	}
	slide, err := Validate(in)
	if err != nil {
		return AdminView{}, err
	}
	updated, err := s.repo.Update(ctx, id, slide)
	if errors.Is(err, ErrNotFound) {
		return AdminView{}, notFound()
	}
	if err != nil {
		return AdminView{}, s.internal("update slide", err)
	}
	return updated.AdminView(), nil
}

// Delete removes a slide.
func (s *Service) Delete(ctx context.Context, identity *auth.Identity, id uuid.UUID) error {
	if err := requireStaff(identity); err != nil {
		return err
	}
	err := s.repo.Delete(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return notFound()
	}
	if err != nil {
		return s.internal("delete slide", err)
	}
	return nil
}

// Reorder rewrites the queue's positions from the submitted id order.
func (s *Service) Reorder(
	ctx context.Context, identity *auth.Identity, ids []uuid.UUID,
) ([]AdminView, error) {
	if err := requireStaff(identity); err != nil {
		return nil, err
	}
	if len(ids) == 0 || len(ids) > 100 {
		return nil, apierror.Validation(map[string][]string{
			"ids": {"Between 1 and 100 slide ids."},
		})
	}
	if err := s.repo.SetOrder(ctx, ids); err != nil {
		return nil, s.internal("reorder slides", err)
	}
	return s.Queue(ctx, identity)
}
