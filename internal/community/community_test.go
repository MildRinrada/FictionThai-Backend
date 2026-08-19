package community

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestValidatePostContent(t *testing.T) {
	t.Run("trims and keeps Thai text intact", func(t *testing.T) {
		got, err := validatePostContent("  อัปเดตงานเขียนสัปดาห์นี้ค่ะ  ")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "อัปเดตงานเขียนสัปดาห์นี้ค่ะ" {
			t.Fatalf("content mangled: %q", got)
		}
	})

	t.Run("rejects empty and whitespace-only", func(t *testing.T) {
		for _, raw := range []string{"", "   ", "\n\t "} {
			if _, err := validatePostContent(raw); err == nil {
				t.Fatalf("expected validation error for %q", raw)
			}
		}
	})

	t.Run("limit counts runes, not bytes", func(t *testing.T) {
		thai := strings.Repeat("ก", MaxPostRunes)
		if _, err := validatePostContent(thai); err != nil {
			t.Fatalf("a %d-rune Thai post must be allowed: %v", MaxPostRunes, err)
		}
		if _, err := validatePostContent(thai + "ก"); err == nil {
			t.Fatal("expected validation error above the rune limit")
		}
	})
}

func TestVisibilityAndReactionAllowlists(t *testing.T) {
	for _, v := range Visibilities() {
		if !ValidVisibility(v) {
			t.Fatalf("allowlisted visibility %q rejected", v)
		}
	}
	for _, v := range []string{"", "PUBLIC", "friends", "unlisted"} {
		if ValidVisibility(v) {
			t.Fatalf("unknown visibility %q accepted", v)
		}
	}

	if !ValidReactionType("like") {
		t.Fatal("documented reaction type 'like' rejected")
	}
	for _, r := range []string{"", "LIKE", "love", "angry", "<script>"} {
		if ValidReactionType(r) {
			t.Fatalf("unlisted reaction %q accepted", r)
		}
	}
}

func TestPostRender(t *testing.T) {
	owner := uuid.New()
	created := time.Now().Add(-time.Hour)
	post := Post{
		ID:        uuid.New(),
		AuthorID:  owner,
		Content:   "โพสต์แรกของฉัน",
		Status:    PostStatusPublished,
		CreatedAt: created,
		UpdatedAt: created,
	}

	if view := post.Render(owner); !view.IsOwner || view.Edited {
		t.Fatalf("owner view wrong: %+v", view)
	}
	if view := post.Render(uuid.New()); view.IsOwner {
		t.Fatal("stranger must not be owner")
	}
	if view := post.Render(uuid.Nil); view.IsOwner {
		t.Fatal("guest must not be owner")
	}

	post.UpdatedAt = created.Add(time.Minute)
	if !post.Render(owner).Edited {
		t.Fatal("edited post must be flagged")
	}
}
