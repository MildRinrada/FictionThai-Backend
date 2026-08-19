package comments

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
)

func TestValidateContent(t *testing.T) {
	t.Run("trims and keeps Thai text intact", func(t *testing.T) {
		got, err := validateContent("  สนุกมากเลยค่ะ รออ่านตอนต่อไปนะคะ  ")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "สนุกมากเลยค่ะ รออ่านตอนต่อไปนะคะ" {
			t.Fatalf("content mangled: %q", got)
		}
	})

	t.Run("rejects empty and whitespace-only", func(t *testing.T) {
		for _, raw := range []string{"", "   ", "\n\t "} {
			if _, err := validateContent(raw); err == nil {
				t.Fatalf("expected validation error for %q", raw)
			}
		}
	})

	t.Run("limit counts runes, not bytes", func(t *testing.T) {
		// MaxContentRunes Thai characters are 3× that in bytes; they must pass.
		thai := strings.Repeat("ก", MaxContentRunes)
		if _, err := validateContent(thai); err != nil {
			t.Fatalf("a %d-rune Thai comment must be allowed: %v", MaxContentRunes, err)
		}
		if _, err := validateContent(thai + "ก"); err == nil {
			t.Fatal("expected validation error above the rune limit")
		}
		var apiErr *apierror.Error
		_, err := validateContent(thai + "ก")
		if !asAPIError(err, &apiErr) || apiErr.Status != 422 {
			t.Fatalf("expected 422, got %v", err)
		}
	})
}

func asAPIError(err error, target **apierror.Error) bool {
	e, ok := err.(*apierror.Error)
	if ok {
		*target = e
	}
	return ok
}

func TestRender(t *testing.T) {
	owner := uuid.New()
	created := time.Now().Add(-time.Hour)

	comment := Comment{
		ID:        uuid.New(),
		UserID:    &owner,
		NovelID:   uuid.New(),
		Content:   "ตอนนี้พีคมาก",
		Status:    StatusVisible,
		CreatedAt: created,
		UpdatedAt: created,
	}

	t.Run("owner and edited flags", func(t *testing.T) {
		view := comment.Render(owner)
		if !view.IsOwner {
			t.Fatal("owner must see is_owner=true")
		}
		if view.Edited {
			t.Fatal("an unedited comment must not be flagged edited")
		}

		view = comment.Render(uuid.New())
		if view.IsOwner {
			t.Fatal("a stranger must see is_owner=false")
		}

		view = comment.Render(uuid.Nil)
		if view.IsOwner {
			t.Fatal("a guest must see is_owner=false")
		}
	})

	t.Run("edit stamps the flag", func(t *testing.T) {
		edited := comment
		edited.UpdatedAt = created.Add(time.Minute)
		if !edited.Render(owner).Edited {
			t.Fatal("an edited comment must be flagged edited")
		}
	})
}

// A guest comment is nobody's: not the poster's, not a stranger's, not the
// fiction author's. Nothing is stored that could prove otherwise, so nothing
// may act as if it could (§13D).
func TestRenderGuestComment(t *testing.T) {
	name := "คนอ่านผ่านมา"
	created := time.Now()

	comment := Comment{
		ID:        uuid.New(),
		GuestName: &name,
		NovelID:   uuid.New(),
		Content:   "ชอบมากค่ะ",
		Status:    StatusPending,
		CreatedAt: created,
		UpdatedAt: created,
	}

	view := comment.Render(uuid.New())
	if view.IsOwner {
		t.Fatal("no account may claim ownership of a guest comment")
	}
	if view.Author != nil {
		t.Fatal("a guest comment must carry no author card")
	}
	if view.GuestName == nil || *view.GuestName != name {
		t.Fatal("the guest's name must survive to the view")
	}
	if !view.Pending {
		t.Fatal("a guest comment is held for review, and must say so")
	}
}
