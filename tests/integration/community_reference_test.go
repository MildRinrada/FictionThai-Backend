package integration

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Phase 12D - a community post that points at a fiction
// (docs/PHASE-12-STORY-DEPTH.md §12D).
//
// The rule the whole feature turns on: the reference is resolved against the
// READER on every read, never copied onto the post. These tests exist to prove
// the two halves of that - the card appears for people who may open the work,
// and a fiction that goes private stops rendering without taking anyone's post
// down with it.

// attachedPost creates a post that references a fiction, optionally a chapter.
func (e *authEnv) attachedPost(
	t *testing.T, w writer, content, novelID, chapterID string,
) postBody {
	t.Helper()

	reference := map[string]any{"novel_id": novelID}
	if chapterID != "" {
		reference["chapter_id"] = chapterID
	}
	res := e.asOwner(t, w, http.MethodPost, "/api/v1/community/posts", map[string]any{
		"content":   content,
		"reference": reference,
	})
	if res.status != http.StatusCreated {
		t.Fatalf("create attached post status = %d, want 201. body: %s", res.status, res.body)
	}
	return dataOf[postBody](t, res)
}

func TestCommunity_PostReference_RendersForEveryReader(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	author := env.newWriter(t)
	novel := env.publishedNovel(t, author, nil)
	chapter := env.createChapter(t, author, novel.ID, map[string]any{
		"title":   "น้ำขึ้นตอนตีสาม",
		"content": strings.Repeat("ฝนตกทั้งคืน ", 40),
	})
	env.publishChapter(t, author, novel.ID, chapter.ID)

	post := env.attachedPost(t, author, "ตอนที่ 7 ปล่อยแล้วนะคะ", novel.ID, chapter.ID)
	if post.Reference == nil {
		t.Fatal("the author who attached the chapter cannot see their own card")
	}
	if post.Reference.ChapterID == nil || *post.Reference.ChapterID != chapter.ID {
		t.Fatalf("chapter reference wrong: %+v", post.Reference)
	}
	if post.Reference.NovelTitle != novel.Title {
		t.Fatalf("novel title = %q, want %q", post.Reference.NovelTitle, novel.Title)
	}
	// The card must carry what it needs to LINK, not only to label.
	if post.Reference.NovelSlug == "" || post.Reference.ChapterSlug == nil {
		t.Fatalf("reference cannot be linked: %+v", post.Reference)
	}
	if post.Reference.WordCount == nil || *post.Reference.WordCount == 0 {
		t.Fatalf("word count missing: %+v", post.Reference)
	}

	// A guest - the widest possible audience - sees the same card.
	res := env.asGuest(t, http.MethodGet, "/api/v1/community/posts/"+post.ID)
	guestView := dataOf[postBody](t, res)
	if guestView.Reference == nil || guestView.Reference.NovelTitle != novel.Title {
		t.Fatalf("guest lost the reference card: %+v", guestView.Reference)
	}
}

// The documented obligation: a post about a fiction the reader may not see
// renders WITHOUT a reference, and does not leak the title (§12D).
func TestCommunity_PostReference_HiddenWhenFictionGoesPrivate(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	author := env.newWriter(t)
	novel := env.publishedNovel(t, author, nil)
	chapter := env.createChapter(t, author, novel.ID, map[string]any{
		"title":   "ตอนที่หายไป",
		"content": "เนื้อเรื่อง",
	})
	env.publishChapter(t, author, novel.ID, chapter.ID)

	reader := env.newUnverifiedWriter(t)
	post := env.attachedPost(t, reader, "อ่านเรื่องนี้อยู่ สนุกมาก", novel.ID, chapter.ID)
	if post.Reference == nil {
		t.Fatal("a reader could not attach a public fiction")
	}

	// The writer takes the fiction private AFTER the post exists.
	res := env.asOwner(t, author, http.MethodPatch, "/api/v1/novels/"+novel.ID,
		map[string]any{"visibility": "private"})
	if res.status != http.StatusOK {
		t.Fatalf("make private status = %d, want 200. body: %s", res.status, res.body)
	}

	// The post SURVIVES: deleting someone else's writing is never the answer.
	res = env.asGuest(t, http.MethodGet, "/api/v1/community/posts/"+post.ID)
	if res.status != http.StatusOK {
		t.Fatalf("post disappeared with the fiction: status = %d", res.status)
	}
	view := dataOf[postBody](t, res)
	if view.Reference != nil {
		t.Fatalf("private fiction leaked through a post: %+v", view.Reference)
	}
	// Nothing about the fiction may appear anywhere in the payload.
	if strings.Contains(string(res.body), novel.Title) || strings.Contains(string(res.body), novel.ID) {
		t.Fatalf("the response leaks the private fiction: %s", res.body)
	}

	// The post's own author is not privileged here either: the rule is about
	// the reader of the post, not the person who attached it.
	res = env.asOwner(t, reader, http.MethodGet, "/api/v1/community/posts/"+post.ID)
	if dataOf[postBody](t, res).Reference != nil {
		t.Fatal("the post's author still sees a fiction they can no longer open")
	}

	// The fiction's OWNER still sees their own work in the card.
	res = env.asOwner(t, author, http.MethodGet, "/api/v1/community/posts/"+post.ID)
	if dataOf[postBody](t, res).Reference == nil {
		t.Fatal("the fiction's own author lost the card for their own work")
	}
}

// Attaching is gated on being able to READ the work, and every refusal reads
// the same so the composer cannot confirm that a private fiction exists.
func TestCommunity_PostReference_CannotAttachUnreadableWork(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	author := env.newWriter(t)
	private := env.createNovel(t, author, createNovelBody(uniqueName(t, "Private "),
		map[string]any{"visibility": "private", "status": "ongoing"}))

	// Verified, so this account can publish a fiction of its own further down;
	// what is being tested is read access to someone else's work, not the
	// publishing gate.
	stranger := env.newWriter(t)

	attach := func(reference map[string]any) apiResponse {
		return env.asOwner(t, stranger, http.MethodPost, "/api/v1/community/posts",
			map[string]any{"content": "ลองแนบดู", "reference": reference})
	}

	hidden := attach(map[string]any{"novel_id": private.ID})
	absent := attach(map[string]any{"novel_id": uuid.NewString()})
	if hidden.status != http.StatusUnprocessableEntity ||
		absent.status != http.StatusUnprocessableEntity {
		t.Fatalf("attach statuses = %d / %d, want 422 / 422", hidden.status, absent.status)
	}
	if string(hidden.body) != string(absent.body) {
		t.Fatalf("a private fiction answers differently from an absent one:\n%s\n%s",
			hidden.body, absent.body)
	}

	// A chapter must belong to the fiction it is attached with.
	other := env.publishedNovel(t, author, nil)
	otherChapter := env.createChapter(t, author, other.ID,
		map[string]any{"content": "เนื้อเรื่อง"})
	env.publishChapter(t, author, other.ID, otherChapter.ID)

	mine := env.publishedNovel(t, stranger, nil)
	crossed := attach(map[string]any{"novel_id": mine.ID, "chapter_id": otherChapter.ID})
	if crossed.status != http.StatusUnprocessableEntity {
		t.Fatalf("a chapter from another fiction was accepted: %s", crossed.body)
	}

	orphan := attach(map[string]any{"chapter_id": otherChapter.ID})
	if orphan.status != http.StatusUnprocessableEntity {
		t.Fatalf("a chapter without its fiction was accepted: %s", orphan.body)
	}
}

// The three-case rule (docs/09 §3): absent leaves the attachment alone, null
// detaches it, an object replaces it.
func TestCommunity_PostReference_PatchSemantics(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	author := env.newWriter(t)
	first := env.publishedNovel(t, author, nil)
	second := env.publishedNovel(t, author, nil)

	post := env.attachedPost(t, author, "โพสต์แรก", first.ID, "")
	path := "/api/v1/community/posts/" + post.ID

	// Absent: editing only the text keeps the card.
	res := env.asOwner(t, author, http.MethodPatch, path, map[string]any{"content": "แก้ข้อความ"})
	edited := dataOf[postBody](t, res)
	if edited.Reference == nil || edited.Reference.NovelID != first.ID {
		t.Fatalf("a text edit dropped the attachment: %+v", edited.Reference)
	}

	// An object replaces it wholesale.
	res = env.asOwner(t, author, http.MethodPatch, path, map[string]any{
		"reference": map[string]any{"novel_id": second.ID},
	})
	moved := dataOf[postBody](t, res)
	if moved.Reference == nil || moved.Reference.NovelID != second.ID {
		t.Fatalf("the reference did not move: %+v", moved.Reference)
	}

	// null detaches, and touches nothing else.
	res = env.asOwner(t, author, http.MethodPatch, path, map[string]any{"reference": nil})
	detached := dataOf[postBody](t, res)
	if detached.Reference != nil {
		t.Fatalf("null did not detach: %+v", detached.Reference)
	}
	if detached.Content != "แก้ข้อความ" {
		t.Fatalf("detaching rewrote the post: %q", detached.Content)
	}
}

// ?feed=attached filters on the COLUMN, so a post whose fiction the reader may
// not open still belongs to the feed - it simply renders without a card.
func TestCommunity_PostReference_AttachedFeed(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	author := env.newWriter(t)
	novel := env.publishedNovel(t, author, nil)

	attached := env.attachedPost(t, author, uniqueName(t, "แนบเรื่อง "), novel.ID, "")
	plain := env.createCommunityPost(t, author, uniqueName(t, "ไม่แนบ "), "public")

	res := env.asGuest(t, http.MethodGet, "/api/v1/community/posts?feed=attached&per_page=100")
	items, _ := collectionOf[postBody](t, res)

	var sawAttached, sawPlain bool
	for _, item := range items {
		switch item.ID {
		case attached.ID:
			sawAttached = true
		case plain.ID:
			sawPlain = true
		}
	}
	if !sawAttached {
		t.Fatal("?feed=attached dropped a post that carries a reference")
	}
	if sawPlain {
		t.Fatal("?feed=attached returned a post with no reference")
	}

	// An unknown feed is still refused rather than silently ignored.
	res = env.asGuest(t, http.MethodGet, "/api/v1/community/posts?feed=nonsense")
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("unknown feed status = %d, want 422", res.status)
	}
}

// The sidebar counts public posts about publicly readable fictions only.
func TestCommunity_DiscussedFictions(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)

	author := env.newWriter(t)
	open := env.publishedNovel(t, author, nil)
	hidden := env.createNovel(t, author, createNovelBody(uniqueName(t, "Hidden "),
		map[string]any{"visibility": "private", "status": "ongoing"}))

	// The panel is a ranked top-N over a database this suite shares, so the
	// fiction under test is given a count nothing else in the suite produces
	// rather than being asserted into a crowded list.
	const talkedAbout = 3
	for i := 0; i < talkedAbout; i++ {
		env.attachedPost(t, author, uniqueName(t, "คุยเรื่องนี้กัน "), open.ID, "")
	}
	env.attachedPost(t, author, "บันทึกส่วนตัวถึงเรื่องนี้", hidden.ID, "")

	res := env.asGuest(t, http.MethodGet, "/api/v1/community/discussed")
	if res.status != http.StatusOK {
		t.Fatalf("discussed status = %d, want 200. body: %s", res.status, res.body)
	}
	if strings.Contains(string(res.body), hidden.Title) || strings.Contains(string(res.body), hidden.ID) {
		t.Fatalf("the sidebar leaks a private fiction: %s", res.body)
	}

	items := dataOf[[]struct {
		Fiction   referenceBody `json:"fiction"`
		PostCount int64         `json:"post_count"`
	}](t, res)

	var found bool
	for _, item := range items {
		if item.Fiction.NovelID != open.ID {
			continue
		}
		found = true
		if item.PostCount != talkedAbout {
			t.Fatalf("post count = %d, want %d", item.PostCount, talkedAbout)
		}
		// A fiction-level row carries no chapter, even though the query shares
		// its column list with the post card.
		if item.Fiction.ChapterID != nil {
			t.Fatalf("the sidebar invented a chapter: %+v", item.Fiction)
		}
	}
	if !found {
		// The panel is a global top-N over a SHARED test database, so absence is
		// only a bug when this fiction outranks something that is there. Every
		// row beating it is the endpoint working exactly as specified.
		for _, item := range items {
			if item.PostCount < talkedAbout {
				t.Fatalf("a less-discussed fiction outranked ours (%d < %d): %s",
					item.PostCount, talkedAbout, res.body)
			}
		}
	}
}
