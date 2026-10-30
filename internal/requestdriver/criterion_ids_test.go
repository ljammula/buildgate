package requestdriver_test

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
)

// idlessSpec has criteria without IDs: structurally valid, so only the
// criterion check can refuse it as a drafted spec.
var idlessSpec = requestdrivertest.TwoCriteriaSpec

var labelledSpec = requestdrivertest.WithCriterionIDs(requestdrivertest.TwoCriteriaSpec)

// sequenceSpecRunner returns each of specs in turn, one per call, and the
// feedback file the call found.
func sequenceSpecRunner(dataDir string, specs []string, seen *[]string) requestdriver.SpecDraftRunner {
	call := 0
	return func(ctx context.Context, dd string, r *request.Request, cfg requestdriver.WorkerConfig) (string, *request.SpecEvidence, error) {
		b, _ := os.ReadFile(request.SpecFeedbackPath(dataDir, r.ID))
		feedback := string(b)
		*seen = append(*seen, feedback)
		i := call
		if i >= len(specs) {
			i = len(specs) - 1
		}
		call++
		return specs[i], &request.SpecEvidence{}, nil
	}
}

func driveSpec(t *testing.T, dataDir string, runner requestdriver.SpecDraftRunner) {
	t.Helper()
	if err := requestdrivertest.DriveRequests(newFakeDeps(t), context.Background(), dataDir, requestdriver.WorkerConfig{}, runner, requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
}

func TestADraftedSpecWithBadCriterionIDsGetsOneRedraft(t *testing.T) {
	dataDir := savedSpecDraftingRequest(t)
	var seen []string
	driveSpec(t, dataDir, sequenceSpecRunner(dataDir, []string{idlessSpec, labelledSpec}, &seen))

	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateSpecReview {
		t.Fatalf("State = %q, want spec_review (Error: %s)", loaded.State, loaded.Error)
	}
	if len(seen) != 2 {
		t.Fatalf("drafting job ran %d times, want 2", len(seen))
	}
	if seen[0] != "" || !strings.Contains(seen[1], "carry no IDs") {
		t.Errorf("feedback seen = %q, want none then the refusal in its own words", seen)
	}
	// The refusal fed the second attempt only: once a draft passes it is not
	// feedback for any later redraft.
	if len(loaded.Rejections) != 0 || request.SpecFeedback(loaded) != "" {
		t.Errorf("Rejections = %+v, feedback %q; want none once a draft passed", loaded.Rejections, request.SpecFeedback(loaded))
	}
	if got := string(mustReadFile(t, requestdriver.RequestSpecPath(dataDir, "req-1"))); got != labelledSpec {
		t.Errorf("spec.md is not the second draft: %q", got)
	}
}

func TestADraftedSpecWithBadCriterionIDsHaltsAfterTheSecondFailure(t *testing.T) {
	dataDir := savedSpecDraftingRequest(t)
	var seen []string
	driveSpec(t, dataDir, sequenceSpecRunner(dataDir, []string{idlessSpec}, &seen))

	halted := loadHalted(t, dataDir, "req-1")
	if len(seen) != 2 {
		t.Fatalf("drafting job ran %d times, want exactly 2", len(seen))
	}
	if !strings.Contains(halted.Error, "drafted spec.md is invalid") || halted.DraftHalt == nil {
		t.Errorf("Error = %q, DraftHalt = %+v; want an invalid-spec halt that leaves a note", halted.Error, halted.DraftHalt)
	}
}

func TestAnImportedSpecWithoutIDsReachesSpecReview(t *testing.T) {
	dataDir := t.TempDir()
	requestdrivertest.HandedOverRequest(t, dataDir, idlessSpec)
	var seen []string
	driveSpec(t, dataDir, sequenceSpecRunner(dataDir, []string{"never used"}, &seen))

	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateSpecReview || len(seen) != 0 || len(loaded.Rejections) != 0 {
		t.Fatalf("State = %q, job runs = %d, Rejections = %+v; want spec_review from the hand-over alone", loaded.State, len(seen), loaded.Rejections)
	}
}

// TestRealApprovedSpecsPassTheCriterionCheck: every spec of the corpus, which
// predates criterion IDs, passes the check as a handed-over spec does.
func TestRealApprovedSpecsPassTheCriterionCheck(t *testing.T) {
	for _, c := range loadPlanCorpus(t) {
		if err := request.ValidateSpecCriteria(c.spec, false); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

var (
	corpusCriterionRE = regexp.MustCompile(`^(\d+)([.)]\s+)(\S.*)$`)
	corpusCoveredRE   = regexp.MustCompile(`^(\s*[-*]\s*)(\d+)(\s*)$`)
)

// withCriterionIDs rewrites a corpus case the way a drafter that follows the
// current prompt writes it: "N. AC-N: text" in the spec and "- AC-N" in each
// ticket's covered list.
func withCriterionIDs(c planCorpusCase) planCorpusCase {
	section := ""
	var spec []string
	for _, line := range strings.Split(c.spec, "\n") {
		if strings.HasPrefix(line, "## ") {
			section = strings.TrimSpace(line)
		}
		if m := corpusCriterionRE.FindStringSubmatch(line); m != nil && section == "## Acceptance criteria" {
			line = m[1] + m[2] + "AC-" + m[1] + ": " + m[3]
		}
		spec = append(spec, line)
	}
	c.spec = strings.Join(spec, "\n")
	tickets := make([]requestdriver.DraftedTicket, len(c.tickets))
	for i, ticket := range c.tickets {
		section := ""
		var lines []string
		for _, line := range strings.Split(ticket.Content, "\n") {
			if strings.HasPrefix(line, "#") {
				section = strings.TrimSpace(line)
			}
			if m := corpusCoveredRE.FindStringSubmatch(line); m != nil && section == "### Acceptance criteria covered" {
				line = m[1] + "AC-" + m[2] + m[3]
			}
			lines = append(lines, line)
		}
		tickets[i] = requestdriver.DraftedTicket{Filename: ticket.Filename, Content: strings.Join(lines, "\n")}
	}
	c.tickets = tickets
	return c
}

// TestRealApprovedPlansWithIDsAddedPassEveryPlanTimeCheck: the corpus with
// criterion IDs added reaches plan_review as it did without them, and its
// spec passes the drafted-spec check.
func TestRealApprovedPlansWithIDsAddedPassEveryPlanTimeCheck(t *testing.T) {
	for _, c := range loadPlanCorpus(t) {
		t.Run(c.name, func(t *testing.T) {
			if _, listed := planCorpusRefused[c.name]; listed {
				t.Skip("known refusal")
			}
			c = withCriterionIDs(c)
			if err := request.ValidateSpecCriteria(c.spec, true); err != nil {
				t.Fatalf("spec with IDs added: %v", err)
			}
			if refusal := planCorpusRefusal(t, c); refusal != "" {
				t.Fatalf("a real approved plan with IDs added is refused: %s", refusal)
			}
		})
	}
}

// TestAScopedRedraftOfASpecWithoutIDsReachesSpecReview: a comment on Scope
// redrafts only that section, so the reviewed criteria stay as they are, ID-less.
func TestAScopedRedraftOfASpecWithoutIDsReachesSpecReview(t *testing.T) {
	original := requestdrivertest.TwoCriteriaSpec
	dataDir, r := rejectedSpecFixture(t, original, request.RejectionAnchor{Path: "spec.md", Section: "## Scope", Quote: "The /refunds endpoint only.", Note: "mention /refunds/{id}"})
	drafted := strings.Replace(rewriteSpecEverywhere(original), "REWRITTEN. The /refunds endpoint only.", "The /refunds and /refunds/{id} endpoints.", 1)
	advanceSpec(t, dataDir, r, drafted)
	loaded := requestdrivertest.LoadRequest(t, dataDir, "req-1")
	if loaded.State != request.StateSpecReview {
		t.Fatalf("State = %q (Error: %s), want spec_review", loaded.State, loaded.Error)
	}
	if got := string(mustReadFile(t, requestdriver.RequestSpecPath(dataDir, "req-1"))); !strings.Contains(got, "1. A retried POST") {
		t.Errorf("the reviewed criteria were not kept: %q", got)
	}
}

// TestAScopedRedraftOfTheCriteriaStillNeedsIDs: when the commented section is
// the criteria, they are the model's output and must carry IDs.
func TestAScopedRedraftOfTheCriteriaStillNeedsIDs(t *testing.T) {
	original := requestdrivertest.TwoCriteriaSpec
	dataDir, r := rejectedSpecFixture(t, original, request.RejectionAnchor{Path: "spec.md", Section: "## Acceptance criteria", Note: "tighten"})
	advanceSpec(t, dataDir, r, original)
	loaded := requestdrivertest.LoadRequest(t, dataDir, "req-1")
	if loaded.State != request.StateHalted || !strings.Contains(loaded.Error, "carry no IDs") {
		t.Fatalf("State = %q Error = %q, want a halt for IDs", loaded.State, loaded.Error)
	}
}
