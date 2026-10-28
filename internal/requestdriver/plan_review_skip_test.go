package requestdriver_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/sessionconfig"
)

// planOnce drives one planning pass over a request of TwoCriteriaSpec whose
// plan is the given number of tickets (one covers both criteria; two cover one
// each) and returns the request as saved.
func planOnce(t *testing.T, dataDir, id string, tickets int) *request.Request {
	t.Helper()
	drafted := []requestdriver.DraftedTicket{{Filename: "001.spec.md", Content: requestdrivertest.ValidBrownfieldTicket("make verify", 1, 2)}}
	if tickets == 2 {
		drafted = []requestdriver.DraftedTicket{
			{Filename: "001.spec.md", Content: requestdrivertest.ValidBrownfieldTicket("make verify", 1)},
			{Filename: "002.spec.md", Content: requestdrivertest.ValidBrownfieldTicket("make verify", 2)},
		}
	}
	return planDrafted(t, dataDir, id, drafted, requestdriver.WorkerConfig{})
}

// planDrafted is planOnce for the given drafted tickets and worker config.
func planDrafted(t *testing.T, dataDir, id string, drafted []requestdriver.DraftedTicket, cfg requestdriver.WorkerConfig) *request.Request {
	t.Helper()
	dp := newFakeDeps(t)
	runner, _ := requestdrivertest.StubPlanTicketsRunner(drafted, &request.PlanEvidence{}, nil)
	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), runner, requestdrivertest.FailingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	return requestdrivertest.LoadRequest(t, dataDir, id)
}

// A one-ticket plan nobody asked to review is approved by the factory in the
// planning step: building, the ticket pinned, the factory on the History entry,
// no plan_review reminder, and the human approval fields left empty.
func TestOneTicketPlanGoesStraightToBuilding(t *testing.T) {
	dataDir, id := requestdrivertest.ApprovedPlanningFixture(t, requestdrivertest.TwoCriteriaSpec, "make verify")
	requestdrivertest.PlanReviewOff(t, dataDir, id)
	before := requestdrivertest.LoadRequest(t, dataDir, id)

	r := planOnce(t, dataDir, id, 1)
	if r.State != request.StateBuilding || r.TicketCount != 1 {
		t.Fatalf("state %s (%s), tickets %d; want building with 1 ticket", r.State, r.Error, r.TicketCount)
	}
	if _, ok := r.ApprovedSHA256["tickets/001.spec.md"]; !ok {
		t.Errorf("ApprovedSHA256 = %v, want tickets/001.spec.md pinned", r.ApprovedSHA256)
	}
	if err := request.VerifyApprovedHashes(dataDir, r); err != nil {
		t.Errorf("the pinned hashes do not verify: %v", err)
	}
	last := r.History[len(r.History)-1]
	if last.From != request.StatePlanning || last.To != request.StateBuilding || last.By != "factory" || !strings.Contains(last.Reason, "plan review not requested") || !strings.Contains(last.Reason, "-plan-review") {
		t.Errorf("last History entry = %+v, want the factory's planning -> building naming how to opt in", last)
	}
	if r.ApprovedBy != before.ApprovedBy || r.ApprovedAt != before.ApprovedAt {
		t.Errorf("ApprovedBy/At = %q/%q, want the spec approval's %q/%q", r.ApprovedBy, r.ApprovedAt, before.ApprovedBy, before.ApprovedAt)
	}
	if n := requestdrivertest.CountNotificationLogLines(t, dataDir, id); n != 0 {
		t.Errorf("%d notification(s) sent, want none for a plan that skips review", n)
	}
}

func TestPlanOfSeveralTicketsWaitsAtPlanReview(t *testing.T) {
	dataDir, id := requestdrivertest.ApprovedPlanningFixture(t, requestdrivertest.TwoCriteriaSpec, "make verify")
	requestdrivertest.PlanReviewOff(t, dataDir, id)
	if r := planOnce(t, dataDir, id, 2); r.State != request.StatePlanReview {
		t.Fatalf("state %s (%s), want plan_review for 2 tickets", r.State, r.Error)
	}
}

func TestOptedInRequestWaitsAtPlanReview(t *testing.T) {
	dataDir, id := requestdrivertest.ApprovedPlanningFixture(t, requestdrivertest.TwoCriteriaSpec, "make verify")
	if !requestdrivertest.LoadRequest(t, dataDir, id).PlanReview {
		t.Fatal("fixture: want the request opted in")
	}
	if r := planOnce(t, dataDir, id, 1); r.State != request.StatePlanReview {
		t.Fatalf("state %s (%s), want plan_review for an opted-in request", r.State, r.Error)
	}
}

func TestRepositoryPlanReviewKeyWaitsAtPlanReview(t *testing.T) {
	dataDir, id := requestdrivertest.ApprovedPlanningFixture(t, requestdrivertest.TwoCriteriaSpec, "make verify")
	requestdrivertest.PlanReviewOff(t, dataDir, id)
	ws := requestdrivertest.LoadRequest(t, dataDir, id).Workspace
	if err := os.WriteFile(filepath.Join(ws, ".factory.yml"), []byte("verify_command: make verify\nplan_review: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := planOnce(t, dataDir, id, 1); r.State != request.StatePlanReview {
		t.Fatalf("state %s (%s), want plan_review under .factory.yml plan_review", r.State, r.Error)
	}
}

// A plan a person already sent back comes back to that person.
func TestAPlanAPersonSentBackWaitsAtPlanReview(t *testing.T) {
	dataDir, id := requestdrivertest.ApprovedPlanningFixture(t, requestdrivertest.TwoCriteriaSpec, "make verify")
	if r := planOnce(t, dataDir, id, 2); r.State != request.StatePlanReview {
		t.Fatalf("state %s (%s), want plan_review for 2 tickets", r.State, r.Error)
	}
	requestdrivertest.PlanReviewOff(t, dataDir, id)
	if _, err := request.Reject(dataDir, id, "alice", "merge them", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := planOnce(t, dataDir, id, 1); got.State != request.StatePlanReview {
		t.Fatalf("state %s (%s), want plan_review after a person's rejection", got.State, got.Error)
	}
}

// The same for a plan sent back from a halt (`reject -to plan`).
func TestAPlanSentBackFromAHaltWaitsAtPlanReview(t *testing.T) {
	dataDir, id := requestdrivertest.ApprovedPlanningFixture(t, requestdrivertest.TwoCriteriaSpec, "make verify")
	if r := planOnce(t, dataDir, id, 2); r.State != request.StatePlanReview {
		t.Fatalf("state %s (%s), want plan_review", r.State, r.Error)
	}
	requestdrivertest.PlanReviewOff(t, dataDir, id)
	if _, err := request.Approve(dataDir, id, "alice", time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	if err := requestdriver.HaltRequest(dataDir, requestdrivertest.LoadRequest(t, dataDir, id), "stopped", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := request.SendBack(dataDir, id, "alice", "one ticket please", request.SendBackToPlan, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := planOnce(t, dataDir, id, 1); got.State != request.StatePlanReview {
		t.Fatalf("state %s (%s), want plan_review after a send back", got.State, got.Error)
	}
}

// A refused draft's note does not outlive the retry whose plan the factory approves.
func TestRetriedOneTicketPlanClearsTheDraftHalt(t *testing.T) {
	dataDir, id := requestdrivertest.ApprovedPlanningFixture(t, requestdrivertest.TwoCriteriaSpec, "make verify")
	requestdrivertest.PlanReviewOff(t, dataDir, id)
	bad := []requestdriver.DraftedTicket{{Filename: "001.spec.md", Content: requestdrivertest.ValidBrownfieldTicket("make wrong-command-xyz", 1, 2)}}
	if r := planDrafted(t, dataDir, id, bad, requestdriver.WorkerConfig{}); r.State != request.StateHalted || r.DraftHalt == nil {
		t.Fatalf("state %s, DraftHalt %+v; want a halt with a note", r.State, r.DraftHalt)
	}
	if _, err := request.Retry(dataDir, id, "alice", "", time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	if r := planOnce(t, dataDir, id, 1); r.State != request.StateBuilding || r.DraftHalt != nil {
		t.Fatalf("state %s (%s), DraftHalt %+v; want building with no note", r.State, r.Error, r.DraftHalt)
	}
}

// The plan's review notes stay on the record of a plan the factory approved.
func TestOneTicketPlanKeepsItsReviewNotes(t *testing.T) {
	dataDir, id := requestdrivertest.ApprovedPlanningFixture(t, requestdrivertest.TwoCriteriaSpec, "make verify")
	requestdrivertest.PlanReviewOff(t, dataDir, id)
	big := requestdrivertest.ValidBrownfieldTicket("make verify", 1, 2)
	big = strings.Replace(big, "Required-Changed-Files: internal/payments/refunds.go\n", "Required-Changed-Files: internal/payments/refunds.go\nEstimated-Changed-Lines: 900\n", 1)
	cfg := requestdriver.WorkerConfig{Settings: sessionconfig.DefaultSettings()}
	cfg.Settings.PRBudgetLines = 400
	r := planDrafted(t, dataDir, id, []requestdriver.DraftedTicket{{Filename: "001.spec.md", Content: big}}, cfg)
	if r.State != request.StateBuilding {
		t.Fatalf("state %s (%s), want building", r.State, r.Error)
	}
	if len(r.ReviewNotes) != 1 || !strings.Contains(r.ReviewNotes[0], "estimates 900 changed lines, over the 400-line budget") {
		t.Errorf("ReviewNotes = %q, want the over-budget note", r.ReviewNotes)
	}
}
