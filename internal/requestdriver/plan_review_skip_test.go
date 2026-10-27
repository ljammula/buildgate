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
)

// planOnce drives one planning pass over a request of TwoCriteriaSpec whose
// plan is the given number of tickets (one covers both criteria; two cover one
// each) and returns the request as saved.
func planOnce(t *testing.T, dataDir, id string, tickets int) *request.Request {
	t.Helper()
	dp := newFakeDeps(t)
	drafted := []requestdriver.DraftedTicket{{Filename: "001.spec.md", Content: requestdrivertest.ValidBrownfieldTicket("make verify", 1, 2)}}
	if tickets == 2 {
		drafted = []requestdriver.DraftedTicket{
			{Filename: "001.spec.md", Content: requestdrivertest.ValidBrownfieldTicket("make verify", 1)},
			{Filename: "002.spec.md", Content: requestdrivertest.ValidBrownfieldTicket("make verify", 2)},
		}
	}
	runner, _ := requestdrivertest.StubPlanTicketsRunner(drafted, &request.PlanEvidence{}, nil)
	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), runner, requestdrivertest.FailingOracleDraftRunner(t), requestdrivertest.FailingBuildRunner(t)); err != nil {
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
	if last.From != request.StatePlanning || last.To != request.StateBuilding || last.By == "" || last.By == "alice" || !strings.Contains(last.Reason, "plan review not requested") || !strings.Contains(last.Reason, "-plan-review") {
		t.Errorf("last History entry = %+v, want the factory's planning -> building naming how to opt in", last)
	}
	if r.ApprovedBy != before.ApprovedBy || r.ApprovedAt != before.ApprovedAt {
		t.Errorf("ApprovedBy/At = %q/%q, want the spec approval's %q/%q", r.ApprovedBy, r.ApprovedAt, before.ApprovedBy, before.ApprovedAt)
	}
	if n := requestdrivertest.CountNotificationLogLines(t, dataDir, id); n != 0 {
		t.Errorf("%d notification(s) sent, want none for a plan that skips review", n)
	}
}

// The oracle files of the ticket are pinned with it, as a person's approval
// pins them, and a changed one stops the build the same way.
func TestOneTicketPlanPinsTheTicketsOracleFiles(t *testing.T) {
	dataDir, id := requestdrivertest.RequestOracleFixture(t, requestdrivertest.TwoOracleFiles("go test ./.oracle/...\n"))
	requestdrivertest.PlanReviewOff(t, dataDir, id)

	r := planOnce(t, dataDir, id, 1)
	if r.State != request.StateBuilding {
		t.Fatalf("state %s (%s), want building", r.State, r.Error)
	}
	var oracle []string
	for rel := range r.ApprovedSHA256 {
		if strings.HasPrefix(rel, "tickets/001.oracle/") {
			oracle = append(oracle, rel)
		}
	}
	if len(oracle) == 0 {
		t.Fatalf("ApprovedSHA256 = %v, want ticket 1's oracle files pinned", r.ApprovedSHA256)
	}
	if err := os.WriteFile(filepath.Join(request.Dir(dataDir, id), oracle[0]), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := request.VerifyApprovedHashes(dataDir, r); err == nil {
		t.Error("an oracle file edited after the factory's approval verifies, want a mismatch")
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
	r := requestdrivertest.LoadRequest(t, dataDir, id)
	r.PlanReview = false
	r.Rejections = append(r.Rejections, request.Rejection{By: "alice", At: time.Now().UTC().Format(time.RFC3339Nano), Reason: "split it", FromState: request.StatePlanReview})
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if got := planOnce(t, dataDir, id, 1); got.State != request.StatePlanReview {
		t.Fatalf("state %s (%s), want plan_review after a person's rejection", got.State, got.Error)
	}
}

// The fallback: a bug request needs ticket 1 to hold an oracle; a one-ticket
// plan without one is not approved by the factory but waits for a person.
func TestBugPlanWithoutAnOracleFallsBackToPlanReview(t *testing.T) {
	dataDir, id := requestdrivertest.ApprovedPlanningFixture(t, requestdrivertest.TwoCriteriaSpec, "make verify")
	r := requestdrivertest.LoadRequest(t, dataDir, id)
	r.PlanReview, r.Bug = false, true
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	got := planOnce(t, dataDir, id, 1)
	if got.State != request.StatePlanReview {
		t.Fatalf("state %s (%s), want plan_review", got.State, got.Error)
	}
	if _, pinned := got.ApprovedSHA256["tickets/001.spec.md"]; pinned {
		t.Errorf("ApprovedSHA256 = %v, want no ticket pinned by a refused factory approval", got.ApprovedSHA256)
	}
}
