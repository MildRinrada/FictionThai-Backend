package variables

import "testing"

// classifyMentions - the rule that keeps "(Scaramouche/Wanderer)" out of the
// undeclared-variable nag (settings review round 3). DETERMINISTIC: a token is
// a mention exactly when a slash piece equals a cast name or one word of a
// cast name, case-insensitively - never a guess from shape.

func usageWith(tokens ...string) Usage {
	usage := emptyUsage()
	for _, token := range tokens {
		usage.Undeclared = append(usage.Undeclared, token)
		usage.UndeclaredUses = append(usage.UndeclaredUses, TokenUse{
			Token:    token,
			Chapters: []ChapterRef{{Number: 1, Slug: "chapter-1"}},
		})
	}
	return usage
}

func TestClassifyMentions_SplitsCastNamesOutOfUndeclared(t *testing.T) {
	got := classifyMentions(
		usageWith("(y/n)", "(Scaramouche/Wanderer)"),
		[]string{"Scaramouche"},
	)

	if len(got.Undeclared) != 1 || got.Undeclared[0] != "(y/n)" {
		t.Fatalf("Undeclared = %v, want only (y/n)", got.Undeclared)
	}
	if len(got.CharacterMentions) != 1 || got.CharacterMentions[0] != "(Scaramouche/Wanderer)" {
		t.Fatalf("CharacterMentions = %v, want (Scaramouche/Wanderer)", got.CharacterMentions)
	}
	// The chapter links follow the tokens that KEPT their warning.
	if len(got.UndeclaredUses) != 1 || got.UndeclaredUses[0].Token != "(y/n)" {
		t.Fatalf("UndeclaredUses = %v, want only (y/n)", got.UndeclaredUses)
	}
}

func TestClassifyMentions_MatchesWordsOfBracketedAliasNames(t *testing.T) {
	// A cast member entered as "สการามุช (Wanderer)": either half of the
	// token names them.
	got := classifyMentions(
		usageWith("(Scaramouche/Wanderer)"),
		[]string{"สการามุช (Wanderer)"},
	)
	if len(got.CharacterMentions) != 1 {
		t.Fatalf("CharacterMentions = %v, want the token classified", got.CharacterMentions)
	}
}

func TestClassifyMentions_IsCaseInsensitiveAndExactPerPiece(t *testing.T) {
	got := classifyMentions(
		usageWith("(scaramouche/anyone)", "(y/n)"),
		[]string{"Scaramouche"},
	)
	if len(got.CharacterMentions) != 1 || got.CharacterMentions[0] != "(scaramouche/anyone)" {
		t.Fatalf("CharacterMentions = %v", got.CharacterMentions)
	}
	// "(y/n)" must NEVER be swallowed: "y" is not a cast name, and pieces are
	// compared whole - a name merely CONTAINING a piece is no match.
	if len(got.Undeclared) != 1 || got.Undeclared[0] != "(y/n)" {
		t.Fatalf("Undeclared = %v, want (y/n) kept", got.Undeclared)
	}
}

func TestClassifyMentions_NoCastMeansNoReclassification(t *testing.T) {
	got := classifyMentions(usageWith("(Scaramouche/Wanderer)"), nil)
	if len(got.Undeclared) != 1 || len(got.CharacterMentions) != 0 {
		t.Fatalf("without a cast nothing may be reclassified: %+v", got)
	}
}
