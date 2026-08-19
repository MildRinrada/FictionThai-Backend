package variables

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/internal/novels"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
)

// NovelAccess is the slice of the novels domain this service needs.
//
// Consumer-defined, so the dependency stays one-directional (variables ->
// novels, never back) and the authorization boundary is explicit: every
// operation starts by asking the novels service whether the caller may read or
// write the parent fiction (docs/10 §27).
type NovelAccess interface {
	ForReader(ctx context.Context, identity *auth.Identity, ref novels.Ref) (*novels.Novel, error)
	ForEditor(ctx context.Context, identity *auth.Identity, ref novels.Ref) (*novels.Novel, error)
}

// Service owns reader-variable business rules.
type Service struct {
	repo   *Repository
	novels NovelAccess
	log    *slog.Logger
}

func NewService(repo *Repository, novelAccess NovelAccess, log *slog.Logger) *Service {
	return &Service{repo: repo, novels: novelAccess, log: log}
}

// Result is a declaration list plus the advisory usage report.
type Result struct {
	Variables []View `json:"variables"`
	Usage     Usage  `json:"usage"`
}

// List returns a fiction's variables.
//
// PUBLIC, under the fiction's own gate: a reader needs the declarations to be
// asked the questions and to fill the slots, and a guest must get them too
// (docs/10 §2.1). There is nothing private here - the ANSWERS are the private
// part, and they never reach the server.
func (s *Service) List(
	ctx context.Context, identity *auth.Identity, ref novels.Ref,
) (*Result, error) {
	novel, err := s.novels.ForReader(ctx, identity, ref)
	if err != nil {
		return nil, err
	}

	list, err := s.repo.List(ctx, novel.ID)
	if err != nil {
		return nil, s.internal("list variables", err)
	}

	result := &Result{Variables: viewsOf(list), Usage: emptyUsage()}

	// The usage scan is for the WRITER. Running it for every reader would put a
	// full-text scan of the fiction on the reader path for nobody's benefit.
	if s.canManage(identity, novel) {
		usage, err := s.repo.Usage(ctx, novel.ID, allTokens(list))
		if err != nil {
			// Advisory data must never take the declarations down with it.
			s.log.Warn("variables: usage scan failed", slog.Any("error", err))
		} else {
			result.Usage = s.withMentions(ctx, novel.ID, usage)
		}
	}
	return result, nil
}

// withMentions splits character-name tokens out of the undeclared report,
// against the fiction's own cast. A failed cast read degrades to the unsplit
// report - advisory data never takes the page down.
func (s *Service) withMentions(
	ctx context.Context, novelID uuid.UUID, usage Usage,
) Usage {
	if len(usage.Undeclared) == 0 {
		return usage
	}
	cast, err := s.repo.CastNames(ctx, novelID)
	if err != nil {
		s.log.Warn("variables: cast read failed", slog.Any("error", err))
		return usage
	}
	return classifyMentions(usage, cast)
}

// Replace sets a fiction's whole declaration list. Owner or staff only.
//
// The whole list, not a per-row API: order is the declaration order a reader is
// asked in, and expressing a reorder as a series of row edits would let a
// half-applied change leave two variables claiming one position.
//
// It writes NO chapter content. Renaming a token here does not rewrite the text
// that uses the old one - the writer is told about it through Usage instead,
// because rewriting an author's manuscript to follow a settings change is
// exactly what this platform does not do.
func (s *Service) Replace(
	ctx context.Context, identity *auth.Identity, ref novels.Ref, inputs []Input,
) (*Result, error) {
	novel, err := s.novels.ForEditor(ctx, identity, ref)
	if err != nil {
		return nil, err
	}

	list, err := Validate(inputs)
	if err != nil {
		return nil, err
	}

	saved, err := s.repo.Replace(ctx, novel.ID, list)
	if err != nil {
		return nil, s.internal("replace variables", err)
	}

	result := &Result{Variables: viewsOf(saved)}
	usage, err := s.repo.Usage(ctx, novel.ID, allTokens(saved))
	if err != nil {
		s.log.Warn("variables: usage scan failed", slog.Any("error", err))
		usage = emptyUsage()
	}
	result.Usage = s.withMentions(ctx, novel.ID, usage)
	return result, nil
}

// canManage mirrors the novels service: the owner, a collaborator (13U), or
// staff.
func (s *Service) canManage(identity *auth.Identity, novel *novels.Novel) bool {
	if !identity.Authenticated() {
		return false
	}
	return novel.EditableBy(identity.UserID()) || identity.IsStaff()
}

func viewsOf(list []Variable) []View {
	views := make([]View, 0, len(list))
	for _, variable := range list {
		views = append(views, variable.View())
	}
	return views
}

// allTokens flattens every literal placeholder the declarations produce,
// including one per pronoun form - a form token used in the text must not be
// reported as an unused declaration.
func allTokens(list []Variable) []string {
	tokens := []string{}
	for _, variable := range list {
		tokens = append(tokens, variable.AllTokens()...)
	}
	return tokens
}

func (s *Service) internal(op string, err error) error {
	s.log.Error("variables: "+op+" failed", slog.Any("error", err))
	return apierror.Internal()
}

// Compile-time assurance that the real novels service satisfies the narrow
// interface declared above.
var _ NovelAccess = (*novels.Service)(nil)
