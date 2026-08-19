package profiles

import "testing"

// The two pure rules the writer band stands on: the rotation that guarantees
// นักเขียนหน้าใหม่ its own week, and the banding that keeps exact popularity
// numbers off the API (docs/WRITER-SPOTLIGHT.md).

func TestSpotlightRotationGivesEveryKindAWeek(t *testing.T) {
	seen := map[SpotlightKind]bool{}
	for week := 0; week < 3; week++ {
		order := spotlightRotation(week)
		if len(order) != 3 {
			t.Fatalf("week %d: rotation has %d kinds, want 3", week, len(order))
		}
		seen[order[0]] = true
	}
	for _, kind := range []SpotlightKind{
		SpotlightRising, SpotlightNewcomer, SpotlightConsistent,
	} {
		if !seen[kind] {
			t.Errorf("kind %q never leads a week - the same names would hold the band forever", kind)
		}
	}
}

func TestSpotlightRotationFallbackCoversAllKinds(t *testing.T) {
	// A thin week falls through the rotation; every kind must appear exactly
	// once so the fallback can never loop or skip.
	for week := 0; week < 6; week++ {
		seen := map[SpotlightKind]int{}
		for _, kind := range spotlightRotation(week) {
			seen[kind]++
		}
		for kind, n := range seen {
			if n != 1 {
				t.Errorf("week %d: kind %q appears %d times, want 1", week, kind, n)
			}
		}
	}
}

func TestBandNeverLeaksAnExactCount(t *testing.T) {
	cases := []struct {
		count int64
		want  string
	}{
		{0, ""}, {9, ""}, // below the first threshold the card says nothing
		{10, "10+"}, {49, "10+"},
		{50, "50+"}, {99, "50+"},
		{100, "100+"}, {12345, "100+"},
	}
	for _, tc := range cases {
		if got := band(tc.count); got != tc.want {
			t.Errorf("band(%d) = %q, want %q", tc.count, got, tc.want)
		}
	}
}
