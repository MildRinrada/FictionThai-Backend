package achievements

import (
	"strings"
	"testing"
)

// The catalogue is product copy as much as it is code, and the rules it has to
// obey are in docs/PROFILE-AND-ACHIEVEMENTS.md Part 3 rather than in any
// compiler. These tests are where those rules live.

// The starter set, named: เส้นทาง entire, the system-poking four, and the two
// ตัวตน achievements. A key silently dropped or renamed would leave an award
// row nobody can render.
func TestCatalog_ShipsTheStarterSet(t *testing.T) {
	want := map[string]Family{
		KeyFirstChapter:  FamilyPath,
		KeyFirstReader:   FamilyPath,
		KeyCompleted:     FamilyPath,
		KeyManyVoices:    FamilyPath,
		KeyWorldbuilder:  FamilyPath,
		KeyNativeSpeaker: FamilyPath,
		KeyTrustNoAI:     FamilyPath,

		KeyOneIsEnough: FamilyIdentity,
		KeyBackAgain:   FamilyIdentity,

		KeyEggDevTools:       FamilyEgg,
		KeyEggAdminPath:      FamilyEgg,
		KeyEggDisabledButton: FamilyEgg,
		KeyEggCtrlS:          FamilyEgg,
	}

	if len(Catalog) != len(want) {
		t.Fatalf("catalogue holds %d entries, want %d", len(Catalog), len(want))
	}
	for key, family := range want {
		definition, ok := Lookup(key)
		if !ok {
			t.Errorf("%q is missing from the catalogue", key)
			continue
		}
		if definition.Family != family {
			t.Errorf("%q is in family %q, want %q", key, definition.Family, family)
		}
		if strings.TrimSpace(definition.Title) == "" {
			t.Errorf("%q has no title", key)
		}
		if definition.Threshold < 1 {
			t.Errorf("%q has threshold %d; an achievement nobody can earn is a bug",
				key, definition.Threshold)
		}
	}
}

// Only the four eggs may be reached from a browser, and each one has to carry
// the words the owner will be shown. A path achievement that became
// client-triggerable would let anyone with a console award themselves work
// they never did.
func TestCatalog_OnlyEggsAreClientTriggerable(t *testing.T) {
	for _, definition := range Catalog {
		if definition.Family == FamilyEgg {
			if !definition.ClientTriggerable {
				t.Errorf("%q is an egg but cannot be triggered from the browser "+
					"that finds it", definition.Key)
			}
			if definition.Trigger == "" || definition.Message == "" {
				t.Errorf("%q has no trigger or message for its finder to read",
					definition.Key)
			}
			if definition.ClientCount < 1 {
				t.Errorf("%q never tells the browser how many times to see it",
					definition.Key)
			}
			continue
		}
		if definition.ClientTriggerable {
			t.Errorf("%q implies real work and must not be client-triggerable",
				definition.Key)
		}
		if definition.Description == "" {
			t.Errorf("%q is listed openly but says nothing about what earns it",
				definition.Key)
		}
	}
}

// Every counter carries a cooldown, EXCEPT the reader-driven ones, which are
// protected by the distinct-account rule instead - and would be broken by a
// cooldown, since it would drop the second reader to arrive in a minute.
func TestCatalog_EveryCounterIsProtected(t *testing.T) {
	for _, definition := range Catalog {
		switch {
		case definition.DistinctActors && definition.Cooldown != 0:
			t.Errorf("%q counts readers AND carries a cooldown; the cooldown "+
				"would silently drop the second reader in a minute", definition.Key)
		case !definition.DistinctActors && definition.Cooldown <= 0:
			t.Errorf("%q has no cooldown, so it can be farmed", definition.Key)
		}
	}
}

// Tone: no game vocabulary anywhere a writer can read. No XP, no levels, no
// "achievement unlocked!". The set is a set of private facts about someone's
// practice, not a scoreboard.
func TestCatalog_UsesNoGameVocabulary(t *testing.T) {
	banned := []string{
		"xp", "level up", "levelup", "achievement unlocked", "unlocked!",
		"leaderboard", "high score", "แต้ม", "คะแนน", "เลเวล", "อันดับ",
	}
	for _, definition := range Catalog {
		copy := strings.ToLower(strings.Join([]string{
			definition.Title, definition.Description,
			definition.Trigger, definition.Message,
		}, " "))
		for _, word := range banned {
			if strings.Contains(copy, word) {
				t.Errorf("%q uses game vocabulary (%q)", definition.Key, word)
			}
		}
	}
}

// The security-adjacent eggs must read as "we noticed, nice try, here is how to
// report something real" - never as a warning. A threat here would be the
// platform frightening the most curious person who ever visited it.
func TestCatalog_SecurityEggsAreWarmAndPointSomewhere(t *testing.T) {
	threats := []string{"ห้าม", "ผิดกฎ", "ดำเนินคดี", "ระงับบัญชี", "เตือนครั้ง", "แบน"}
	for _, key := range []string{KeyEggDevTools, KeyEggAdminPath} {
		definition, ok := Lookup(key)
		if !ok {
			t.Fatalf("%q is missing", key)
		}
		for _, threat := range threats {
			if strings.Contains(definition.Message, threat) {
				t.Errorf("%q reads as a warning (%q)", key, threat)
			}
		}
		if !strings.Contains(definition.Message, "ติดต่อ") {
			t.Errorf("%q does not tell the finder where to report something real", key)
		}
	}
}

// The counts the profile renders. If either of these ever included eggs in the
// listed total, a visitor could work out how many eggs exist by subtraction -
// which is fine - but a listed total that COUNTED them would make the medal
// grid permanently incomplete for everybody.
func TestCatalog_CountsSplitByVisibility(t *testing.T) {
	if EggTotal() != 4 {
		t.Errorf("EggTotal() = %d, want 4", EggTotal())
	}
	if ListedTotal() != len(Catalog)-EggTotal() {
		t.Errorf("ListedTotal() = %d, want %d", ListedTotal(), len(Catalog)-EggTotal())
	}
	for _, definition := range Catalog {
		if definition.Family == FamilyEgg && definition.Description != "" {
			t.Errorf("%q carries a description; an egg must not describe how to "+
				"get it anywhere a public shape could pick it up", definition.Key)
		}
	}
}
