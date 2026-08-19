package integration

import (
	"net/http"
	"testing"
)

// Helpers shared by the novels and chapters integration tests.
//
// They drive the real HTTP API rather than calling services directly, so every
// assertion below also exercises routing, middleware, CSRF, and the response
// envelope - the layers where an authorization gap would actually appear.

// novelBody is the decoded shape of a fiction resource.
type novelBody struct {
	ID                    string  `json:"id"`
	Slug                  string  `json:"slug"`
	Title                 string  `json:"title"`
	Description           *string `json:"description"`
	CoverURL              *string `json:"cover_url"`
	ContentWarning        *string `json:"content_warning"`
	StoryStructure        string  `json:"story_structure"`
	PresentationFormat    string  `json:"presentation_format"`
	ContentMode           string  `json:"content_mode"`
	HasMixedFormats       bool    `json:"has_mixed_formats"`
	Status                string  `json:"status"`
	Visibility            *string `json:"visibility"`
	ChapterCount          int     `json:"chapter_count"`
	DraftChapterCount     *int    `json:"draft_chapter_count"`
	UsesChapterNavigation bool    `json:"uses_chapter_navigation"`
	IsOwner               bool    `json:"is_owner"`
	CanEdit               bool    `json:"can_edit"`
	CountsHidden          bool    `json:"counts_hidden"`
	ViewCount             int64   `json:"view_count"`
	LikeCount             int     `json:"like_count"`
	BookmarkCount         int     `json:"bookmark_count"`
	PublishAt             *string `json:"publish_at"`
	PublishedAt           *string `json:"published_at"`
	Author                struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"author"`
}

type messageBody struct {
	ID          string `json:"id"`
	Position    int    `json:"position"`
	SpeakerName string `json:"speaker_name"`
	MessageType string `json:"message_type"`
	Content     string `json:"content"`
	Metadata    *struct {
		Side *string `json:"side"`
	} `json:"metadata"`
}

// chapterBody is the decoded shape of a chapter resource.
type entryBody struct {
	ID          string   `json:"id"`
	Position    int      `json:"position"`
	CharacterID *string  `json:"character_id"`
	Name        string   `json:"name"`
	Values      []string `json:"values"`
	Body        string   `json:"body"`
	ImageURL    *string  `json:"image_url"`
}

type chapterBody struct {
	ID                 string        `json:"id"`
	NovelID            string        `json:"novel_id"`
	ChapterNumber      int           `json:"chapter_number"`
	Title              *string       `json:"title"`
	Slug               string        `json:"slug"`
	Status             string        `json:"status"`
	WordCount          int           `json:"word_count"`
	ContentReady       bool          `json:"content_ready"`
	PresentationFormat *string       `json:"presentation_format"`
	ActiveFormat       string        `json:"active_format"`
	ContentFormat      string        `json:"content_format"`
	ScheduledAt        *string       `json:"scheduled_at"`
	Content            *string       `json:"content"`
	Messages           []messageBody `json:"messages"`
	Entries            []entryBody   `json:"entries"`
	EntryFields        []string      `json:"entry_fields"`
	IsOwner            bool          `json:"is_owner"`
	HasStandardContent *bool         `json:"has_standard_content"`
	HasChatContent     *bool         `json:"has_chat_content"`
	HasEntries         *bool         `json:"has_entries"`
	PreviousChapterID  *string       `json:"previous_chapter_id"`
	NextChapterID      *string       `json:"next_chapter_id"`
}

type formatBody struct {
	ID                 string `json:"id"`
	StoryStructure     string `json:"story_structure"`
	PresentationFormat string `json:"presentation_format"`
	ContentMode        string `json:"content_mode"`
	NeedsChatSetup     bool   `json:"needs_chat_setup"`
}

// dataOf decodes a single-resource envelope.
func dataOf[T any](t *testing.T, res apiResponse) T {
	t.Helper()
	var payload struct {
		Data T `json:"data"`
	}
	res.json(t, &payload)
	return payload.Data
}

// collectionOf decodes a collection envelope with its meta.
func collectionOf[T any](t *testing.T, res apiResponse) ([]T, int64) {
	t.Helper()
	var payload struct {
		Data []T `json:"data"`
		Meta struct {
			Total int64 `json:"total"`
		} `json:"meta"`
	}
	res.json(t, &payload)
	return payload.Data, payload.Meta.Total
}

// errorCodeOf decodes the stable error code clients branch on (docs/09 §7).
func errorCodeOf(t *testing.T, res apiResponse) string {
	t.Helper()
	var payload struct {
		Error struct {
			Code   string              `json:"code"`
			Fields map[string][]string `json:"fields"`
		} `json:"error"`
	}
	res.json(t, &payload)
	return payload.Error.Code
}

// writer is an authenticated browser session with a verified email address.
type writer struct {
	webSession
}

// asOwner sends a request as this writer, with the CSRF token a browser would
// echo on a mutation (docs/11 §22). The body is optional so reads read cleanly.
func (e *authEnv) asOwner(t *testing.T, w writer, method, path string, body ...any) apiResponse {
	t.Helper()

	var payload any
	if len(body) > 0 {
		payload = body[0]
	}
	return e.do(t, apiRequest{
		method:  method,
		path:    path,
		body:    payload,
		cookies: w.authCookies(),
		csrf:    w.csrfToken,
	})
}

// asGuest sends an unauthenticated request - no cookie, no token, nothing.
func (e *authEnv) asGuest(t *testing.T, method, path string, body ...any) apiResponse {
	t.Helper()
	req := apiRequest{method: method, path: path}
	// Variadic so every existing read call site stays a two-argument read. A
	// guest WRITE exists since §13D - a fiction on the "ทุกคน" level accepts a
	// comment from a reader with no account - and it needs a body like any
	// other write.
	if len(body) > 0 {
		req.body = body[0]
	}
	return e.do(t, req)
}

// newWriter registers an account and completes email verification.
//
// Verification runs through the real endpoint using the token captured from the
// outgoing mail, so these tests exercise the same flow a person would - and the
// publishing gate in docs/AUTHENTICATION.md §9 is genuinely satisfied rather
// than bypassed with a direct database write.
func (e *authEnv) newWriter(t *testing.T) writer {
	t.Helper()

	session := e.registerWeb(t)
	token := e.mailer.lastLinkToken(t, "/verify-email")

	res := e.do(t, apiRequest{
		method: http.MethodPost,
		path:   "/api/v1/auth/verify-email",
		body:   map[string]string{"token": token},
	})
	if res.status != http.StatusOK {
		t.Fatalf("verify email status = %d, want 200. body: %s", res.status, res.body)
	}
	return writer{webSession: session}
}

// newUnverifiedWriter registers an account WITHOUT verifying its address, for
// the publishing-gate tests.
func (e *authEnv) newUnverifiedWriter(t *testing.T) writer {
	t.Helper()
	return writer{webSession: e.registerWeb(t)}
}

// createNovelBody is a create request with sensible defaults for a test.
//
// age_rating is filled in because the API REQUIRES it (Phase 13A): every
// fiction states one, so every fixture states one. A test that wants to prove
// the requirement itself sends its own body without it.
func createNovelBody(title string, overrides map[string]any) map[string]any {
	body := map[string]any{"title": title, "age_rating": "general"}
	for key, value := range overrides {
		body[key] = value
	}
	return body
}

// createNovel creates a fiction and fails the test if it does not succeed.
func (e *authEnv) createNovel(t *testing.T, w writer, body map[string]any) novelBody {
	t.Helper()

	res := e.asOwner(t, w, http.MethodPost, "/api/v1/novels", body)
	if res.status != http.StatusCreated {
		t.Fatalf("create novel status = %d, want 201. body: %s", res.status, res.body)
	}
	return dataOf[novelBody](t, res)
}

// completeChecklist fills in everything the pre-publish gate requires (§13L).
//
// A helper rather than a wider fixture, because the gate is real: a test that
// publishes has to satisfy it exactly as a writer would, and one that only
// drafts should stay free of the extra fields.
func (e *authEnv) completeChecklist(t *testing.T, w writer, novelID string) {
	t.Helper()

	genres := e.seededGenres(t)
	tag := e.newTag(t, w, uniqueName(t, "พร้อม "))

	res := e.asOwner(t, w, http.MethodPatch, "/api/v1/novels/"+novelID, map[string]any{
		"description": "เรื่องย่อสำหรับการทดสอบการเผยแพร่",
		"cover_url":   "https://example.com/cover.jpg",
		// Only 15+/18+ work needs one, but sending it always keeps this helper
		// usable for every rating without the caller having to know which.
		"content_warning": "มีฉากรุนแรงเล็กน้อย",
		"genre_ids":       []string{genres["fantasy"].ID},
		"tag_ids":         []string{tag.ID},
	})
	if res.status != http.StatusOK {
		t.Fatalf("complete checklist status = %d. body: %s", res.status, res.body)
	}

	// The account's one-time adult statement (§13B). Harmless for a writer of
	// general-rated work and required for anyone publishing 18+, so the fixture
	// makes it once rather than making every 18+ test remember it.
	if res := e.asOwner(t, w, http.MethodPost,
		"/api/v1/auth/adult-attestation", nil); res.status != http.StatusOK {
		t.Fatalf("adult attestation status = %d. body: %s", res.status, res.body)
	}
}

// publishedNovel creates a fiction that is publicly readable.
//
// It goes the way a WRITER goes since §13L: create a private draft, fill in
// what the pre-publish checklist asks for, then publish. Creating it published
// in one call is refused by the API - the gate applies to a create that exposes
// the work exactly as it applies to the PATCH that would have - so a fixture
// that took the shortcut would be testing against a rule the product does not
// have.
func (e *authEnv) publishedNovel(t *testing.T, w writer, overrides map[string]any) novelBody {
	t.Helper()

	body := createNovelBody(uniqueName(t, "Fiction "), overrides)

	status, visibility := "ongoing", "public"
	if value, ok := body["status"].(string); ok {
		status = value
	}
	if value, ok := body["visibility"].(string); ok {
		visibility = value
	}
	delete(body, "status")
	delete(body, "visibility")

	// Fill in whatever the checklist asks for that this caller did not supply.
	// The caller's own values always win: a discovery test that assigns one
	// genre must not silently acquire a second one from a fixture.
	fill := func(key string, value any) {
		if _, set := body[key]; !set {
			body[key] = value
		}
	}
	fill("description", "เรื่องย่อสำหรับการทดสอบการเผยแพร่")
	fill("cover_url", "https://example.com/cover.jpg")
	fill("content_warning", "มีฉากรุนแรงเล็กน้อย")
	if _, set := body["genre_ids"]; !set {
		body["genre_ids"] = []string{e.seededGenres(t)["fantasy"].ID}
	}
	if _, set := body["tag_ids"]; !set {
		body["tag_ids"] = []string{e.newTag(t, w, uniqueName(t, "พร้อม ")).ID}
	}

	novel := e.createNovel(t, w, body)

	// The account's one-time adult statement (§13B): harmless for general work,
	// required for 18+, so the fixture makes it once rather than making every
	// 18+ test remember it.
	if res := e.asOwner(t, w, http.MethodPost,
		"/api/v1/auth/adult-attestation", nil); res.status != http.StatusOK {
		t.Fatalf("adult attestation status = %d. body: %s", res.status, res.body)
	}

	res := e.asOwner(t, w, http.MethodPatch, "/api/v1/novels/"+novel.ID, map[string]any{
		"status": status, "visibility": visibility,
	})
	if res.status != http.StatusOK {
		t.Fatalf("publish novel status = %d, want 200. body: %s", res.status, res.body)
	}
	return dataOf[novelBody](t, res)
}

// createChapter adds a chapter and fails the test if it does not succeed.
func (e *authEnv) createChapter(
	t *testing.T, w writer, novelID string, body map[string]any,
) chapterBody {
	t.Helper()

	res := e.asOwner(t, w, http.MethodPost, "/api/v1/novels/"+novelID+"/chapters", body)
	if res.status != http.StatusCreated {
		t.Fatalf("create chapter status = %d, want 201. body: %s", res.status, res.body)
	}
	return dataOf[chapterBody](t, res)
}

// publishChapter takes a chapter live through the real publish endpoint.
func (e *authEnv) publishChapter(t *testing.T, w writer, novelID, chapterID string) chapterBody {
	t.Helper()

	res := e.asOwner(t, w, http.MethodPost,
		"/api/v1/novels/"+novelID+"/chapters/"+chapterID+"/publish", nil)
	if res.status != http.StatusOK {
		t.Fatalf("publish status = %d, want 200. body: %s", res.status, res.body)
	}
	return dataOf[chapterBody](t, res)
}

// headcanonEntries builds a small topic for the headcanon-format tests (12F).
func headcanonEntries() []map[string]any {
	return []map[string]any{
		{"name": "อลิซ", "values": []string{"20", "ราศีเมษ"}, "body": "ตื่นเช้าเสมอ แม้ไม่มีอะไรต้องทำ"},
		{"name": "บ็อบ", "values": []string{"22", "ราศีกรกฎ"}, "body": "เก็บตั๋วหนังทุกใบไว้ในกล่อง"},
	}
}

// chatMessages builds a small conversation for the chat-format tests.
func chatMessages() []map[string]any {
	return []map[string]any{
		{
			"speaker_name": "Alice",
			"message_type": "message",
			"content":      "นายอยู่ไหน?",
			"metadata":     map[string]any{"side": "left"},
		},
		{
			"speaker_name": "Bob",
			"message_type": "message",
			"content":      "กำลังกลับ",
			"metadata":     map[string]any{"side": "right"},
		},
		{"message_type": "separator", "content": ""},
		{"speaker_name": "Alice", "message_type": "message", "content": "โอเค เดี๋ยวรอนะ"},
	}
}
