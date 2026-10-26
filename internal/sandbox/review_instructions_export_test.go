package sandbox

import "testing"

func TestIsInstructionPathMatchesTheReviewTableAtAnyDepth(t *testing.T) {
	for p, want := range map[string]bool{
		"AGENTS.md":                       true,
		"pkg/agents.md":                   true,
		"pkg/.GitHub/Instructions/x.md":   true,
		".claude/skills/a/SKILL.md":       true,
		"sub/.mcp.json":                   true,
		"internal/foo/foo_test.go":        false,
		"tests/test_agents.py":            false,
		".github/workflows/ci.yml":        false,
		"src/__tests__/claude.md.test.ts": false,
	} {
		if got := IsInstructionPath(p); got != want {
			t.Errorf("IsInstructionPath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestFoldPathFoldsCaseAndCompatibilityForms(t *testing.T) {
	if FoldPath("Src/Straße_Test.go") != FoldPath("src/STRASSE_test.go") {
		t.Errorf("FoldPath does not fold the sharp s and case alike")
	}
	if FoldPath("a/b_test.go") == FoldPath("a/c_test.go") {
		t.Errorf("FoldPath folds distinct paths alike")
	}
}
