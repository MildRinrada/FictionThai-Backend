package integration

import (
	"net/http"
	"strings"
	"testing"
)

// Phase 13H - reader variables (docs/PHASE-13-CREATION-AND-CONTROL.md §13H).
//
// The rule the whole slice exists to protect is one line: never substitute at
// save; store tokens and resolve at render. Substituted text could never be
// renamed afterwards, and every reader of a cached chapter would see one
// reader's name. These tests are that rule, checked against a real database.

type variableBody struct {
	ID           string   `json:"id"`
	Position     int      `json:"position"`
	Token        string   `json:"token"`
	Label        string   `json:"label"`
	DefaultValue *string  `json:"default_value"`
	Kind         string   `json:"kind"`
	Tokens       []string `json:"tokens"`
	Options      *struct {
		Values []string `json:"values"`
		Forms  []string `json:"forms"`
		Sets   []struct {
			Label  string   `json:"label"`
			Values []string `json:"values"`
		} `json:"sets"`
	} `json:"options"`
}

type variablesBody struct {
	Variables []variableBody `json:"variables"`
	Usage     struct {
		Undeclared []string `json:"undeclared"`
		Unused     []string `json:"unused"`
	} `json:"usage"`
}

func variablePath(ref string) string { return "/api/v1/novels/" + ref + "/variables" }

// The heart of it: declaring, renaming, and deleting a variable never writes a
// byte of the author's text.
func TestVariables_NeverTouchChapterContent(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)

	const prose = "(y/n) เดินเข้ามาในห้อง แล้ว (y/n) ก็หยุดยืนตรงนั้น"

	novel := env.createNovel(t, w, createNovelBody(uniqueName(t, "Variables "), nil))
	chapter := env.createChapter(t, w, novel.ID, map[string]any{"content": prose})
	chapterPath := "/api/v1/novels/" + novel.ID + "/chapters/" + chapter.ID

	res := env.asOwner(t, w, http.MethodPut, variablePath(novel.ID), map[string]any{
		"variables": []map[string]any{
			{"token": "(y/n)", "label": "ชื่อของคุณ", "default_value": "คุณ", "kind": "text"},
		},
	})
	if res.status != http.StatusOK {
		t.Fatalf("declare status = %d. body: %s", res.status, res.body)
	}

	after := dataOf[chapterBody](t, env.asOwner(t, w, http.MethodGet, chapterPath))
	if after.Content == nil || *after.Content != prose {
		t.Fatalf("declaring a variable rewrote the manuscript: %v", after.Content)
	}

	// Renaming the token must NOT rewrite the text that uses the old one. The
	// writer is told through the usage report instead - rewriting an author's
	// manuscript to follow a settings change is what this platform does not do.
	renamed := dataOf[variablesBody](t, env.asOwner(t, w, http.MethodPut, variablePath(novel.ID),
		map[string]any{"variables": []map[string]any{
			{"token": "(ช/ท)", "label": "ชื่อของคุณ", "kind": "text"},
		}}))

	after = dataOf[chapterBody](t, env.asOwner(t, w, http.MethodGet, chapterPath))
	if after.Content == nil || *after.Content != prose {
		t.Fatalf("renaming a token rewrote the manuscript: %v", after.Content)
	}
	if !contains(renamed.Usage.Undeclared, "(y/n)") {
		t.Errorf("the abandoned token should be reported as undeclared: %+v", renamed.Usage)
	}
	if !contains(renamed.Usage.Unused, "(ช/ท)") {
		t.Errorf("the new token is used nowhere yet: %+v", renamed.Usage)
	}

	// And removing every declaration still leaves the words alone.
	env.asOwner(t, w, http.MethodPut, variablePath(novel.ID),
		map[string]any{"variables": []map[string]any{}})
	after = dataOf[chapterBody](t, env.asOwner(t, w, http.MethodGet, chapterPath))
	if after.Content == nil || *after.Content != prose {
		t.Fatalf("clearing the declarations rewrote the manuscript: %v", after.Content)
	}
}

// A guest reading a reader-insert fiction has to be asked the questions, so the
// declarations are public under the fiction's own gate (docs/10 §2.1).
func TestVariables_ReadableByGuestsUnderTheFictionsGate(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)

	published := env.publishedNovel(t, w, nil)
	env.asOwner(t, w, http.MethodPut, variablePath(published.ID), map[string]any{
		"variables": []map[string]any{
			{"token": "(y/n)", "label": "ชื่อของคุณ", "kind": "text"},
		},
	})

	res := env.asGuest(t, http.MethodGet, variablePath(published.Slug))
	if res.status != http.StatusOK {
		t.Fatalf("guest read status = %d. body: %s", res.status, res.body)
	}
	got := dataOf[variablesBody](t, res)
	if len(got.Variables) != 1 || got.Variables[0].Token != "(y/n)" {
		t.Fatalf("guest did not receive the declarations: %+v", got.Variables)
	}

	// The usage scan is for the writer. A reader gets an empty report rather
	// than a full-text scan of the fiction run on their behalf.
	if len(got.Usage.Undeclared) != 0 || len(got.Usage.Unused) != 0 {
		t.Errorf("a reader must not be served the usage report: %+v", got.Usage)
	}

	// A private fiction's declarations are as private as the fiction.
	private := env.createNovel(t, w, createNovelBody(uniqueName(t, "Private "), nil))
	res = env.asGuest(t, http.MethodGet, variablePath(private.ID))
	if res.status != http.StatusNotFound {
		t.Fatalf("private fiction variables status = %d, want 404", res.status)
	}
}

// Only the owner may declare. A stranger gets the fiction's own 404, not a 403
// that would confirm the fiction exists (docs/11 §3.4).
func TestVariables_OnlyTheOwnerMayDeclare(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	owner := env.newWriter(t)
	stranger := env.newWriter(t)

	novel := env.publishedNovel(t, owner, nil)

	res := env.asOwner(t, stranger, http.MethodPut, variablePath(novel.ID), map[string]any{
		"variables": []map[string]any{{"token": "(y/n)", "label": "ชื่อ", "kind": "text"}},
	})
	if res.status != http.StatusNotFound && res.status != http.StatusForbidden {
		t.Fatalf("stranger write status = %d, want 404 or 403. body: %s", res.status, res.body)
	}

	res = env.asGuest(t, http.MethodPut, variablePath(novel.ID))
	if res.status == http.StatusOK {
		t.Fatal("a guest wrote a fiction's variables")
	}
}

// The whole list is replaced, in order, and each declaration keeps its shape.
func TestVariables_ReplaceKeepsOrderAndKinds(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)

	novel := env.createNovel(t, w, createNovelBody(uniqueName(t, "Kinds "), nil))

	got := dataOf[variablesBody](t, env.asOwner(t, w, http.MethodPut, variablePath(novel.ID),
		map[string]any{"variables": []map[string]any{
			{"token": "(y/n)", "label": "ชื่อของคุณ", "default_value": "คุณ", "kind": "text"},
			{"token": "(e/c)", "label": "สีตา", "kind": "choice",
				"options": map[string]any{"values": []string{"น้ำตาล", "ฟ้า"}}},
			{"token": "(p/n)", "label": "สรรพนาม", "kind": "pronoun",
				"options": map[string]any{
					"forms": []string{"ประธาน", "เจ้าของ"},
					"sets": []map[string]any{
						{"label": "เขา", "values": []string{"เขา", "ของเขา"}},
						{"label": "เธอ", "values": []string{"เธอ", "ของเธอ"}},
					},
				}},
		}}))

	if len(got.Variables) != 3 {
		t.Fatalf("variables = %d, want 3. %+v", len(got.Variables), got.Variables)
	}
	for i, variable := range got.Variables {
		if variable.Position != i {
			t.Fatalf("variable %d has position %d - array order was not used", i, variable.Position)
		}
	}

	// A pronoun produces one token per form, and the API serves them so no
	// client has to rebuild the suffix rule (docs/09 §51).
	pronoun := got.Variables[2]
	if len(pronoun.Tokens) != 2 || pronoun.Tokens[0] != "(p/n)" ||
		pronoun.Tokens[1] != "(p/n.เจ้าของ)" {
		t.Fatalf("pronoun tokens = %v", pronoun.Tokens)
	}

	// Replacing with a shorter list removes the rest; nothing lingers.
	got = dataOf[variablesBody](t, env.asOwner(t, w, http.MethodPut, variablePath(novel.ID),
		map[string]any{"variables": []map[string]any{
			{"token": "(e/c)", "label": "สีตา", "kind": "choice",
				"options": map[string]any{"values": []string{"น้ำตาล"}}},
		}}))
	if len(got.Variables) != 1 || got.Variables[0].Token != "(e/c)" {
		t.Fatalf("replace did not shrink the list: %+v", got.Variables)
	}
	if got.Variables[0].Position != 0 {
		t.Errorf("position was not reassigned: %d", got.Variables[0].Position)
	}
}

// Validation reaches the client as field errors it can attach to a row.
func TestVariables_ValidationIsPerRow(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)

	novel := env.createNovel(t, w, createNovelBody(uniqueName(t, "Invalid "), nil))

	res := env.asOwner(t, w, http.MethodPut, variablePath(novel.ID), map[string]any{
		"variables": []map[string]any{
			{"token": "(y/n)", "label": "ชื่อ", "kind": "text"},
			{"token": "(Y N)", "label": "ช่องว่าง", "kind": "text"},
		},
	})
	if res.status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422. body: %s", res.status, res.body)
	}
	if !strings.Contains(string(res.body), "variables[1].token") {
		t.Errorf("the error must name the offending row: %s", res.body)
	}

	// A failed write leaves the previous declarations alone.
	got := dataOf[variablesBody](t, env.asOwner(t, w, http.MethodGet, variablePath(novel.ID)))
	if len(got.Variables) != 0 {
		t.Errorf("a rejected request wrote something: %+v", got.Variables)
	}
}

// The usage report reads every place a writer can type - prose, chat, and
// headcanon bodies. Missing one would report a token as unused while it is on
// screen.
func TestVariables_UsageScansEveryRepresentation(t *testing.T) {
	t.Parallel()
	env := newAuthEnv(t)
	w := env.newWriter(t)

	novel := env.createNovel(t, w, createNovelBody(uniqueName(t, "Usage "), map[string]any{}))
	env.createChapter(t, w, novel.ID, map[string]any{"content": "(y/n) ยืนอยู่ตรงนั้น"})
	env.createChapter(t, w, novel.ID, map[string]any{
		"presentation_format": "chat",
		"messages": []map[string]any{
			{"speaker_name": "Alice", "message_type": "message", "content": "สวัสดี (l/n)"},
		},
	})
	env.createChapter(t, w, novel.ID, map[string]any{
		"presentation_format": "headcanon",
		"entries": []map[string]any{
			{"name": "อลิซ", "body": "เรียก (n/n) เสมอเวลาโกรธ"},
		},
	})

	got := dataOf[variablesBody](t, env.asOwner(t, w, http.MethodPut, variablePath(novel.ID),
		map[string]any{"variables": []map[string]any{
			{"token": "(y/n)", "label": "ชื่อของคุณ", "kind": "text"},
			{"token": "(d/n)", "label": "ชื่อลูกสาว", "kind": "text"},
		}}))

	for _, token := range []string{"(l/n)", "(n/n)"} {
		if !contains(got.Usage.Undeclared, token) {
			t.Errorf("%s is used but undeclared, and was not reported: %+v", token, got.Usage)
		}
	}
	if contains(got.Usage.Undeclared, "(y/n)") {
		t.Errorf("a declared token must not be reported as undeclared: %+v", got.Usage)
	}
	if !contains(got.Usage.Unused, "(d/n)") {
		t.Errorf("an unused declaration was not reported: %+v", got.Usage)
	}
	if contains(got.Usage.Unused, "(y/n)") {
		t.Errorf("a token used in the prose was reported as unused: %+v", got.Usage)
	}
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}
