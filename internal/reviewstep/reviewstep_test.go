package reviewstep

import "testing"

// TestPlanFoldsBothEnabledIntoOneCombinedStep exercises Plan's own
// central claim: enabling both spec-conformity and code review must
// produce ONE Step (CombinedStep), never both standalone ones -- see
// combined_review.py's own module doc comment for why (measured live
// a Flutter + Go app repo spend, 2026-09-28).
func TestPlanFoldsBothEnabledIntoOneCombinedStep(t *testing.T) {
	got := Plan(true, true)
	if len(got) != 1 {
		t.Fatalf("Plan(true, true) = %v, want exactly one step", got)
	}
	if got[0].Name != Combined {
		t.Errorf("Plan(true, true)[0].Name = %q, want %q", got[0].Name, Combined)
	}
	if got[0].ScriptName != CombinedScriptName {
		t.Errorf("Plan(true, true)[0].ScriptName = %q, want %q", got[0].ScriptName, CombinedScriptName)
	}
}

func TestPlanOnlyConformityEnabledReturnsTheStandaloneStep(t *testing.T) {
	got := Plan(true, false)
	if len(got) != 1 || got[0].Name != SpecConformity {
		t.Fatalf("Plan(true, false) = %v, want [SpecConformity]", got)
	}
	if got[0].ScriptName != conformityScriptNameFixture(t) {
		t.Errorf("Plan(true, false)[0].ScriptName = %q, want conformity_review.py", got[0].ScriptName)
	}
}

func TestPlanOnlyCodeReviewEnabledReturnsTheStandaloneStep(t *testing.T) {
	got := Plan(false, true)
	if len(got) != 1 || got[0].Name != CodeReview {
		t.Fatalf("Plan(false, true) = %v, want [CodeReview]", got)
	}
}

func TestPlanNeitherEnabledReturnsNoSteps(t *testing.T) {
	got := Plan(false, false)
	if len(got) != 0 {
		t.Fatalf("Plan(false, false) = %v, want no steps", got)
	}
}

// TestReviewStepsOrderIsSpecConformityThenCodeReview (M4-K3) already pins
// len(Steps) == 2 in internal/workflow/review_step_test.go -- Combined is
// deliberately never added to Steps (see ByName's own doc comment), so
// this asserts the same invariant here, next to the code that could
// regress it.
func TestCombinedStepIsNotInStepsTable(t *testing.T) {
	if len(Steps) != 2 {
		t.Fatalf("len(Steps) = %d, want 2 (Combined must not be added here)", len(Steps))
	}
	if _, ok := ByName(Combined); ok {
		t.Fatalf("ByName(Combined) found a step, want Combined to be absent from Steps")
	}
}

func TestGateExitCodesDecodesBothBitsIndependently(t *testing.T) {
	cases := []struct {
		exit                           int
		wantConformity, wantCodeReview int
	}{
		{CombinedExitBase + 0, 0, 0},
		{CombinedExitBase + 1, 1, 0},
		{CombinedExitBase + 2, 0, 1},
		{CombinedExitBase + 3, 1, 1},
		// Anything else -- a crash or unrecognized status -- must never
		// read as a pass on either gate. 1 is Python's uncaught-exception
		// status (the live ModuleNotFoundError case, a Flutter + Go app repo M-E2 run,
		// 2026-09-28); 0 and 2/3 are no longer valid combined statuses.
		{0, 1, 1},
		{1, 1, 1},
		{2, 1, 1},
		{3, 1, 1},
		{CombinedExitBase + 4, 1, 1},
		{-1, 1, 1},
		{255, 1, 1},
	}
	for _, c := range cases {
		gotConformity, gotCodeReview := GateExitCodes(c.exit)
		if gotConformity != c.wantConformity || gotCodeReview != c.wantCodeReview {
			t.Errorf("GateExitCodes(%d) = (%d, %d), want (%d, %d)", c.exit, gotConformity, gotCodeReview, c.wantConformity, c.wantCodeReview)
		}
	}
}

func TestCombinedArgsIncludesBothHostPathsAndOmitsEmptyOptionals(t *testing.T) {
	got := CombinedArgs("/path/combined_review.py", "/ws", "/ws/ticket.md", "/ws/criteria.md", "required", "advisory", "", "", "pi")
	want := []string{
		"/path/combined_review.py",
		"--workspace", "/ws",
		"--spec", "/ws/ticket.md",
		"--spec-acceptance-criteria", "/ws/criteria.md",
		"--conformity-policy", "required",
		"--review-policy", "advisory",
		"--harness", "pi",
	}
	if len(got) != len(want) {
		t.Fatalf("CombinedArgs() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("CombinedArgs()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestCombinedArgsIncludesBaseSHAAndThinkingWhenSet(t *testing.T) {
	got := CombinedArgs("/path/combined_review.py", "/ws", "/ws/ticket.md", "/ws/criteria.md", "required", "required", "deadbeef", "max", "pi")
	want := []string{
		"/path/combined_review.py",
		"--workspace", "/ws",
		"--spec", "/ws/ticket.md",
		"--spec-acceptance-criteria", "/ws/criteria.md",
		"--conformity-policy", "required",
		"--review-policy", "required",
		"--review-base-sha", "deadbeef",
		"--thinking", "max",
		"--harness", "pi",
	}
	if len(got) != len(want) {
		t.Fatalf("CombinedArgs() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("CombinedArgs()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func conformityScriptNameFixture(t *testing.T) string {
	t.Helper()
	step, ok := ByName(SpecConformity)
	if !ok {
		t.Fatalf("ByName(SpecConformity) not found")
	}
	return step.ScriptName
}

func TestCombinedArgsPassesHarnessWhenSet(t *testing.T) {
	got := CombinedArgs("/path/combined_review.py", "/ws", "/ws/ticket.md", "/ws/criteria.md", "required", "advisory", "", "", "pifork")
	if n := len(got); n < 2 || got[n-2] != "--harness" || got[n-1] != "pifork" {
		t.Fatalf("CombinedArgs() = %v, want it to end with --harness pifork", got)
	}
}
