package novels

import "testing"

// Phase 13A - the two fields that join the create form
// (docs/PHASE-13-CREATION-AND-CONTROL.md §13A, §13B).

func TestValidateAgeRating(t *testing.T) {
	t.Run("is required on create", func(t *testing.T) {
		errs := validationErrors{}
		validateAgeRating(errs, "", true)
		if len(errs["age_rating"]) == 0 {
			t.Fatal("a fiction was created without its author stating a rating")
		}
	})

	t.Run("is optional on update", func(t *testing.T) {
		errs := validationErrors{}
		if got := validateAgeRating(errs, "", false); got != DefaultAgeRating {
			t.Fatalf("rating = %q, want the default", got)
		}
		if len(errs) != 0 {
			t.Fatalf("an edit that does not mention the rating was rejected: %v", errs)
		}
	})

	t.Run("accepts the three documented values and nothing else", func(t *testing.T) {
		for _, rating := range AgeRatings() {
			errs := validationErrors{}
			if got := validateAgeRating(errs, string(rating), true); got != rating {
				t.Fatalf("rating %q came back as %q", rating, got)
			}
			if len(errs) != 0 {
				t.Fatalf("documented rating %q rejected: %v", rating, errs)
			}
		}
		for _, bad := range []string{"18+", "adult", "GENERAL", "nsfw", "<script>"} {
			errs := validationErrors{}
			validateAgeRating(errs, bad, true)
			if len(errs["age_rating"]) == 0 {
				t.Fatalf("unknown rating %q accepted", bad)
			}
		}
	})

	// Only 18+ is kept out of browse surfaces. 15+ carries a warning, not a
	// disappearance - hiding it would punish a writer for being honest.
	t.Run("restricts listing only for 18+", func(t *testing.T) {
		if !RatingMature.RestrictsListing() {
			t.Fatal("18+ work must not appear in listings by default")
		}
		if RatingGeneral.RestrictsListing() || RatingTeen.RestrictsListing() {
			t.Fatal("only 18+ is excluded from listings")
		}
	})
}

func TestValidateAgeGate(t *testing.T) {
	// The wider gate is the default: nobody is locked out of a work because
	// the writer never opened the setting.
	t.Run("defaults to the warning, not the strict gate", func(t *testing.T) {
		errs := validationErrors{}
		if got := validateAgeGate(errs, "", RatingMature); got != GateWarning {
			t.Fatalf("gate = %q, want %q", got, GateWarning)
		}
	})

	// Explicit work has a FLOOR: the warning gate is refused rather than
	// silently raised, and an omitted gate takes login instead of warning.
	t.Run("explicit work refuses the warning gate", func(t *testing.T) {
		errs := validationErrors{}
		validateAgeGate(errs, string(GateWarning), RatingExplicit)
		if len(errs["age_gate"]) == 0 {
			t.Fatal("explicit + warning must be refused, not quietly upgraded")
		}

		errs = validationErrors{}
		if got := validateAgeGate(errs, "", RatingExplicit); got != GateLogin {
			t.Fatalf("omitted gate on explicit work = %q, want %q", got, GateLogin)
		}
		if len(errs) != 0 {
			t.Fatalf("a gate the writer never named must not be an error: %v", errs)
		}
	})

	t.Run("accepts every documented gate", func(t *testing.T) {
		for _, gate := range AgeGates() {
			errs := validationErrors{}
			if got := validateAgeGate(errs, string(gate), RatingMature); got != gate {
				t.Fatalf("gate %q came back as %q", gate, got)
			}
			if len(errs) != 0 {
				t.Fatalf("documented gate %q rejected: %v", gate, errs)
			}
		}
	})

	// "ฉันอายุ 18 ปี" as a bare self-declaration is not one of the two options.
	t.Run("rejects a gate it does not know", func(t *testing.T) {
		for _, bad := range []string{"self_declared", "confirm", "none", "VERIFIED"} {
			errs := validationErrors{}
			validateAgeGate(errs, bad, RatingMature)
			if len(errs["age_gate"]) == 0 {
				t.Fatalf("unknown gate %q accepted", bad)
			}
		}
	})
}

func TestValidateOrigin(t *testing.T) {
	source := func(v string) *string { return &v }

	t.Run("defaults to original", func(t *testing.T) {
		errs := validationErrors{}
		origin, fandom := validateOrigin(errs, "", nil)
		if origin != OriginOriginal || fandom != nil {
			t.Fatalf("origin = %q, fandom = %v", origin, fandom)
		}
	})

	t.Run("keeps a source only for a fanfiction", func(t *testing.T) {
		errs := validationErrors{}
		origin, fandom := validateOrigin(errs, "fanfiction", source("  วรรณคดีไทย  "))
		if origin != OriginFanfiction {
			t.Fatalf("origin = %q", origin)
		}
		if fandom == nil || *fandom != "วรรณคดีไทย" {
			t.Fatalf("fandom not trimmed or lost: %v", fandom)
		}
	})

	// The author said "this is my own work". That is the answer to honour -
	// dropping the stray source beats rejecting the whole request.
	t.Run("drops a source on original work rather than failing", func(t *testing.T) {
		errs := validationErrors{}
		origin, fandom := validateOrigin(errs, "original", source("Some Source"))
		if origin != OriginOriginal {
			t.Fatalf("origin = %q", origin)
		}
		if fandom != nil {
			t.Fatalf("original work kept a source: %q", *fandom)
		}
		if len(errs) != 0 {
			t.Fatalf("creation failed over a field the author did not mean to set: %v", errs)
		}
	})

	t.Run("bounds the source name", func(t *testing.T) {
		long := make([]rune, FandomMaxLength+1)
		for i := range long {
			long[i] = 'ก'
		}
		errs := validationErrors{}
		validateOrigin(errs, "fanfiction", source(string(long)))
		if len(errs["fandom"]) == 0 {
			t.Fatal("an over-long source name was accepted")
		}
	})

	t.Run("rejects an origin it does not know", func(t *testing.T) {
		errs := validationErrors{}
		validateOrigin(errs, "doujin", nil)
		if len(errs["origin_type"]) == 0 {
			t.Fatal("unknown origin accepted")
		}
	})
}
