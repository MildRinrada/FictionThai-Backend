package community

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/internal/auth"
	"github.com/fictionthai/fictionthai/backend/internal/users"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
	"github.com/fictionthai/fictionthai/backend/pkg/pagination"
)

// Search validation runs entirely before the repository is touched, so these
// tests exercise the real service with a nil repo: reaching the database
// would be the failure. The database-backed half - what actually matches -
// is proved by the integration suite.

func searchService() *Service {
	return NewService(nil, nil, nil, nil, nil, slog.New(slog.DiscardHandler))
}

func signedIn() *auth.Identity {
	return &auth.Identity{User: &users.User{ID: uuid.New()}}
}

func wantStatus(t *testing.T, err error, status int) *apierror.Error {
	t.Helper()
	var apiErr *apierror.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %v is not an *apierror.Error", err)
	}
	if apiErr.Status != status {
		t.Fatalf("status = %d, want %d (%v)", apiErr.Status, status, apiErr)
	}
	return apiErr
}

func TestSearchPostsValidation(t *testing.T) {
	page := pagination.Params{Page: 1, PerPage: 20}

	t.Run("personal scopes require a signed-in caller", func(t *testing.T) {
		for _, from := range []string{"following", "me"} {
			_, _, err := searchService().SearchPosts(
				context.Background(), nil, SearchQuery{Q: "ฟิค", From: from}, page)
			wantStatus(t, err, 401)
		}
	})

	t.Run("unknown enum values are a 422 naming the field", func(t *testing.T) {
		cases := map[string]SearchQuery{
			"from":  {Q: "ฟิค", From: "everyone"},
			"range": {Q: "ฟิค", Range: "1y"},
			"has":   {Q: "ฟิค", Has: "image"},
			"sort":  {Q: "ฟิค", Sort: "trending"},
			"type":  {Q: "ฟิค", Type: "poll"},
		}
		for field, query := range cases {
			_, _, err := searchService().SearchPosts(context.Background(), nil, query, page)
			apiErr := wantStatus(t, err, 422)
			if _, ok := apiErr.Fields[field]; !ok {
				t.Errorf("%s: fields = %v, want key %q", field, apiErr.Fields, field)
			}
		}
	})

	t.Run("an empty search is a 422, not the firehose", func(t *testing.T) {
		for _, query := range []SearchQuery{
			{},
			{Q: "   "},
			{Range: "7d", Sort: "top"}, // reordering alone is the feed's job
		} {
			_, _, err := searchService().SearchPosts(context.Background(), nil, query, page)
			apiErr := wantStatus(t, err, 422)
			if _, ok := apiErr.Fields["q"]; !ok {
				t.Errorf("fields = %v, want key %q", apiErr.Fields, "q")
			}
		}
	})

	t.Run("an overlong needle is a 422, counted in runes", func(t *testing.T) {
		_, _, err := searchService().SearchPosts(context.Background(), nil,
			SearchQuery{Q: strings.Repeat("ก", MaxSearchRunes+1)}, page)
		wantStatus(t, err, 422)
	})
}

func TestListPostsFeedValidation(t *testing.T) {
	page := pagination.Params{Page: 1, PerPage: 20}

	t.Run("mine and saved require a signed-in caller", func(t *testing.T) {
		for _, feed := range []string{"mine", "saved"} {
			_, _, err := searchService().ListPosts(
				context.Background(), nil, ListQuery{Feed: feed}, page)
			wantStatus(t, err, 401)
		}
	})

	t.Run("an unknown feed is still a 422", func(t *testing.T) {
		_, _, err := searchService().ListPosts(
			context.Background(), signedIn(), ListQuery{Feed: "trending"}, page)
		wantStatus(t, err, 422)
	})

	t.Run("an unknown post type is a 422", func(t *testing.T) {
		_, _, err := searchService().ListPosts(
			context.Background(), nil, ListQuery{Type: "poll"}, page)
		wantStatus(t, err, 422)
	})
}

// The view must carry the new fields under the documented names - the
// frontend types mirror these strings.
func TestPostViewSerializesFeedToolFields(t *testing.T) {
	post := Post{
		ID:         uuid.New(),
		AuthorID:   uuid.New(),
		Content:    "หาเบต้าช่วยอ่านแนวแฟนตาซีค่ะ",
		Visibility: VisibilityPublic,
		Type:       PostTypeBetaRequest,
		Bookmarked: true,
	}

	raw, err := json.Marshal(post.Render(post.AuthorID))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got := string(fields["post_type"]); got != `"beta_request"` {
		t.Errorf("post_type = %s, want %q", got, "beta_request")
	}
	if got := string(fields["bookmarked"]); got != "true" {
		t.Errorf("bookmarked = %s, want true", got)
	}

	// bookmarked is omitted when false - a guest's answer weighs nothing.
	post.Bookmarked = false
	raw, _ = json.Marshal(post.Render(uuid.Nil))
	if strings.Contains(string(raw), "bookmarked") {
		t.Errorf("unbookmarked view still carries the field: %s", raw)
	}
}
