package request

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const specWithDecision = `# Spec

## Problem

p

## Scope

The marker [NEEDS DECISION] quoted here is not an item.

## Non-goals

n

## Affected services and packages

a

## Acceptance criteria

1. c

## Risks

r

## Open questions

1. [NEEDS DECISION] Keep the sign or reject negative input. Recommended: keep it.
   Alternative 1: raise.
2. [NEEDS DECISION] Second choice.
`

func TestOpenDecisionsListsOnlyTheOpenQuestionsItems(t *testing.T) {
	got := OpenDecisions(specWithDecision)
	want := []string{
		"1. [NEEDS DECISION] Keep the sign or reject negative input. Recommended: keep it.",
		"2. [NEEDS DECISION] Second choice.",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("OpenDecisions = %q, want %q", got, want)
	}
	resolved := strings.Split(specWithDecision, "## Open questions")[0] + "## Open questions\n\nNone.\n"
	if got := OpenDecisions(resolved); len(got) != 0 {
		t.Errorf("OpenDecisions on a spec with none = %q, want none", got)
	}
}

// TestApproveRefusesASpecWithOpenDecisions: found live 2026-10-08, when a
// drafted spec asked for a decision and `factoryd approve` moved it to
// planning with the choice unanswered. The refusal names each item and the
// way to answer, and leaves the request in spec_review; once the spec has no
// item left the same approval goes through.
func TestApproveRefusesASpecWithOpenDecisions(t *testing.T) {
	dataDir := t.TempDir()
	newApprovableRequest(t, dataDir, "req-1", StateSpecReview, true)
	specPath := filepath.Join(Dir(dataDir, "req-1"), specFileName)
	if err := os.WriteFile(specPath, []byte(specWithDecision), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Approve(dataDir, "req-1", "alice", fixedNow, nil)
	if !errors.Is(err, ErrOpenDecisions) {
		t.Fatalf("Approve error = %v, want ErrOpenDecisions", err)
	}
	for _, want := range []string{"Keep the sign", "Second choice", "factoryd reject -reason", "req-1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not contain %q", err.Error(), want)
		}
	}
	r, loadErr := Load(dataDir, "req-1")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if r.State != StateSpecReview || len(r.ApprovedSHA256) != 0 {
		t.Errorf("after the refusal: state %q, approved hashes %v; want spec_review and none", r.State, r.ApprovedSHA256)
	}

	resolved := strings.Split(specWithDecision, "## Open questions")[0] + "## Open questions\n\nNone.\n"
	if err := os.WriteFile(specPath, []byte(resolved), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err = Approve(dataDir, "req-1", "alice", fixedNow, nil)
	if err != nil {
		t.Fatalf("Approve once the decisions are answered: %v", err)
	}
	if r.State != StatePlanning {
		t.Errorf("State = %q, want %q", r.State, StatePlanning)
	}
}

// TestOpenDecisionsItemShapes: what counts as an item and what is only a
// mention, from the shapes a drafted or hand-written spec uses.
func TestOpenDecisionsItemShapes(t *testing.T) {
	open := func(body string) int {
		return len(OpenDecisions("# Spec\n\n## Risks\n\nr\n\n## Open questions\n\n" + body))
	}
	cases := []struct {
		name string
		body string
		want int
	}{
		{"numbered", "1. [NEEDS DECISION] a\n", 1},
		{"bulleted and bold", "- **[NEEDS DECISION]** a\n", 1},
		{"sub-heading items, both counted", "### 1. [NEEDS DECISION] a\n\nRecommended: x\n\n### 2. [NEEDS DECISION] b\n", 2},
		{"item after a plain sub-heading", "### Decisions\n\n1. [NEEDS DECISION] a\n", 1},
		{"item after an issue reference line", "#123 is related.\n1. [NEEDS DECISION] a\n", 1},
		{"CRLF", "1. [NEEDS DECISION] a\r\n", 1},
		{"a mention that none remain", "None. No `[NEEDS DECISION]` items remain.\n", 0},
		{"a resolved note", "Resolved: [NEEDS DECISION] keep the sign -- kept.\n", 0},
		{"inside a fenced block", "```\n1. [NEEDS DECISION] a\n```\n", 0},
		{"after the section ends", "None.\n\n## Appendix\n\n1. [NEEDS DECISION] a\n", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := open(tc.body); got != tc.want {
				t.Errorf("open decisions = %d, want %d", got, tc.want)
			}
		})
	}
}
