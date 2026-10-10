package requestdriver_test

import (
	"context"
	"strings"
	"testing"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
)

// malformedCoverageTicket is a valid ticket whose "Acceptance criteria
// covered" list is prose, which the plan parser refuses.
func malformedCoverageTicket(t *testing.T) string {
	t.Helper()
	valid := requestdrivertest.ValidBrownfieldTicket("make verify", 1, 2)
	broken := strings.Replace(valid, "- 1\n- 2\n", "Criteria one and two.\n", 1)
	if broken == valid {
		t.Fatal("the valid ticket no longer lists its criteria as \"- 1\\n- 2\\n\"; update this fixture")
	}
	return broken
}

// TestAdvancePlanningRedraftsAMalformedPlanOnceWithTheParsersMessage: a
// drafted plan the parser refuses gets one more planning launch, told the
// parser's message, and a valid second plan reaches plan_review.
func TestAdvancePlanningRedraftsAMalformedPlanOnceWithTheParsersMessage(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.ApprovedPlanningFixture(t, requestdrivertest.TwoCriteriaSpec, "make verify")
	malformed := []requestdriver.DraftedTicket{{Filename: "001.spec.md", Content: malformedCoverageTicket(t)}}
	valid := []requestdriver.DraftedTicket{{Filename: "001.spec.md", Content: requestdrivertest.ValidBrownfieldTicket("make verify", 1, 2)}}
	runner, callCount, feedbackByCall := sequencedPlanTicketsRunner(t, dataDir,
		func() ([]requestdriver.DraftedTicket, *request.PlanEvidence, error) {
			return malformed, &request.PlanEvidence{}, nil
		},
		func() ([]requestdriver.DraftedTicket, *request.PlanEvidence, error) {
			return valid, &request.PlanEvidence{}, nil
		},
	)
	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), runner, requestdrivertest.FailingOracleDraftRunner(t), requestdrivertest.FailingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	if *callCount != 2 {
		t.Fatalf("planning launches = %d, want exactly 2", *callCount)
	}
	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StatePlanReview {
		t.Fatalf("State = %q (%s), want %q", loaded.State, loaded.Error, request.StatePlanReview)
	}
	if len(loaded.Rejections) != 1 || loaded.Rejections[0].By != "factoryd" {
		t.Fatalf("Rejections = %+v, want the factory's one refusal", loaded.Rejections)
	}
	reason := loaded.Rejections[0].Reason
	if !strings.Contains(reason, "001.spec.md") || !strings.Contains(reason, "Acceptance criteria covered") {
		t.Errorf("refusal = %q, want the parser's message naming the ticket and the section", reason)
	}
	if second := (*feedbackByCall)[1]; !strings.Contains(second, reason) {
		t.Errorf("second launch's plan-feedback.md = %q, want it to carry the parser's message %q", second, reason)
	}
}

// TestAdvancePlanningMalformedPlanTwiceHaltsAfterTwoLaunches: the redraft is
// bounded. A second malformed plan halts the request with the parser's
// message, after exactly two launches.
func TestAdvancePlanningMalformedPlanTwiceHaltsAfterTwoLaunches(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.ApprovedPlanningFixture(t, requestdrivertest.TwoCriteriaSpec, "make verify")
	malformed := func() ([]requestdriver.DraftedTicket, *request.PlanEvidence, error) {
		return []requestdriver.DraftedTicket{{Filename: "001.spec.md", Content: malformedCoverageTicket(t)}}, &request.PlanEvidence{}, nil
	}
	runner, callCount, _ := sequencedPlanTicketsRunner(t, dataDir, malformed, malformed)
	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), runner, requestdrivertest.FailingOracleDraftRunner(t), requestdrivertest.FailingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	if *callCount != 2 {
		t.Fatalf("planning launches = %d, want exactly 2 (one redraft, then a halt)", *callCount)
	}
	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateHalted {
		t.Fatalf("State = %q, want %q", loaded.State, request.StateHalted)
	}
	if !strings.Contains(loaded.Error, "Acceptance criteria covered") {
		t.Errorf("Error = %q, want the parser's message", loaded.Error)
	}
}
