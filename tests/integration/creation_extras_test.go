package integration

import (
	"net/http"
	"testing"
)

// Phase 13K - ตั้งค่าเพิ่มเติม, the collapsed section the create form was always
// specified to have (docs/PHASE-13-CREATION-AND-CONTROL.md §13E, §13K).
//
// The rule that runs through it: a control that cannot be enforced is a
// declaration and must say so. The comment switch CAN be enforced, so it is -
// and the permission flags cannot, so they are stored and shown, never checked.

type extrasBody struct {
	ID              string  `json:"id"`
	Language        string  `json:"language"`
	ChapterUnit     string  `json:"chapter_unit"`
	AuthorNoteStart *string `json:"author_note_start"`
	AuthorNoteEnd   *string `json:"author_note_end"`
	SeriesName      *string `json:"series_name"`
	SeriesPosition  *int    `json:"series_position"`
	CommentAccess   string  `json:"comment_access"`
	CommentApproval bool    `json:"comment_approval"`
	Rights          struct {
		AllowScreenshot  bool    `json:"allow_screenshot"`
		AllowTranslation bool    `json:"allow_translation"`
		AllowDerivative  bool    `json:"allow_derivative"`
		AllowAudio       bool    `json:"allow_audio"`
		RequireCredit    bool    `json:"require_credit"`
		DerivativeTerms  *string `json:"derivative_terms"`
	} `json:"rights"`
}

// A writer who never opens the section gets a complete, valid state.
func TestExtras_DefaultsWithoutOpeningTheSection(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)

	got := dataOf[extrasBody](t, env.asOwner(t, w, http.MethodPost, "/api/v1/novels",
		createNovelBody(uniqueName(t, "Defaults "), nil)))

	if got.Language != "th" {
		t.Errorf("language = %q, want th on a Thai-first platform", got.Language)
	}
	if got.ChapterUnit != "ตอน" {
		t.Errorf("chapter_unit = %q, want ตอน", got.ChapterUnit)
	}
	if got.CommentAccess != "members" {
		t.Errorf("comment_access = %q, want members - a new fiction should not launch silenced", got.CommentAccess)
	}
	if got.CommentApproval {
		t.Error("a new fiction should not hold comments for review by default")
	}
	// Quoting is how a fiction is shared, and credit is the ask that costs
	// nothing; the rest are off until the author says otherwise.
	if !got.Rights.AllowScreenshot || !got.Rights.RequireCredit {
		t.Errorf("permissive defaults not applied: %+v", got.Rights)
	}
	if got.Rights.AllowTranslation || got.Rights.AllowDerivative || got.Rights.AllowAudio {
		t.Errorf("a permission the author never gave was granted: %+v", got.Rights)
	}
}

// The whole section round-trips through create, and an edit that mentions one
// field leaves the other twelve alone.
func TestExtras_RoundTripAndPartialUpdate(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)

	created := dataOf[extrasBody](t, env.asOwner(t, w, http.MethodPost, "/api/v1/novels",
		createNovelBody(uniqueName(t, "Extras "), map[string]any{
			"language":          "en",
			"chapter_unit":      "  EP  ",
			"author_note_start": "  ฝากด้วยนะคะ  ",
			"series_name":       "จักรวาลเดียวกัน",
			"series_position":   2,
			"comment_access":    "off",
			"allow_translation": true,
			"allow_derivative":  true,
			"derivative_terms":  "ทำต่อได้ แต่บอกกันก่อน",
			"require_credit":    false,
		})))

	if created.Language != "en" || created.ChapterUnit != "EP" {
		t.Fatalf("language/unit not stored or not trimmed: %+v", created)
	}
	if created.AuthorNoteStart == nil || *created.AuthorNoteStart != "ฝากด้วยนะคะ" {
		t.Fatalf("author note not trimmed or lost: %+v", created.AuthorNoteStart)
	}
	if created.SeriesName == nil || created.SeriesPosition == nil || *created.SeriesPosition != 2 {
		t.Fatalf("series not stored: %+v", created)
	}
	if created.CommentAccess != "off" {
		t.Errorf("comment_access = %q, want off", created.CommentAccess)
	}
	if !created.Rights.AllowDerivative || created.Rights.DerivativeTerms == nil {
		t.Fatalf("derivative permission and its terms not stored: %+v", created.Rights)
	}

	path := "/api/v1/novels/" + created.ID

	// One field changes; nothing else moves.
	after := dataOf[extrasBody](t, env.asOwner(t, w, http.MethodPatch, path,
		map[string]any{"chapter_unit": "บท"}))
	if after.ChapterUnit != "บท" {
		t.Fatalf("chapter_unit = %q", after.ChapterUnit)
	}
	if after.Language != "en" || after.CommentAccess != "off" || !after.Rights.AllowDerivative {
		t.Fatalf("an unrelated field moved: %+v", after)
	}
	if after.SeriesName == nil || *after.SeriesName != "จักรวาลเดียวกัน" {
		t.Fatalf("the series was lost by an unrelated edit: %+v", after.SeriesName)
	}
}

// Leaving a series clears the position with it: the CHECK requires the pair to
// stay coherent, and a position about no series is a number about nothing.
func TestExtras_LeavingASeriesClearsItsPosition(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)

	novel := env.createNovel(t, w, createNovelBody(uniqueName(t, "Series "), map[string]any{
		"series_name": "ภาคหลัก", "series_position": 3,
	}))
	path := "/api/v1/novels/" + novel.ID

	after := dataOf[extrasBody](t, env.asOwner(t, w, http.MethodPatch, path,
		map[string]any{"series_name": ""}))
	if after.SeriesName != nil || after.SeriesPosition != nil {
		t.Fatalf("leaving the series kept a dangling position: %+v", after)
	}

	// Turning a permission off takes its condition with it - a condition shown
	// to nobody is text the author will never see again.
	env.asOwner(t, w, http.MethodPatch, path, map[string]any{
		"allow_derivative": true, "derivative_terms": "บอกก่อนนะ",
	})
	off := dataOf[extrasBody](t, env.asOwner(t, w, http.MethodPatch, path,
		map[string]any{"allow_derivative": false}))
	if off.Rights.DerivativeTerms != nil {
		t.Errorf("terms survived their permission: %+v", off.Rights.DerivativeTerms)
	}
}

func TestExtras_RejectsUnsupportedValues(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)

	// A language the platform cannot display, search, or moderate is not a
	// language it supports, and offering one would be a claim it cannot keep.
	res := env.asOwner(t, w, http.MethodPost, "/api/v1/novels",
		createNovelBody(uniqueName(t, "Bad lang "), map[string]any{"language": "jp"}))
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("unsupported language status = %d, want 422. body: %s", res.status, res.body)
	}

	res = env.asOwner(t, w, http.MethodPost, "/api/v1/novels",
		createNovelBody(uniqueName(t, "Bad pos "), map[string]any{
			"series_name": "ซีรีส์", "series_position": 0,
		}))
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("series_position 0 status = %d, want 422. body: %s", res.status, res.body)
	}
}

// The comment switch is ENFORCED, not decorative. A switch that turned nothing
// off would be the exact dishonesty this platform refuses.
func TestExtras_ClosedCommentsAreRefused(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	author := env.newWriter(t)
	reader := env.newWriter(t)

	novel := env.publishedNovel(t, author, map[string]any{"comment_access": "off"})
	chapter := env.createChapter(t, author, novel.ID, map[string]any{"content": "ข้อความ"})
	env.publishChapter(t, author, novel.ID, chapter.ID)

	res := env.asOwner(t, reader, http.MethodPost,
		"/api/v1/novels/"+novel.Slug+"/comments", map[string]any{"content": "ขอคอมเมนต์หน่อย"})
	if res.status != http.StatusForbidden {
		t.Fatalf("comment on a closed fiction status = %d, want 403. body: %s", res.status, res.body)
	}
	if code := errorCodeOf(t, res); code != "COMMENTS_CLOSED" {
		t.Errorf("error code = %q, want COMMENTS_CLOSED", code)
	}

	// The chapter thread is closed too, or the switch would only cover half the
	// places a reader can write.
	res = env.asOwner(t, reader, http.MethodPost,
		"/api/v1/novels/"+novel.Slug+"/chapters/"+chapter.Slug+"/comments",
		map[string]any{"content": "ขอคอมเมนต์หน่อย"})
	if res.status != http.StatusForbidden {
		t.Fatalf("chapter comment status = %d, want 403. body: %s", res.status, res.body)
	}

	// Turning it back on reopens both, and no comment was lost in between.
	env.asOwner(t, author, http.MethodPatch, "/api/v1/novels/"+novel.ID,
		map[string]any{"comment_access": "members"})
	res = env.asOwner(t, reader, http.MethodPost,
		"/api/v1/novels/"+novel.Slug+"/comments", map[string]any{"content": "กลับมาแล้ว"})
	if res.status != http.StatusCreated {
		t.Fatalf("comment after reopening status = %d, want 201. body: %s", res.status, res.body)
	}
}

// Permissions are DECLARATIONS. Every reader receives them, because the point
// is that a reader can see what the author allows (§13E).
func TestExtras_RightsAreVisibleToReaders(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)

	novel := env.publishedNovel(t, w, map[string]any{
		"allow_screenshot": false, "allow_audio": true,
	})

	got := dataOf[extrasBody](t, env.asGuest(t, http.MethodGet, "/api/v1/novels/"+novel.Slug))
	if got.Rights.AllowScreenshot || !got.Rights.AllowAudio {
		t.Fatalf("a guest did not receive the author's stated permissions: %+v", got.Rights)
	}
	if got.ChapterUnit == "" {
		t.Error("the chapter unit is presentation a reader needs")
	}
}
