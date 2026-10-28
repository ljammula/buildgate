package requestdriver_test

import (
	"context"
	"strings"
	"testing"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
)

func TestBuildTicketRunArgsForwardsBugOnlyWhenSet(t *testing.T) {
	with := &requestdriver.QueueEntry{ID: "t1", Workspace: "/repo", SpecPath: "/repo/spec.md", VerifyCommand: "make verify", Bug: true}
	if !containsFlag(requestdriver.BuildTicketRunArgs("data", with, requestdriver.WorkerConfig{}), "-bug") {
		t.Error("a bug entry must forward -bug")
	}
	without := &requestdriver.QueueEntry{ID: "t2", Workspace: "/repo", SpecPath: "/repo/spec.md", VerifyCommand: "make verify"}
	if containsFlag(requestdriver.BuildTicketRunArgs("data", without, requestdriver.WorkerConfig{}), "-bug") {
		t.Error("an entry that is not a bug must not forward the flag")
	}
}

// Ticket 1 of a bug request is built with -bug, which keeps its Tests-Required
// opt-out from being honoured; a ticket of another request is not.
func TestTicketOneOfABugRequestIsBuiltWithTheBugFlag(t *testing.T) {
	for _, bug := range []bool{true, false} {
		dp := newFakeDeps(t)
		dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
		r := requestdrivertest.LoadRequest(t, dataDir, id)
		r.Bug = bug
		if err := r.Save(dataDir); err != nil {
			t.Fatal(err)
		}
		accept := requestdrivertest.AcceptingBuildRunner(t, dataDir)
		forwarded := false
		runner := func(ctx context.Context, args []string, onReady func(*run.Run)) error {
			forwarded = containsFlag(args, "-bug")
			return accept(ctx, args, onReady)
		}
		requestdrivertest.AdvanceBuildingOnce(dp, t, dataDir, id, requestdriver.ResumeGate{}, runner)
		if forwarded != bug {
			t.Errorf("bug request %v: -bug forwarded to ticket 1 = %v", bug, forwarded)
		}
	}
}

// A drafted plan whose ticket 1 of a bug request declares Tests-Required: no
// is refused at planning, naming ticket 1 and the header; the same plan for a
// request that is not a bug request reaches plan_review.
func TestAdvancePlanningRefusesABugTicketOneThatOptsOutOfTests(t *testing.T) {
	for _, bug := range []bool{true, false} {
		dp := newFakeDeps(t)
		dataDir, id := requestdrivertest.ApprovedPlanningFixture(t, requestdrivertest.TwoCriteriaSpec, "make verify")
		r := requestdrivertest.LoadRequest(t, dataDir, id)
		r.Bug = bug
		if err := r.Save(dataDir); err != nil {
			t.Fatal(err)
		}
		content := strings.Replace(requestdrivertest.ValidBrownfieldTicket("make verify", 1, 2), "Verify-Command: make verify\n", "Verify-Command: make verify\nTests-Required: no -- a rename\n", 1)
		runner, _ := requestdrivertest.StubPlanTicketsRunner([]requestdriver.DraftedTicket{{Filename: "001.spec.md", Content: content}}, &request.PlanEvidence{}, nil)
		if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), runner, requestdrivertest.FailingBuildRunner(t)); err != nil {
			t.Fatalf("driveRequests: %v", err)
		}
		got := requestdrivertest.LoadRequest(t, dataDir, id)
		if !bug {
			if got.State != request.StatePlanReview {
				t.Errorf("not a bug request: state %q (%s), want plan_review", got.State, got.Error)
			}
			continue
		}
		if got.State != request.StateHalted || !strings.Contains(got.Error, "ticket 1") || !strings.Contains(got.Error, "Tests-Required") {
			t.Errorf("bug request: state %q error %q, want a halt naming ticket 1 and Tests-Required", got.State, got.Error)
		}
	}
}
