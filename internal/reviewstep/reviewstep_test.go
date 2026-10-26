package reviewstep

import (
	"slices"
	"testing"
)

func TestPlanReturnsTheOneCombinedStepWhenReviewIsOn(t *testing.T) {
	got := Plan(true)
	if len(got) != 1 {
		t.Fatalf("Plan(true) = %v, want exactly one step", got)
	}
	if got[0].Name != Combined {
		t.Errorf("Plan(true)[0].Name = %q, want %q", got[0].Name, Combined)
	}
	if got[0].ScriptName != CombinedScriptName {
		t.Errorf("Plan(true)[0].ScriptName = %q, want %q", got[0].ScriptName, CombinedScriptName)
	}
}

func TestPlanReviewOffReturnsNoSteps(t *testing.T) {
	if got := Plan(false); len(got) != 0 {
		t.Fatalf("Plan(false) = %v, want no steps", got)
	}
}

func TestReviewExitCodeDecodesAVerdictAndNothingElseAsAPass(t *testing.T) {
	cases := []struct {
		exit, want int
	}{
		{CombinedExitBase, 0},
		{CombinedExitBase + 2, 1},
		// Anything else -- a crash or unrecognized status -- must never read
		// as a pass. 1 is Python's uncaught-exception status (a
		// ModuleNotFoundError at import once recorded a pass); bit 0 is no
		// longer ever set.
		{CombinedExitBase + 1, 1},
		{CombinedExitBase + 3, 1},
		{0, 1},
		{1, 1},
		{2, 1},
		{3, 1},
		{CombinedExitBase + 4, 1},
		{-1, 1},
		{255, 1},
	}
	for _, c := range cases {
		if got := ReviewExitCode(c.exit); got != c.want {
			t.Errorf("ReviewExitCode(%d) = %d, want %d", c.exit, got, c.want)
		}
	}
}

func TestCombinedArgsIncludesApprovedSpecWhenSetAndOmitsEmptyOptionals(t *testing.T) {
	got := CombinedArgs("/path/combined_review.py", "/ws", "/ws/ticket.md", "/ws/spec.md", "advisory", "", "", "pi")
	want := []string{
		"/path/combined_review.py",
		"--workspace", "/ws",
		"--spec", "/ws/ticket.md",
		"--approved-spec", "/ws/spec.md",
		"--review-policy", "advisory",
		"--harness", "pi",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("CombinedArgs() = %v, want %v", got, want)
	}
	got = CombinedArgs("/path/combined_review.py", "/ws", "/ws/ticket.md", "", "advisory", "", "", "pi")
	if slices.Contains(got, "--approved-spec") {
		t.Fatalf("CombinedArgs() = %v, want no --approved-spec for an empty path", got)
	}
}

func TestCombinedArgsIncludesBaseSHAAndThinkingWhenSet(t *testing.T) {
	got := CombinedArgs("/path/combined_review.py", "/ws", "/ws/ticket.md", "/ws/spec.md", "required", "deadbeef", "max", "pi")
	want := []string{
		"/path/combined_review.py",
		"--workspace", "/ws",
		"--spec", "/ws/ticket.md",
		"--approved-spec", "/ws/spec.md",
		"--review-policy", "required",
		"--review-base-sha", "deadbeef",
		"--thinking", "max",
		"--harness", "pi",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("CombinedArgs() = %v, want %v", got, want)
	}
}

func TestCombinedArgsPassesHarnessWhenSet(t *testing.T) {
	got := CombinedArgs("/path/combined_review.py", "/ws", "/ws/ticket.md", "", "advisory", "", "", "pifork")
	if n := len(got); n < 2 || got[n-2] != "--harness" || got[n-1] != "pifork" {
		t.Fatalf("CombinedArgs() = %v, want it to end with --harness pifork", got)
	}
}
