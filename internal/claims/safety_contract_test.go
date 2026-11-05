package claims

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"buildgate/internal/run"
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

	for i := 1; i <= 22; i++ {
		id := regexp.QuoteMeta(fmtInvariantID(i))
		if !regexp.MustCompile(`(?m)^- \*\*` + id + ` —`).MatchString(contract) {
			t.Errorf("safety contract is missing required invariant SC-%03d", i)
		}
	}

	// The request lifecycle's edges (internal/request has no table of its
	// own yet; each is checked to be present).
	requiredTransitions := [][2]string{
		{"quarantined", "cancelled"},
		{"halted", "cancelled"},
		{"submitted", "cancelled"},
		{"spec_drafting", "cancelled"},
		{"spec_review", "cancelled"},
		{"planning", "cancelled"},
		{"plan_review", "cancelled"},
		{"building", "cancelled"},
		{"pr_review", "cancelled"},
		{"spec_drafting", "resume_review"},
		{"planning", "resume_review"},
		{"building", "resume_review"},
		{"pr_review", "resume_review"},
		{"resume_review", "spec_drafting"},
		{"resume_review", "planning"},
		{"resume_review", "building"},
		{"resume_review", "pr_review"},
		{"resume_review", "cancelled"},
		{"pr_review", "closed"},
		{"spec_review", "halted"},
		{"plan_review", "halted"},
		{"resume_review", "halted"},
		{"halted", "spec_review"},
		{"halted", "plan_review"},
		{"planning", "building"},
		{"building", "planning"},
		{"halted", "resume_review"},
	}
	for _, transition := range requiredTransitions {
		line := "- `" + transition[0] + " -> " + transition[1] + "`"
		if !regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(line) + `$`).MatchString(contract) {
			t.Errorf("safety contract is missing required transition %q", line)
		}
	}
}

// TestSafetyContractRunGraphIsTheCodes checks the contract's "Run state
// graph" edge list against run.Transitions() in both directions: an edge the
// code allows and the contract omits, or one the contract lists and the code
// does not have, fails.
func TestSafetyContractRunGraphIsTheCodes(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile(filepath.Join(findRepoRoot(t), "safety-contract.md"))
	if err != nil {
		t.Fatalf("read safety contract: %v", err)
	}
	contract := string(b)
	start := strings.Index(contract, "\n### Run state graph\n")
	if start < 0 {
		t.Fatal(`safety contract has no "### Run state graph" section`)
	}
	section := contract[start+1:]
	if end := strings.Index(section[1:], "\n### "); end >= 0 {
		section = section[:end+1]
	}
	documented := map[string]bool{}
	for _, m := range regexp.MustCompile("(?m)^- `([a-z_]+) -> ([a-z_]+)`$").FindAllStringSubmatch(section, -1) {
		documented[m[1]+" -> "+m[2]] = true
	}
	code := map[string]bool{}
	for _, tr := range run.Transitions() {
		code[string(tr.From)+" -> "+string(tr.To)] = true
	}
	for edge := range code {
		if !documented[edge] {
			t.Errorf("run.Transitions() has %q; the contract's run state graph does not list it", edge)
		}
	}
	for edge := range documented {
		if !code[edge] {
			t.Errorf("the contract's run state graph lists %q; run.Transitions() does not have it", edge)
		}
	}
}

func fmtInvariantID(number int) string {
	return fmt.Sprintf("SC-%03d", number)
}
