package variables_test

import (
	"strings"
	"testing"

	"github.com/fictionthai/fictionthai/backend/internal/variables"
	"github.com/fictionthai/fictionthai/backend/pkg/apierror"
)

// Phase 13H - reader variables (docs/PHASE-13-CREATION-AND-CONTROL.md §13H).
//
// These carry forward the rules 12B's y/n tests established, which is the point
// of the slice: the toggle became the first row of a table, and none of its
// guarantees were dropped on the way.

func fieldsOf(t *testing.T, err error) map[string][]string {
	t.Helper()
	if err == nil {
		t.Fatal("expected a validation error")
	}
	apiErr, ok := err.(*apierror.Error)
	if !ok {
		t.Fatalf("expected an *apierror.Error, got %T", err)
	}
	return apiErr.Fields
}

func text(token, label string) variables.Input {
	return variables.Input{Token: token, Label: label, Kind: "text"}
}

func TestValidate_Token(t *testing.T) {
	list, err := variables.Validate([]variables.Input{text("  (y/n)  ", "  ชื่อของคุณ  ")})
	if err != nil {
		t.Fatalf("a normal declaration should not error: %v", err)
	}
	if list[0].Token != "(y/n)" || list[0].Label != "ชื่อของคุณ" {
		t.Errorf("token/label not trimmed: %+v", list[0])
	}
	if list[0].Position != 0 {
		t.Errorf("position = %d, want 0", list[0].Position)
	}

	for _, bad := range []struct {
		name  string
		token string
	}{
		{"empty", "   "},
		// A token has to survive being typed mid-sentence and found again by an
		// exact string search, which a space breaks.
		{"contains a space", "(Y N)"},
		{"too long", strings.Repeat("x", variables.TokenMaxLength+1)},
		// A NUL would also be refused by PostgreSQL; a clean 422 beats a 500.
		{"control characters", "(Y\x00N)"},
	} {
		t.Run(bad.name, func(t *testing.T) {
			_, err := variables.Validate([]variables.Input{text(bad.token, "ชื่อ")})
			if len(fieldsOf(t, err)["variables[0].token"]) == 0 {
				t.Errorf("expected %s to be rejected", bad.name)
			}
		})
	}
}

// Two rows answering to one placeholder would make which answer wins depend on
// row order - and the UNIQUE constraint would reject the write anyway, with an
// error naming a constraint instead of a field.
func TestValidate_RejectsDuplicateTokens(t *testing.T) {
	_, err := variables.Validate([]variables.Input{
		text("(y/n)", "ชื่อของคุณ"),
		text("(y/n)", "ชื่ออีกอัน"),
	})
	if len(fieldsOf(t, err)["variables[1].token"]) == 0 {
		t.Error("a duplicate token must be a field error on the SECOND row")
	}
}

func TestValidate_PositionsComeFromArrayOrder(t *testing.T) {
	list, err := variables.Validate([]variables.Input{
		text("(y/n)", "ชื่อ"), text("(l/n)", "นามสกุล"), text("(n/n)", "ชื่อเล่น"),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i, variable := range list {
		if variable.Position != i {
			t.Fatalf("variable %d has position %d - array order was not used", i, variable.Position)
		}
	}
}

func TestValidate_Choice(t *testing.T) {
	list, err := variables.Validate([]variables.Input{{
		Token: "(e/c)", Label: "สีตา", Kind: "choice",
		Options: &variables.Options{Values: []string{" น้ำตาล ", "ฟ้า"}},
	}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := list[0].Options.Values; len(got) != 2 || got[0] != "น้ำตาล" {
		t.Errorf("choices not trimmed or lost: %v", got)
	}

	_, err = variables.Validate([]variables.Input{{
		Token: "(e/c)", Label: "สีตา", Kind: "choice",
	}})
	if len(fieldsOf(t, err)["variables[0].options"]) == 0 {
		t.Error("a choice with no options is a choice of nothing")
	}
}

// A pronoun is not one word: choosing เขา also decides ของเขา. One declaration
// therefore has to carry the whole linked set.
func TestValidate_Pronoun(t *testing.T) {
	list, err := variables.Validate([]variables.Input{{
		Token: "(P/N)", Label: "สรรพนามของคุณ", Kind: "pronoun",
		Options: &variables.Options{
			Forms: []string{"ประธาน", "เจ้าของ"},
			Sets: []variables.PronounSet{
				{Label: "เขา (ชาย)", Values: []string{"เขา", "ของเขา"}},
				// Deliberately short: the missing form must be filled with a
				// blank rather than shifting the next word onto the wrong form.
				{Label: "เธอ", Values: []string{"เธอ"}},
			},
		},
	}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	sets := list[0].Options.Sets
	if len(sets) != 2 {
		t.Fatalf("sets = %d, want 2", len(sets))
	}
	if len(sets[1].Values) != 2 || sets[1].Values[1] != "" {
		t.Errorf("a short set must be padded, not shifted: %v", sets[1].Values)
	}

	// Every form gets a token of its own, and form 0 keeps the bare token so an
	// author who never thinks about forms never sees the syntax.
	tokens := list[0].AllTokens()
	if len(tokens) != 2 || tokens[0] != "(P/N)" || tokens[1] != "(P/N.เจ้าของ)" {
		t.Errorf("form tokens = %v", tokens)
	}

	// A form name becomes part of a token, so it obeys the token rules.
	_, err = variables.Validate([]variables.Input{{
		Token: "(P/N)", Label: "สรรพนาม", Kind: "pronoun",
		Options: &variables.Options{
			Forms: []string{"มี ช่องว่าง"},
			Sets:  []variables.PronounSet{{Label: "เขา", Values: []string{"เขา"}}},
		},
	}})
	if len(fieldsOf(t, err)["variables[0].options.forms[0]"]) == 0 {
		t.Error("a form name with a space would build an unfindable token")
	}
}

// A writer switching a variable from choice to text should not have to clear
// the old options by hand, and keeping them would leave a row whose stored
// shape contradicts its kind.
func TestValidate_DropsOptionsThatBelongToAnotherKind(t *testing.T) {
	list, err := variables.Validate([]variables.Input{{
		Token: "(y/n)", Label: "ชื่อของคุณ", Kind: "text",
		Options: &variables.Options{Values: []string{"เหลือจากตอนเป็น choice"}},
	}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if list[0].Options != nil {
		t.Errorf("a text variable must carry no options: %+v", list[0].Options)
	}
}

func TestValidate_RejectsUnknownKind(t *testing.T) {
	_, err := variables.Validate([]variables.Input{{
		Token: "(y/n)", Label: "ชื่อ", Kind: "colour",
	}})
	if len(fieldsOf(t, err)["variables[0].kind"]) == 0 {
		t.Error("an unknown kind must be rejected rather than defaulted")
	}
}

func TestValidate_BoundsTheList(t *testing.T) {
	inputs := make([]variables.Input, variables.MaxPerNovel+1)
	for i := range inputs {
		inputs[i] = text("(V"+strings.Repeat("i", i)+")", "ตัวแปร")
	}
	if len(fieldsOf(t, mustErr(t, inputs))["variables"]) == 0 {
		t.Error("the list must be bounded")
	}
}

func mustErr(t *testing.T, inputs []variables.Input) error {
	t.Helper()
	_, err := variables.Validate(inputs)
	return err
}

// An omitted kind is text - the commonest declaration by far, and the one 12B
// could express at all.
func TestValidate_DefaultsToText(t *testing.T) {
	list, err := variables.Validate([]variables.Input{{Token: "(y/n)", Label: "ชื่อของคุณ"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if list[0].Kind != variables.KindText {
		t.Errorf("kind = %q, want text", list[0].Kind)
	}
}
