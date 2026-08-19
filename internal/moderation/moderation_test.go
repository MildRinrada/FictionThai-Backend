package moderation

import "testing"

// TestLifecycleTransitions pins the documented state machine (docs/08 §24.1):
// pending → reviewing → resolved/rejected, the review step skippable, nothing
// backwards, terminal states final.
func TestLifecycleTransitions(t *testing.T) {
	cases := []struct {
		from, to Status
		want     bool
	}{
		{StatusPending, StatusReviewing, true},
		{StatusPending, StatusResolved, true},
		{StatusPending, StatusRejected, true},
		{StatusReviewing, StatusResolved, true},
		{StatusReviewing, StatusRejected, true},

		// Nothing moves backwards.
		{StatusReviewing, StatusPending, false},
		{StatusResolved, StatusPending, false},
		{StatusResolved, StatusReviewing, false},

		// Terminal states are final - including flips between them.
		{StatusResolved, StatusRejected, false},
		{StatusRejected, StatusResolved, false},
		{StatusRejected, StatusReviewing, false},

		// Self-transitions are not transitions.
		{StatusPending, StatusPending, false},
		{StatusResolved, StatusResolved, false},
	}
	for _, tc := range cases {
		if got := CanTransition(tc.from, tc.to); got != tc.want {
			t.Errorf("CanTransition(%s, %s) = %v, want %v", tc.from, tc.to, got, tc.want)
		}
	}

	if !StatusResolved.Terminal() || !StatusRejected.Terminal() {
		t.Error("resolved and rejected must be terminal")
	}
	if StatusPending.Terminal() || StatusReviewing.Terminal() {
		t.Error("pending and reviewing must not be terminal")
	}
}

// TestActionMatrix pins the per-target action matrix to the states earlier
// phases actually created - no hide for fiction (no hidden column), no
// content actions for users, no user actions for content.
func TestActionMatrix(t *testing.T) {
	cases := []struct {
		target TargetType
		action Action
		want   bool
	}{
		{TargetNovel, ActionRemove, true},
		{TargetNovel, ActionRestore, true},
		{TargetNovel, ActionHide, false},
		{TargetNovel, ActionBan, false},

		{TargetChapter, ActionRemove, true},
		{TargetChapter, ActionRestore, true},
		{TargetChapter, ActionHide, false},

		{TargetComment, ActionHide, true},
		{TargetComment, ActionRemove, true},
		{TargetComment, ActionRestore, true},
		{TargetComment, ActionSuspend, false},

		{TargetCommunityPost, ActionHide, true},
		{TargetCommunityPost, ActionRemove, true},
		{TargetCommunityPost, ActionRestore, true},
		{TargetCommunityPost, ActionWarn, false},

		{TargetCommunityComment, ActionHide, true},
		{TargetCommunityComment, ActionRemove, true},
		{TargetCommunityComment, ActionRestore, true},

		{TargetUser, ActionWarn, true},
		{TargetUser, ActionSuspend, true},
		{TargetUser, ActionBan, true},
		{TargetUser, ActionRestore, true},
		{TargetUser, ActionHide, false},
		{TargetUser, ActionRemove, false},

		{TargetMedia, ActionRemove, true},
		{TargetMedia, ActionRestore, true},
		{TargetMedia, ActionHide, false},
		{TargetMedia, ActionBan, false},
	}
	for _, tc := range cases {
		if got := ActionAllowed(tc.target, tc.action); got != tc.want {
			t.Errorf("ActionAllowed(%s, %s) = %v, want %v", tc.target, tc.action, got, tc.want)
		}
	}
}

// TestVocabularies pins the allowlists to their documented sources.
func TestVocabularies(t *testing.T) {
	// docs/11 §38's full list - media joined in Phase 9 with its table.
	for _, target := range TargetTypes() {
		if !ValidTargetType(target) {
			t.Errorf("TargetTypes() returned %q but ValidTargetType rejects it", target)
		}
	}
	if !ValidTargetType("media") {
		t.Error("media is reportable as of Phase 9 (docs/11 §38)")
	}
	if ValidTargetType("") || ValidTargetType("novels") {
		t.Error("unknown target types must be rejected")
	}

	// docs/01 §21's categories.
	for _, reason := range Reasons() {
		if !ValidReason(reason) {
			t.Errorf("Reasons() returned %q but ValidReason rejects it", reason)
		}
	}
	if ValidReason("dislike") || ValidReason("") {
		t.Error("unknown reasons must be rejected")
	}

	// docs/08 §24.1's lifecycle.
	for _, status := range Statuses() {
		if !ValidStatus(status) {
			t.Errorf("Statuses() returned %q but ValidStatus rejects it", status)
		}
	}
	if ValidStatus("open") {
		t.Error("unknown statuses must be rejected")
	}

	// docs/08 §24.2's examples: "no action" is rejecting the report, never an
	// audit row.
	if ValidAction("no_action") || ValidAction("") {
		t.Error("unknown actions must be rejected")
	}
}

// TestActionsFor keeps the UI vocabulary in sync with the matrix.
func TestActionsFor(t *testing.T) {
	for _, target := range TargetTypes() {
		actions := ActionsFor(TargetType(target))
		if len(actions) == 0 {
			t.Errorf("target %q offers no actions", target)
		}
		for _, action := range actions {
			if !ActionAllowed(TargetType(target), Action(action)) {
				t.Errorf("ActionsFor(%s) offers %q but ActionAllowed rejects it", target, action)
			}
		}
	}
}
