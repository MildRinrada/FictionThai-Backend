package community

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/internal/chapters"
	"github.com/fictionthai/fictionthai/backend/internal/novels"
	"github.com/fictionthai/fictionthai/backend/internal/users"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/pagination"
)

// Content bounds, in runes - Thai text must not get a third of the room
// (docs/11 §16 input validation, same rationale as fiction comments).
const (
	MaxPostRunes    = 10000
	MaxCommentRunes = 5000
)

// DiscussedLimit bounds the community sidebar. Small on purpose: it is a
// pointer into discovery, not a chart.
const DiscussedLimit = 6

// TrendingTagLimit bounds the "แท็กที่กำลังพูดถึง" panel and the `#`
// autocomplete, for the same reason.
const TrendingTagLimit = 8

// MaxSearchRunes bounds the free-text needle; anything longer is not a query
// (docs/COMMUNITY-FEED.md, same rationale as the novels search).
const MaxSearchRunes = 200

// UserLookup resolves `?author=<username>` to an id, exactly like the novels
// listing. Soft-deleted accounts are already excluded by the users repository.
type UserLookup interface {
	FindByUsername(ctx context.Context, username string) (*users.User, error)
}

// NovelAccess resolves the fiction a post attaches to. Reading the fiction is
// the gate for attaching it: a caller may only point at work they could open
// themselves, so the composer can never be used to confirm that a private
// fiction exists (docs/11 §3.4, §31).
type NovelAccess interface {
	ForReader(ctx context.Context, identity *auth.Identity, ref novels.Ref) (*novels.Novel, error)
}

// ChapterAccess is the chapter counterpart. Resolving through the fiction is
// what guarantees the pair is coherent - a chapter of some OTHER fiction can
// never be stored beside this novel_id.
type ChapterAccess interface {
	ResolveForReader(
		ctx context.Context, identity *auth.Identity,
		novelRef novels.Ref, chapterRef chapters.Ref,
	) (*novels.Novel, *chapters.Chapter, error)
}

// Notifier is the slice of the notifications domain this service needs
// (docs/08 §23.1 reserves `community_reaction`; comments follow the
// documented new_comment/comment_reply semantics). Consumer-defined, so the
// dependency stays one-directional; fire-and-forget because the action has
// already committed.
type Notifier interface {
	CommunityCommentCreated(ctx context.Context, actorID, commentID uuid.UUID)
	CommunityReactionAdded(ctx context.Context, actorID, postID uuid.UUID)
}

// Service owns community business rules and is the authorization boundary for
// every community endpoint (docs/10 §27). Identity always comes from the
// authenticated session - never from the request body.
type Service struct {
	repo     *Repository
	users    UserLookup
	novels   NovelAccess
	chapters ChapterAccess
	notifier Notifier
	log      *slog.Logger
}

// NewService wires the service. notifier may be nil: community actions then
// simply emit nothing.
func NewService(
	repo *Repository, userLookup UserLookup,
	novelAccess NovelAccess, chapterAccess ChapterAccess,
	notifier Notifier, log *slog.Logger,
) *Service {
	return &Service{
		repo:     repo,
		users:    userLookup,
		novels:   novelAccess,
		chapters: chapterAccess,
		notifier: notifier,
		log:      log,
	}
}

func postNotFound() *apierror.Error {
	return apierror.New(http.StatusNotFound, "COMMUNITY_POST_NOT_FOUND", "Post not found.")
}

func commentNotFound() *apierror.Error {
	return apierror.New(http.StatusNotFound, "COMMUNITY_COMMENT_NOT_FOUND", "Comment not found.")
}

func requireUser(identity *auth.Identity) (uuid.UUID, error) {
	if !identity.Authenticated() {
		return uuid.Nil, apierror.Unauthorized("Authentication required.")
	}
	return identity.UserID(), nil
}

// viewerID is uuid.Nil for guests.
func viewerID(identity *auth.Identity) uuid.UUID {
	if !identity.Authenticated() {
		return uuid.Nil
	}
	return identity.UserID()
}

// canModerate implements "the owner, or staff" for posts and comments alike.
func canModerate(identity *auth.Identity, ownerID uuid.UUID) bool {
	if !identity.Authenticated() {
		return false
	}
	return ownerID == identity.UserID() || identity.IsStaff()
}

// validateContent trims and bounds a text field.
func validateContent(raw string, maxRunes int, empty, tooLong string) (string, error) {
	content := strings.TrimSpace(raw)
	if content == "" {
		return "", apierror.Validation(map[string][]string{"content": {empty}})
	}
	if utf8.RuneCountInString(content) > maxRunes {
		return "", apierror.Validation(map[string][]string{"content": {tooLong}})
	}
	return content, nil
}

func validatePostContent(raw string) (string, error) {
	return validateContent(raw, MaxPostRunes,
		"A post cannot be empty.", "A post cannot be longer than 10000 characters.")
}

func validateCommentContent(raw string) (string, error) {
	return validateContent(raw, MaxCommentRunes,
		"A comment cannot be empty.", "A comment cannot be longer than 5000 characters.")
}

// ---------------------------------------------------------------------------
// Fiction references (docs/PHASE-12-STORY-DEPTH.md §12D)
// ---------------------------------------------------------------------------

// ReferenceInput is the attachment as the client sent it: a fiction, and
// optionally a chapter within it. Both accept an id or a slug, like every
// other reference in the API.
type ReferenceInput struct {
	NovelRef   string
	ChapterRef string
}

// Empty reports whether the caller attached nothing.
func (r ReferenceInput) Empty() bool {
	return strings.TrimSpace(r.NovelRef) == "" && strings.TrimSpace(r.ChapterRef) == ""
}

func referenceUnavailable(field string) error {
	return apierror.Validation(map[string][]string{
		field: {"That work is not available."},
	})
}

// resolveReference turns the client's refs into stored ids, under the AUTHOR's
// own read access.
//
// Every failure - absent, deleted, private, someone else's draft, a chapter
// from a different fiction - is the SAME message, so the composer cannot be
// used to learn which of those a given id is (docs/11 §3.4). Attaching work
// that is merely unpublished is allowed on purpose: visibility is resolved
// when the post is READ, so a writer may queue a post about a chapter that is
// not out yet and readers simply see no card until it is.
func (s *Service) resolveReference(
	ctx context.Context, identity *auth.Identity, input ReferenceInput,
) (novelID, chapterID *uuid.UUID, err error) {
	novelRaw := strings.TrimSpace(input.NovelRef)
	chapterRaw := strings.TrimSpace(input.ChapterRef)

	if novelRaw == "" {
		if chapterRaw != "" {
			return nil, nil, apierror.Validation(map[string][]string{
				"novel_id": {"A chapter reference must name its fiction as well."},
			})
		}
		return nil, nil, nil
	}

	novelRef, err := novels.ParseRef(novelRaw)
	if err != nil {
		return nil, nil, referenceUnavailable("novel_id")
	}

	if chapterRaw == "" {
		novel, err := s.novels.ForReader(ctx, identity, novelRef)
		if err != nil {
			return nil, nil, referenceUnavailable("novel_id")
		}
		id := novel.ID
		return &id, nil, nil
	}

	chapterRef, err := chapters.ParseRef(chapterRaw)
	if err != nil {
		return nil, nil, referenceUnavailable("chapter_id")
	}
	novel, chapter, err := s.chapters.ResolveForReader(ctx, identity, novelRef, chapterRef)
	if err != nil {
		return nil, nil, referenceUnavailable("chapter_id")
	}

	novelID = &novel.ID
	chapterID = &chapter.ID
	return novelID, chapterID, nil
}

// ---------------------------------------------------------------------------
// Posts (docs/09 §21)
// ---------------------------------------------------------------------------

// ListQuery is the validated shape of GET /community/posts.
type ListQuery struct {
	// Author narrows to one author's posts by username; unknown names yield
	// an empty page, never an oracle.
	Author string

	// Feed selects the feed type (docs/09 §21): "" or "all" is everything the
	// viewer may see; "following" narrows to authors the viewer follows and
	// therefore requires a signed-in caller; "attached" narrows to posts that
	// point at a fiction (§12D).
	Feed string

	// Novel narrows to posts attached to ONE fiction, by id or slug (§13R). An
	// unreadable or unknown ref yields an empty page rather than an error, the
	// same way an unknown author does - a filter must never become a way to
	// learn that a private fiction exists.
	Novel string

	// Type narrows any feed to one declared post type
	// (docs/COMMUNITY-FEED.md) - the หาเบต้า and อีเวนต์เขียน entries.
	Type string

	// Sort orders the feed: "" / "new" is recency, "top" is engagement
	// (docs/COMMUNITY-FEED.md). The saved feed ignores it - it reads in save
	// order.
	Sort string
}

// ListPosts returns one page of the feed the viewer is entitled to.
func (s *Service) ListPosts(
	ctx context.Context, identity *auth.Identity, query ListQuery, page pagination.Params,
) ([]PostView, pagination.Meta, error) {
	filter := PostFilter{}

	switch query.Feed {
	case "", "all":
		// The default feed.
	case "following":
		if !identity.Authenticated() {
			return nil, pagination.Meta{}, apierror.Unauthorized(
				"Sign in to see posts from people you follow.")
		}
		filter.FollowingOnly = true
	case "attached":
		filter.WithReferenceOnly = true
	case "mine":
		if !identity.Authenticated() {
			return nil, pagination.Meta{}, apierror.Unauthorized(
				"Sign in to see your own posts.")
		}
		filter.AuthorID = identity.UserID()
	case "saved":
		if !identity.Authenticated() {
			return nil, pagination.Meta{}, apierror.Unauthorized(
				"Sign in to see your saved posts.")
		}
		filter.BookmarkedBy = identity.UserID()
	default:
		return nil, pagination.Meta{}, apierror.Validation(map[string][]string{
			"feed": {"Must be one of: all, following, attached, mine, saved."},
		})
	}

	if query.Type != "" {
		if !ValidPostType(query.Type) {
			return nil, pagination.Meta{}, apierror.Validation(map[string][]string{
				"type": {"Must be one of: " + strings.Join(PostTypeList(), ", ") + "."},
			})
		}
		filter.Type = query.Type
	}

	switch query.Sort {
	case "", "new":
	case "top":
		filter.SortTop = true
	default:
		return nil, pagination.Meta{}, apierror.Validation(map[string][]string{
			"sort": {"Must be one of: new, top."},
		})
	}

	if author := strings.TrimSpace(query.Author); author != "" {
		user, err := s.users.FindByUsername(ctx, author)
		if errors.Is(err, users.ErrNotFound) {
			// An unknown author is an empty page, matching the novels listing.
			return []PostView{}, page.MetaFor(0), nil
		}
		if err != nil {
			return nil, pagination.Meta{}, s.internal("resolve author", err)
		}
		if filter.AuthorID != uuid.Nil && filter.AuthorID != user.ID {
			// ?feed=mine already pinned the author; a different ?author= can
			// match nothing, honestly.
			return []PostView{}, page.MetaFor(0), nil
		}
		filter.AuthorID = user.ID
	}

	if ref := strings.TrimSpace(query.Novel); ref != "" {
		parsed, err := novels.ParseRef(ref)
		if err != nil {
			return []PostView{}, page.MetaFor(0), nil
		}
		novel, err := s.novels.ForReader(ctx, identity, parsed)
		if err != nil {
			// Absent, deleted, private, someone else's draft - all the same
			// empty page, for the reason resolveReference gives above.
			return []PostView{}, page.MetaFor(0), nil
		}
		filter.NovelID = novel.ID
	}

	viewer := viewerID(identity)
	posts, total, err := s.repo.ListPosts(ctx, viewer, filter, page)
	if err != nil {
		return nil, pagination.Meta{}, s.internal("list posts", err)
	}

	views := make([]PostView, 0, len(posts))
	for i := range posts {
		views = append(views, posts[i].Render(viewer))
	}
	return views, page.MetaFor(total), nil
}

// GetPost returns one post the viewer may open.
func (s *Service) GetPost(
	ctx context.Context, identity *auth.Identity, id uuid.UUID,
) (*PostView, error) {
	viewer := viewerID(identity)
	post, err := s.repo.FindVisiblePost(ctx, viewer, id)
	if errors.Is(err, ErrNotFound) {
		return nil, postNotFound()
	}
	if err != nil {
		return nil, s.internal("load post", err)
	}
	view := post.Render(viewer)
	return &view, nil
}

// CreatePost publishes a community post (docs/09 §21). Authenticated, but
// NEVER verification-gated: the access matrix (docs/03 §27) lets any
// signed-in user post; email verification gates publishing FICTION only.
func (s *Service) CreatePost(
	ctx context.Context, identity *auth.Identity, input CreatePostInput,
) (*PostView, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return nil, err
	}
	content, err := validatePostContent(input.Content)
	if err != nil {
		return nil, err
	}

	visibility := VisibilityPublic
	if input.Visibility != "" {
		if !ValidVisibility(input.Visibility) {
			return nil, apierror.Validation(map[string][]string{
				"visibility": {"Must be one of: public, followers, private."},
			})
		}
		visibility = Visibility(input.Visibility)
	}

	postType := PostTypeDiscussion
	if input.Type != "" {
		if !ValidPostType(input.Type) {
			return nil, apierror.Validation(map[string][]string{
				"post_type": {"Must be one of: " + strings.Join(PostTypeList(), ", ") + "."},
			})
		}
		postType = PostType(input.Type)
	}

	novelID, chapterID, err := s.resolveReference(ctx, identity, input.Reference)
	if err != nil {
		return nil, err
	}

	post, err := s.repo.CreatePost(ctx, CreatePostParams{
		AuthorID:   userID,
		Content:    content,
		Visibility: visibility,
		Type:       postType,
		NovelID:    novelID,
		ChapterID:  chapterID,
	})
	if err != nil {
		return nil, s.internal("create post", err)
	}

	view := post.Render(userID)
	return &view, nil
}

// CreatePostInput is the validated shape of POST /community/posts.
type CreatePostInput struct {
	Content    string
	Visibility string

	// Type is the author's declared intent; empty means discussion.
	Type string

	// Reference is the attached fiction; zero means none.
	Reference ReferenceInput
}

// UpdatePostInput is a partial edit; nil fields stay untouched.
type UpdatePostInput struct {
	Content    *string
	Visibility *string
	Type       *string

	// Reference follows the three-case rule (docs/09 §3): nil leaves the
	// attachment alone, a non-nil pointer replaces it, and a non-nil pointer
	// to an EMPTY value detaches. Editing the text of a post about a fiction
	// the editor can no longer open therefore keeps the attachment rather than
	// silently dropping it.
	Reference *ReferenceInput
}

// UpdatePost edits the caller's own post (staff may act under docs/01 §21).
// Changing visibility is non-destructive both ways: private → public → private
// round-trips with nothing lost, matching the format-change principle.
func (s *Service) UpdatePost(
	ctx context.Context, identity *auth.Identity, id uuid.UUID, input UpdatePostInput,
) (*PostView, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return nil, err
	}

	post, err := s.repo.FindPost(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return nil, postNotFound()
	}
	if err != nil {
		return nil, s.internal("load post", err)
	}
	if post.DeletedAt != nil || post.Status != PostStatusPublished {
		return nil, postNotFound()
	}
	// A visible post's existence is not a secret to anyone inside its
	// audience, but a PRIVATE post's is: only the owner gets an honest 403 vs
	// 404 distinction - and the owner is the only one who can ever see it, so
	// every non-owner gets the same 404 the visibility predicate would give.
	if post.AuthorID != userID && !identity.IsStaff() {
		if post.Visibility == VisibilityPublic {
			return nil, apierror.Forbidden("Only the post's author may edit it.")
		}
		return nil, postNotFound()
	}

	params := UpdatePostParams{}
	if input.Content != nil {
		content, err := validatePostContent(*input.Content)
		if err != nil {
			return nil, err
		}
		params.Content = &content
	}
	if input.Visibility != nil {
		if !ValidVisibility(*input.Visibility) {
			return nil, apierror.Validation(map[string][]string{
				"visibility": {"Must be one of: public, followers, private."},
			})
		}
		visibility := Visibility(*input.Visibility)
		params.Visibility = &visibility
	}
	if input.Type != nil {
		if !ValidPostType(*input.Type) {
			return nil, apierror.Validation(map[string][]string{
				"post_type": {"Must be one of: " + strings.Join(PostTypeList(), ", ") + "."},
			})
		}
		postType := PostType(*input.Type)
		params.Type = &postType
	}
	if input.Reference != nil {
		params.SetReference = true
		if !input.Reference.Empty() {
			novelID, chapterID, err := s.resolveReference(ctx, identity, *input.Reference)
			if err != nil {
				return nil, err
			}
			params.NovelID = novelID
			params.ChapterID = chapterID
		}
	}

	updated, err := s.repo.UpdatePost(ctx, post.ID, userID, params)
	if errors.Is(err, ErrNotFound) {
		return nil, postNotFound()
	}
	if err != nil {
		return nil, s.internal("update post", err)
	}

	view := updated.Render(userID)
	return &view, nil
}

// DeletePost soft-deletes the caller's own post. Idempotent (docs/09 §33).
// docs/11 §37: the thread beneath goes unreachable with it, and notification
// delivery re-checks the post, so nothing pending about it is delivered.
func (s *Service) DeletePost(
	ctx context.Context, identity *auth.Identity, id uuid.UUID,
) error {
	userID, err := requireUser(identity)
	if err != nil {
		return err
	}

	post, err := s.repo.FindPost(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return postNotFound()
	}
	if err != nil {
		return s.internal("load post", err)
	}
	if post.DeletedAt != nil {
		return nil
	}
	if post.AuthorID != userID && !identity.IsStaff() {
		if post.Visibility == VisibilityPublic {
			return apierror.Forbidden("Only the post's author may delete it.")
		}
		return postNotFound()
	}

	if err := s.repo.SoftDeletePost(ctx, post.ID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return s.internal("delete post", err)
	}

	s.log.Info("community post deleted",
		slog.String("post_id", post.ID.String()),
		slog.String("actor_id", userID.String()),
		slog.Bool("by_author", post.AuthorID == userID),
	)
	return nil
}

// ListDiscussedFictions returns the fictions public posts have been about
// lately - the community sidebar (§12D).
//
// Guest-safe and identical for everyone: it counts public posts only, and only
// fictions anyone may open. No identity is taken, because taking one would
// make a cacheable discovery panel personal.
func (s *Service) ListDiscussedFictions(ctx context.Context) ([]DiscussedFiction, error) {
	items, err := s.repo.ListDiscussedFictions(ctx, DiscussedLimit)
	if err != nil {
		return nil, s.internal("list discussed fictions", err)
	}
	return items, nil
}

// ListTrendingTags returns the hashtags recent public posts used most -
// "แท็กที่กำลังพูดถึง" and the `#` autocomplete (docs/COMMUNITY-FEED.md).
// Guest-safe and identical for everyone, like the discussed sidebar.
func (s *Service) ListTrendingTags(ctx context.Context, prefix string) ([]TrendingTag, error) {
	prefix = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(prefix), "#"))
	if utf8.RuneCountInString(prefix) > MaxHashtagRunes {
		// Longer than any stored tag: nothing can match, so say so cheaply.
		return []TrendingTag{}, nil
	}
	items, err := s.repo.ListTrendingTags(ctx, prefix, TrendingTagLimit)
	if err != nil {
		return nil, s.internal("list trending tags", err)
	}
	return items, nil
}

// ---------------------------------------------------------------------------
// Post search (docs/COMMUNITY-FEED.md)
// ---------------------------------------------------------------------------

// SearchQuery is the validated shape of GET /search/posts. The frontend
// parses the light operator syntax (from:@ to:@ has: fandom:"" #tag) into
// these fields; the API only ever sees structured parameters.
type SearchQuery struct {
	Q       string // free text; a leading # searches the tag instead
	From    string // "" | all | following | me
	Range   string // "" | all | 24h | 7d | month
	Has     string // "" | chapter | none
	Sort    string // "" | new | top
	Type    string // "" | a post type
	Author  string // from:@handle - a username, with or without the @
	Mention string // to:@handle - posts whose text mentions the handle
	Fandom  string // fandom:"..." - the attached fiction's fandom
	Tag     string // #tag as an explicit parameter
}

// SearchPosts returns one page of posts matching the query.
//
// THE SEARCH RULE (docs/COMMUNITY-FEED.md): search only ever surfaces PUBLIC
// posts, even to viewers who could read narrower posts in their feed. A
// followers-only post is shown to followers as they scroll; letting it be
// SEARCHED would turn the search box into a tool for combing through one
// person's narrower posts, which is exactly the stalking shape the rule
// exists to prevent. The one exception is searching your own posts
// (from:me), where the only person being searched is the caller.
func (s *Service) SearchPosts(
	ctx context.Context, identity *auth.Identity, query SearchQuery, page pagination.Params,
) ([]PostView, pagination.Meta, error) {
	filter := PostFilter{PublicOnly: true}
	fields := map[string][]string{}

	q := strings.TrimSpace(query.Q)
	if utf8.RuneCountInString(q) > MaxSearchRunes {
		fields["q"] = append(fields["q"], "A search query cannot be longer than 200 characters.")
	}
	if tag, isTag := strings.CutPrefix(q, "#"); isTag {
		// "#แท็ก" means the extracted tag, matched exactly - not the text "#แท็ก".
		filter.Tag = strings.TrimSpace(tag)
	} else {
		filter.Query = q
	}
	if tag := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(query.Tag), "#")); tag != "" {
		filter.Tag = tag
	}

	switch query.From {
	case "", "all":
		// Everyone's public posts.
	case "following":
		if !identity.Authenticated() {
			return nil, pagination.Meta{}, apierror.Unauthorized(
				"Sign in to search posts from people you follow.")
		}
		filter.FollowingOnly = true
	case "me":
		if !identity.Authenticated() {
			return nil, pagination.Meta{}, apierror.Unauthorized(
				"Sign in to search your own posts.")
		}
		filter.AuthorID = identity.UserID()
		// Your own posts are yours to search, whatever their audience.
		filter.PublicOnly = false
	default:
		fields["from"] = append(fields["from"], "Must be one of: all, following, me.")
	}

	now := time.Now().UTC()
	switch query.Range {
	case "", "all":
	case "24h":
		since := now.Add(-24 * time.Hour)
		filter.Since = &since
	case "7d":
		since := now.AddDate(0, 0, -7)
		filter.Since = &since
	case "month":
		since := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		filter.Since = &since
	default:
		fields["range"] = append(fields["range"], "Must be one of: all, 24h, 7d, month.")
	}

	switch query.Has {
	case "":
	case "chapter":
		filter.HasChapter = true
	case "none":
		filter.TextOnly = true
	default:
		fields["has"] = append(fields["has"], "Must be one of: chapter, none.")
	}

	switch query.Sort {
	case "", "new":
	case "top":
		filter.SortTop = true
	default:
		fields["sort"] = append(fields["sort"], "Must be one of: new, top.")
	}

	if query.Type != "" {
		if !ValidPostType(query.Type) {
			fields["type"] = append(fields["type"],
				"Must be one of: "+strings.Join(PostTypeList(), ", ")+".")
		}
		filter.Type = query.Type
	}

	filter.Mention = strings.TrimPrefix(strings.TrimSpace(query.Mention), "@")
	filter.Fandom = strings.TrimSpace(query.Fandom)

	if len(fields) > 0 {
		return nil, pagination.Meta{}, apierror.Validation(fields)
	}

	// A search needs a needle. Range and sort alone reorder the feed, which
	// the feed endpoint already serves - and serving it here too would put
	// the whole firehose on the search tier.
	if filter.Query == "" && filter.Tag == "" && filter.Mention == "" &&
		filter.Fandom == "" && filter.Type == "" && !filter.HasChapter &&
		!filter.TextOnly && strings.TrimSpace(query.Author) == "" &&
		filter.AuthorID == uuid.Nil {
		return nil, pagination.Meta{}, apierror.Validation(map[string][]string{
			"q": {"Search needs a query or at least one filter."},
		})
	}

	if author := strings.TrimPrefix(strings.TrimSpace(query.Author), "@"); author != "" {
		user, err := s.users.FindByUsername(ctx, author)
		if errors.Is(err, users.ErrNotFound) {
			// Unknown names are an empty page, never an oracle.
			return []PostView{}, page.MetaFor(0), nil
		}
		if err != nil {
			return nil, pagination.Meta{}, s.internal("resolve author", err)
		}
		if filter.AuthorID != uuid.Nil && filter.AuthorID != user.ID {
			// from:me plus somebody else's from:@handle matches nothing.
			return []PostView{}, page.MetaFor(0), nil
		}
		filter.AuthorID = user.ID
		if identity.Authenticated() && user.ID == identity.UserID() {
			// Searching yourself by handle is searching your own posts.
			filter.PublicOnly = false
		}
	}

	viewer := viewerID(identity)
	posts, total, err := s.repo.ListPosts(ctx, viewer, filter, page)
	if err != nil {
		return nil, pagination.Meta{}, s.internal("search posts", err)
	}

	views := make([]PostView, 0, len(posts))
	for i := range posts {
		views = append(views, posts[i].Render(viewer))
	}
	return views, page.MetaFor(total), nil
}

// ---------------------------------------------------------------------------
// Bookmarks (docs/COMMUNITY-FEED.md)
// ---------------------------------------------------------------------------

// BookmarkPost saves a post the caller can open. Idempotent: saving twice is
// the same bookmark.
func (s *Service) BookmarkPost(
	ctx context.Context, identity *auth.Identity, postID uuid.UUID,
) (*BookmarkView, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return nil, err
	}

	if _, err := s.repo.FindVisiblePost(ctx, userID, postID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, postNotFound()
		}
		return nil, s.internal("load post", err)
	}

	if err := s.repo.UpsertBookmark(ctx, postID, userID); err != nil {
		return nil, s.internal("bookmark post", err)
	}
	return &BookmarkView{PostID: postID, Bookmarked: true}, nil
}

// UnbookmarkPost removes the caller's bookmark. Idempotent, and never gated
// on the post still being visible - taking a bookmark back must always work.
func (s *Service) UnbookmarkPost(
	ctx context.Context, identity *auth.Identity, postID uuid.UUID,
) (*BookmarkView, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return nil, err
	}
	if err := s.repo.DeleteBookmark(ctx, postID, userID); err != nil {
		return nil, s.internal("unbookmark post", err)
	}
	return &BookmarkView{PostID: postID, Bookmarked: false}, nil
}

// ---------------------------------------------------------------------------
// Comments (docs/01 §20.1; endpoints recorded in docs/09 §21's update block)
// ---------------------------------------------------------------------------

// ListComments returns a post's top-level thread. Being able to OPEN the post
// is the gate for reading its discussion - guests read public posts' threads
// (docs/03 §27), and a followers-only post's thread stays inside its audience.
func (s *Service) ListComments(
	ctx context.Context, identity *auth.Identity, postID uuid.UUID, page pagination.Params,
) ([]CommentView, pagination.Meta, error) {
	viewer := viewerID(identity)
	if _, err := s.repo.FindVisiblePost(ctx, viewer, postID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, pagination.Meta{}, postNotFound()
		}
		return nil, pagination.Meta{}, s.internal("load post", err)
	}

	comments, total, err := s.repo.ListComments(ctx, CommentFilter{PostID: postID}, page)
	if err != nil {
		return nil, pagination.Meta{}, s.internal("list comments", err)
	}
	return renderComments(comments, viewer), page.MetaFor(total), nil
}

// ListReplies returns one comment's replies, oldest first.
func (s *Service) ListReplies(
	ctx context.Context, identity *auth.Identity, commentID uuid.UUID, page pagination.Params,
) ([]CommentView, pagination.Meta, error) {
	parent, err := s.visibleComment(ctx, identity, commentID)
	if err != nil {
		return nil, pagination.Meta{}, err
	}

	comments, total, err := s.repo.ListComments(ctx, CommentFilter{
		PostID: parent.PostID, ParentID: &parent.ID,
	}, page)
	if err != nil {
		return nil, pagination.Meta{}, s.internal("list replies", err)
	}
	return renderComments(comments, viewerID(identity)), page.MetaFor(total), nil
}

// CreateComment posts a comment on a post the caller can open.
func (s *Service) CreateComment(
	ctx context.Context, identity *auth.Identity, postID uuid.UUID, rawContent string,
) (*CommentView, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return nil, err
	}
	content, err := validateCommentContent(rawContent)
	if err != nil {
		return nil, err
	}

	if _, err := s.repo.FindVisiblePost(ctx, userID, postID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, postNotFound()
		}
		return nil, s.internal("load post", err)
	}

	return s.createComment(ctx, postID, userID, nil, content)
}

// Reply posts a reply under a top-level comment. Threading is single-level,
// exactly like fiction threads: a reply to a reply is a clear 422, never a
// silent re-attach, and the schema's parent_id allows deepening later.
func (s *Service) Reply(
	ctx context.Context, identity *auth.Identity, parentID uuid.UUID, rawContent string,
) (*CommentView, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return nil, err
	}
	content, err := validateCommentContent(rawContent)
	if err != nil {
		return nil, err
	}

	parent, err := s.visibleComment(ctx, identity, parentID)
	if err != nil {
		return nil, err
	}
	if parent.ParentID != nil {
		return nil, apierror.Validation(map[string][]string{
			"parent_id": {"Replies cannot be nested; reply to the top-level comment instead."},
		})
	}

	return s.createComment(ctx, parent.PostID, userID, &parent.ID, content)
}

func (s *Service) createComment(
	ctx context.Context, postID, authorID uuid.UUID, parentID *uuid.UUID, content string,
) (*CommentView, error) {
	comment, err := s.repo.CreateComment(ctx, postID, authorID, parentID, content)
	if err != nil {
		return nil, s.internal("create comment", err)
	}

	if s.notifier != nil {
		s.notifier.CommunityCommentCreated(ctx, authorID, comment.ID)
	}

	view := comment.Render(authorID)
	return &view, nil
}

// UpdateComment replaces the caller's own comment text.
func (s *Service) UpdateComment(
	ctx context.Context, identity *auth.Identity, commentID uuid.UUID, rawContent string,
) (*CommentView, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return nil, err
	}
	content, err := validateCommentContent(rawContent)
	if err != nil {
		return nil, err
	}

	comment, err := s.visibleComment(ctx, identity, commentID)
	if err != nil {
		return nil, err
	}
	if !canModerate(identity, comment.AuthorID) {
		return nil, apierror.Forbidden("Only the comment's author may edit it.")
	}

	updated, err := s.repo.UpdateCommentContent(ctx, comment.ID, content)
	if errors.Is(err, ErrNotFound) {
		return nil, commentNotFound()
	}
	if err != nil {
		return nil, s.internal("update comment", err)
	}

	view := updated.Render(userID)
	return &view, nil
}

// DeleteComment soft-deletes the caller's own comment. Idempotent.
func (s *Service) DeleteComment(
	ctx context.Context, identity *auth.Identity, commentID uuid.UUID,
) error {
	if _, err := requireUser(identity); err != nil {
		return err
	}

	comment, err := s.repo.FindComment(ctx, commentID)
	if errors.Is(err, ErrNotFound) {
		return commentNotFound()
	}
	if err != nil {
		return s.internal("load comment", err)
	}
	if comment.DeletedAt != nil {
		return nil
	}
	if !canModerate(identity, comment.AuthorID) {
		return apierror.Forbidden("Only the comment's author may delete it.")
	}

	if err := s.repo.SoftDeleteComment(ctx, comment.ID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return s.internal("delete comment", err)
	}
	return nil
}

// visibleComment loads a comment for reading through it: the comment must be
// visible AND its post must still be open to the viewer. Everything else is
// the same 404 (docs/11 §3.4).
func (s *Service) visibleComment(
	ctx context.Context, identity *auth.Identity, commentID uuid.UUID,
) (*Comment, error) {
	comment, err := s.repo.FindComment(ctx, commentID)
	if errors.Is(err, ErrNotFound) {
		return nil, commentNotFound()
	}
	if err != nil {
		return nil, s.internal("load comment", err)
	}
	if comment.DeletedAt != nil || comment.Status != CommentStatusVisible {
		return nil, commentNotFound()
	}
	if _, err := s.repo.FindVisiblePost(ctx, viewerID(identity), comment.PostID); err != nil {
		return nil, commentNotFound()
	}
	return comment, nil
}

func renderComments(items []Comment, viewer uuid.UUID) []CommentView {
	views := make([]CommentView, 0, len(items))
	for i := range items {
		views = append(views, items[i].Render(viewer))
	}
	return views
}

// ---------------------------------------------------------------------------
// Reactions (docs/09 §21, docs/08 §21.3, docs/01 §20.2)
// ---------------------------------------------------------------------------

// React records the caller's reaction to a post they can open. One reaction
// per (post, user) - reacting again replaces, never accumulates (docs/09 §34).
func (s *Service) React(
	ctx context.Context, identity *auth.Identity, postID uuid.UUID, reactionType string,
) (*ReactionView, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return nil, err
	}
	if !ValidReactionType(reactionType) {
		return nil, apierror.Validation(map[string][]string{
			"type": {"Must be one of: " + strings.Join(ReactionTypeList(), ", ") + "."},
		})
	}

	if _, err := s.repo.FindVisiblePost(ctx, userID, postID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, postNotFound()
		}
		return nil, s.internal("load post", err)
	}

	inserted, err := s.repo.UpsertReaction(ctx, postID, userID, reactionType)
	if err != nil {
		return nil, s.internal("react", err)
	}

	// Only a NEW reaction emits; the worker dedupes as the second guard, so
	// react → unreact → react cycles can never spam the author
	// (docs/01 §20.2 "should not encourage spam").
	if inserted && s.notifier != nil {
		s.notifier.CommunityReactionAdded(ctx, userID, postID)
	}

	return s.reactionView(ctx, postID, userID)
}

// Unreact removes the caller's reaction. Idempotent, and never gated on the
// post still being visible - taking a reaction back must always work.
func (s *Service) Unreact(
	ctx context.Context, identity *auth.Identity, postID uuid.UUID,
) (*ReactionView, error) {
	userID, err := requireUser(identity)
	if err != nil {
		return nil, err
	}
	if err := s.repo.DeleteReaction(ctx, postID, userID); err != nil {
		return nil, s.internal("unreact", err)
	}
	return s.reactionView(ctx, postID, userID)
}

func (s *Service) reactionView(
	ctx context.Context, postID, userID uuid.UUID,
) (*ReactionView, error) {
	myReaction, total, err := s.repo.ReactionState(ctx, postID, userID)
	if err != nil {
		return nil, s.internal("reaction state", err)
	}
	return &ReactionView{PostID: postID, MyReaction: myReaction, ReactionCount: total}, nil
}

// ---------------------------------------------------------------------------
// Moderation (docs/08 §24, Phase 8)
// ---------------------------------------------------------------------------

// VisiblePostForViewer answers (by error) whether the caller can open this
// post - the report-target check (docs/08 §24.1), with the same 404
// non-oracle semantics as GetPost (docs/11 §37).
func (s *Service) VisiblePostForViewer(
	ctx context.Context, identity *auth.Identity, postID uuid.UUID,
) error {
	if _, err := s.repo.FindVisiblePost(ctx, viewerID(identity), postID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return postNotFound()
		}
		return s.internal("load post", err)
	}
	return nil
}

// VisibleCommentForViewer is the community-comment counterpart.
func (s *Service) VisibleCommentForViewer(
	ctx context.Context, identity *auth.Identity, commentID uuid.UUID,
) error {
	_, err := s.visibleComment(ctx, identity, commentID)
	return err
}

// ModeratePost sets the platform's moderation axis on a post (docs/08 §21.1)
// and returns its author - the moderation notification's recipient.
// Staff-only. Visibility and deleted_at are the AUTHOR's axes and are never
// touched (docs/11 §37's three independent axes).
func (s *Service) ModeratePost(
	ctx context.Context, identity *auth.Identity, postID uuid.UUID, status PostStatus,
) (uuid.UUID, error) {
	if !identity.IsStaff() {
		return uuid.Nil, apierror.Forbidden("You do not have permission to do that.")
	}
	switch status {
	case PostStatusPublished, PostStatusHidden, PostStatusRemoved:
	default:
		return uuid.Nil, apierror.Validation(map[string][]string{
			"status": {"Unknown post status."},
		})
	}

	post, err := s.repo.FindPost(ctx, postID)
	if errors.Is(err, ErrNotFound) {
		return uuid.Nil, postNotFound()
	}
	if err != nil {
		return uuid.Nil, s.internal("load post", err)
	}
	if post.DeletedAt != nil {
		// Author-deleted: nothing left to moderate.
		return uuid.Nil, postNotFound()
	}
	if post.Status == status {
		return uuid.Nil, apierror.Conflict("The post is already in that state.")
	}

	if err := s.repo.SetPostStatus(ctx, post.ID, status); err != nil {
		return uuid.Nil, s.internal("moderate post", err)
	}

	s.log.Info("community post moderated",
		slog.String("post_id", post.ID.String()),
		slog.String("actor_id", identity.UserID().String()),
		slog.String("from", string(post.Status)),
		slog.String("to", string(status)),
	)
	return post.AuthorID, nil
}

// ModerateComment is the community-comment counterpart of ModeratePost.
func (s *Service) ModerateComment(
	ctx context.Context, identity *auth.Identity, commentID uuid.UUID, status CommentStatus,
) (uuid.UUID, error) {
	if !identity.IsStaff() {
		return uuid.Nil, apierror.Forbidden("You do not have permission to do that.")
	}
	switch status {
	case CommentStatusVisible, CommentStatusHidden, CommentStatusRemoved:
	default:
		return uuid.Nil, apierror.Validation(map[string][]string{
			"status": {"Unknown comment status."},
		})
	}

	comment, err := s.repo.FindComment(ctx, commentID)
	if errors.Is(err, ErrNotFound) {
		return uuid.Nil, commentNotFound()
	}
	if err != nil {
		return uuid.Nil, s.internal("load comment", err)
	}
	if comment.DeletedAt != nil {
		return uuid.Nil, commentNotFound()
	}
	if comment.Status == status {
		return uuid.Nil, apierror.Conflict("The comment is already in that state.")
	}

	if err := s.repo.SetCommentStatus(ctx, comment.ID, status); err != nil {
		return uuid.Nil, s.internal("moderate comment", err)
	}

	s.log.Info("community comment moderated",
		slog.String("comment_id", comment.ID.String()),
		slog.String("actor_id", identity.UserID().String()),
		slog.String("from", string(comment.Status)),
		slog.String("to", string(status)),
	)
	return comment.AuthorID, nil
}

// The real implementations, asserted here so a signature change in the fiction
// domain breaks at compile time rather than at wiring time.
var (
	_ NovelAccess   = (*novels.Service)(nil)
	_ ChapterAccess = (*chapters.Service)(nil)
)

// internal logs the real failure and returns the opaque error (docs/11 §67).
func (s *Service) internal(op string, err error) error {
	s.log.Error("community service failure", slog.String("op", op), slog.Any("error", err))
	return apierror.Internal()
}
