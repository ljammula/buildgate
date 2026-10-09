package claims

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestSafetyContractContainsRequiredControls keeps the standalone Phase 0
// artifact reviewable: removing a required section, invariant, or state edge
// fails the same broad verification that checks CLAIMS.md test references.
// This validates the contract's shape, not runtime enforcement; each
// invariant's implementation status is deliberately recorded in the artifact.
func TestSafetyContractContainsRequiredControls(t *testing.T) {
	t.Parallel()
	root := findRepoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "safety-contract.md"))
	if err != nil {
		t.Fatalf("read safety contract: %v", err)
	}
	contract := string(b)

	sections := []string{
		"Scope and trust boundaries",
		"Threat model",
		"Safety invariants",
		"State transitions",
		"Gate-retirement conditions",
		"Evidence and approval record",
	}
	for _, section := range sections {
		if !regexp.MustCompile(`(?m)^## ` + regexp.QuoteMeta(section) + `$`).MatchString(contract) {
			t.Errorf("safety contract is missing required section %q", section)
		}
	}

	for i := 1; i <= 18; i++ {
		id := regexp.QuoteMeta(fmtInvariantID(i))
		if !regexp.MustCompile(`(?m)^- \*\*` + id + ` —`).MatchString(contract) {
			t.Errorf("safety contract is missing required invariant SC-%03d", i)
		}
	}

	requiredTransitions := [][2]string{
		{"draft", "product_policy_check"},
		{"product_policy_check", "architecture_policy_check"},
		{"architecture_policy_check", "program_design_policy_check"},
		{"program_design_policy_check", "ready"},
		{"ready", "slice_running"},
		{"slice_running", "verifying"},
		{"verifying", "slice_policy_check"},
		{"slice_policy_check", "accepted"},
		{"accepted", "next_slice"},
		{"next_slice", "slice_running"},
		{"any execution", "halted"},
		{"halted", "quarantined"},
		{"quarantined", "resumed"},
		{"accepted", "merge_policy_check"},
		{"merge_policy_check", "decision_recorded"},
		{"quarantined", "cancelled"},
		{"halted", "cancelled"},
		{"spec_drafting", "resume_review"},
		{"oracle_drafting", "resume_review"},
		{"planning", "resume_review"},
		{"building", "resume_review"},
		{"pr_review", "resume_review"},
		{"resume_review", "spec_drafting"},
		{"resume_review", "oracle_drafting"},
		{"resume_review", "planning"},
		{"resume_review", "building"},
		{"resume_review", "pr_review"},
		{"resume_review", "cancelled"},
	}
	for _, transition := range requiredTransitions {
		line := "- `" + transition[0] + " -> " + transition[1] + "`"
		if !regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(line) + `$`).MatchString(contract) {
			t.Errorf("safety contract is missing required transition %q", line)
		}
	}
}

func fmtInvariantID(number int) string {
	return fmt.Sprintf("SC-%03d", number)
}
