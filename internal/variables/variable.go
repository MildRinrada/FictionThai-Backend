// Package variables owns reader variables - the tokens a fiction declares and a
// reader answers (docs/PHASE-13-CREATION-AND-CONTROL.md §13H).
//
// It generalises 12B's single y/n switch. The rule that carried over unchanged,
// and that every part of this package exists to protect:
//
//	Never substitute at save. Store tokens; resolve at render.
//
// Substituted text could never be renamed afterwards, and every reader of a
// cached chapter would see one reader's name. Nothing here writes chapter
// content, and the reader's ANSWERS are never sent to the server at all - they
// live in the reader's browser, which is also what lets a guest use the feature
// (docs/10 §2.1).
//
// Ownership flows Novel -> Variable, so this package stores no owner of its own:
// it asks the novels service, the single authorization boundary for the subtree
// (docs/10 §27).
package variables

import (
	"strings"
	"time"

	"github.com/google/uuid"
)

// Kind is what a reader answers with.
type Kind string

const (
	// KindText is a free-text answer: a name, a nickname, a colour.
	KindText Kind = "text"
	// KindChoice is one of a list the AUTHOR defined.
	KindChoice Kind = "choice"
	// KindPronoun is one linked SET of words. It earns its own kind because a
	// pronoun is not one word: choosing "เขา" also decides "ของเขา". One
	// declaration therefore serves readers of any gender without the writer
	// maintaining three versions of the text.
	KindPronoun Kind = "pronoun"
)

func (k Kind) Valid() bool {
	switch k {
	case KindText, KindChoice, KindPronoun:
		return true
	}
	return false
}

func (k Kind) String() string { return string(k) }

// Kinds returns every supported value, for API metadata.
func Kinds() []Kind { return []Kind{KindText, KindChoice, KindPronoun} }

// PronounSet is one selectable set of linked forms.
//
// Values answer Options.Forms positionally, the same way a headcanon entry
// answers its topic's fields.
type PronounSet struct {
	Label  string   `json:"label"`
	Values []string `json:"values"`
}

// Options carries the kind-specific configuration.
//
// One struct rather than three, because it is stored in one JSONB column and a
// kind change must not leave a shape the decoder cannot read.
type Options struct {
	// Values are the choices, for KindChoice.
	Values []string `json:"values,omitempty"`
	// Forms are the pronoun form NAMES, for KindPronoun - e.g. ประธาน, เจ้าของ.
	Forms []string `json:"forms,omitempty"`
	// Sets are the selectable pronoun sets, for KindPronoun.
	Sets []PronounSet `json:"sets,omitempty"`
}

// IsEmpty reports whether the options carry nothing, so an unconfigured text
// variable stores SQL NULL rather than an empty object.
func (o *Options) IsEmpty() bool {
	return o == nil || (len(o.Values) == 0 && len(o.Forms) == 0 && len(o.Sets) == 0)
}

// Variable mirrors one novel_variables row.
type Variable struct {
	ID       uuid.UUID
	NovelID  uuid.UUID
	Position int

	// Token is the literal placeholder the author types into the text. It is
	// matched LITERALLY at render, never compiled as a regular expression.
	Token string
	// Label is what the READER is asked.
	Label string
	// DefaultValue is what the text shows before the reader answers.
	DefaultValue *string

	Kind    Kind
	Options *Options

	CreatedAt time.Time
	UpdatedAt time.Time
}

// View is one variable as returned by the API.
//
// Public: a reader needs the declaration to render the form and to fill the
// slots. There is nothing private in it - the answers are the private part, and
// they never come here.
type View struct {
	ID           uuid.UUID `json:"id"`
	Position     int       `json:"position"`
	Token        string    `json:"token"`
	Label        string    `json:"label"`
	DefaultValue *string   `json:"default_value,omitempty"`
	Kind         Kind      `json:"kind"`
	Options      *Options  `json:"options,omitempty"`

	// Tokens is every literal placeholder this declaration produces: one for a
	// text or choice variable, and one per FORM for a pronoun. Served so the
	// client never has to rebuild the suffix rule and disagree with the server
	// about which strings to replace (docs/09 §51).
	Tokens []string `json:"tokens"`
}

// FormSeparator joins a pronoun's base token to one of its forms, giving each
// form a token of its own - (p/n) and (p/n.เจ้าของ).
//
// A separator rather than positional syntax because the writer never types it:
// the editor's insert button offers each form by name and writes the token.
const FormSeparator = "."

// FormToken returns the literal placeholder for one form of a pronoun.
//
// Form 0 is the BASE token unsuffixed, so the commonest case reads as an
// ordinary variable and an author who never thinks about forms never sees one.
func (v Variable) FormToken(index int) string {
	if v.Kind != KindPronoun || index <= 0 || v.Options == nil ||
		index >= len(v.Options.Forms) {
		return v.Token
	}
	// The base token carries its own delimiters, e.g. "(p/n)". The form name
	// goes INSIDE them so the result is still one bracketed marker.
	base := v.Token
	if last := len(base) - 1; last > 0 && base[last] == ')' {
		return base[:last] + FormSeparator + v.Options.Forms[index] + ")"
	}
	return base + FormSeparator + v.Options.Forms[index]
}

// AllTokens returns every literal placeholder this declaration produces.
func (v Variable) AllTokens() []string {
	if v.Kind != KindPronoun || v.Options == nil || len(v.Options.Forms) == 0 {
		return []string{v.Token}
	}
	tokens := make([]string, 0, len(v.Options.Forms))
	for i := range v.Options.Forms {
		tokens = append(tokens, v.FormToken(i))
	}
	return tokens
}

func (v Variable) View() View {
	return View{
		ID:           v.ID,
		Position:     v.Position,
		Token:        v.Token,
		Label:        v.Label,
		DefaultValue: v.DefaultValue,
		Kind:         v.Kind,
		Options:      v.Options,
		Tokens:       v.AllTokens(),
	}
}

// Usage is the advisory report a writer gets after saving
// (docs/PHASE-13-CREATION-AND-CONTROL.md §13H "Validation on save").
//
// WARNINGS, never errors. A token typed before its declaration exists is an
// ordinary order of work, and a variable declared for a chapter not yet written
// is not a mistake. Refusing the save would only make the writer fight the form.
type Usage struct {
	// Undeclared are token-shaped strings found in the fiction's text that no
	// variable declares.
	Undeclared []string `json:"undeclared"`
	// UndeclaredUses says WHERE each undeclared token was found, so the studio
	// can link the writer to the chapter instead of only naming the token. Same
	// tokens as Undeclared, which stays for clients that only need the list.
	UndeclaredUses []TokenUse `json:"undeclared_uses"`
	// Unused are declared tokens that appear nowhere in the fiction's text.
	Unused []string `json:"unused"`
	// CharacterMentions are token-shaped strings whose slash pieces name one of
	// the fiction's own CHARACTERS - "(Scaramouche/Wanderer)" beside a cast
	// member called either is the writer styling a name, not a question for the
	// reader. Classified HERE, against the cast the writer declared, so every
	// surface agrees; no client is left guessing from the token's shape
	// (docs/09 §51). These never appear in Undeclared.
	CharacterMentions []string `json:"character_mentions"`
}

// emptyUsage is the report with nothing to report - every list present and
// empty, never nil, so no client reads `.length` off JSON null.
func emptyUsage() Usage {
	return Usage{
		Undeclared:        []string{},
		UndeclaredUses:    []TokenUse{},
		Unused:            []string{},
		CharacterMentions: []string{},
	}
}

// classifyMentions moves tokens that name a cast member out of Undeclared and
// into CharacterMentions. Deterministic on purpose (settings review round 3):
// a token is a mention exactly when one of its slash pieces equals a
// character's name, or one word of a character's name, case-insensitively -
// never a guess from capitalisation or length.
func classifyMentions(usage Usage, castNames []string) Usage {
	if len(castNames) == 0 || len(usage.Undeclared) == 0 {
		return usage
	}

	names := map[string]struct{}{}
	add := func(word string) {
		word = strings.ToLower(strings.TrimSpace(word))
		// A word that arrived wrapped in brackets - the "(Wanderer)" half of a
		// cast name like "สการามุช (Wanderer)" - is the word inside them.
		word = strings.Trim(word, "()[]")
		if len([]rune(word)) >= 2 {
			names[word] = struct{}{}
		}
	}
	for _, name := range castNames {
		add(name)
		for _, word := range strings.Fields(name) {
			add(word)
		}
	}

	isMention := func(token string) bool {
		inner := strings.TrimSuffix(strings.TrimPrefix(token, "("), ")")
		for _, piece := range strings.Split(inner, "/") {
			if _, ok := names[strings.ToLower(strings.TrimSpace(piece))]; ok {
				return true
			}
		}
		return false
	}

	kept := Usage{
		Undeclared:        []string{},
		UndeclaredUses:    []TokenUse{},
		Unused:            usage.Unused,
		CharacterMentions: []string{},
	}
	for _, use := range usage.UndeclaredUses {
		if isMention(use.Token) {
			kept.CharacterMentions = append(kept.CharacterMentions, use.Token)
			continue
		}
		kept.Undeclared = append(kept.Undeclared, use.Token)
		kept.UndeclaredUses = append(kept.UndeclaredUses, use)
	}
	return kept
}

// TokenUse is one undeclared token and the chapters it appears in.
type TokenUse struct {
	Token    string       `json:"token"`
	Chapters []ChapterRef `json:"chapters"`
}

// ChapterRef locates a chapter for the writer's report - just enough to name it
// and link to it. Declared here rather than imported: variables must not depend
// on the chapters package (the dependency arrow points at novels only).
type ChapterRef struct {
	Number int     `json:"chapter_number"`
	Title  *string `json:"title,omitempty"`
	Slug   string  `json:"slug"`
}
