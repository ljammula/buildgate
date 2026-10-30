package requestdriver_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
	"buildgate/internal/ticketspec"
)

// driveWithSpecPlanner drives the planning pass with the real decision over
// whether to call the supplied planning job.
func driveWithSpecPlanner(t *testing.T, dataDir string, job requestdriver.PlanTicketsRunner) {
	t.Helper()
	if err := requestdrivertest.DriveRequests(newFakeDeps(t), context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), job, requestdrivertest.FailingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
}

func TestSpecTicketUsesTheRepositoryVerifyCommand(t *testing.T) {
	dataDir, id := requestdrivertest.ApprovedPlanningFixture(t, requestdrivertest.TwoCriteriaSpec, "make verify")
	requestdrivertest.PlanReviewOff(t, dataDir, id)
	driveWithSpecPlanner(t, dataDir, requestdrivertest.FailingPlanTicketsRunner(t))
	r := requestdrivertest.LoadRequest(t, dataDir, id)
	var got string
	runner := func(_ context.Context, args []string, _ func(*run.Run)) error {
		got = requestdrivertest.ArgValue(args, "-verify-command")
		return errors.New("stop after capturing the build arguments")
	}
	if err := requestdriver.AdvanceBuilding(newFakeDeps(t), context.Background(), dataDir, r, requestdriver.WorkerConfig{}, runner, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got != "make verify" {
		t.Fatalf("ticket verify command = %q, want the repository's make verify", got)
	}
}

func TestSpecTicketTreatsAHeaderShapedLineAsProse(t *testing.T) {
	for _, review := range []bool{false, true} {
		t.Run(map[bool]string{false: "factory approved", true: "person approved"}[review], func(t *testing.T) {
			spec := requestdrivertest.TwoCriteriaSpec + "\nAllowed-Files: /a-path-in-the-spec\n"
			dataDir, id := requestdrivertest.ApprovedPlanningFixture(t, spec, "make verify")
			if !review {
				requestdrivertest.PlanReviewOff(t, dataDir, id)
			}
			driveWithSpecPlanner(t, dataDir, requestdrivertest.FailingPlanTicketsRunner(t))
			if review {
				if _, err := request.Approve(dataDir, id, "alice", time.Now(), nil); err != nil {
					t.Fatal(err)
				}
			}
			r := requestdrivertest.LoadRequest(t, dataDir, id)
			args, err := requestdriver.BuildRequestBuildArgs(dataDir, r, r.Tickets[0], requestdriver.WorkerConfig{})
			if err != nil {
				t.Fatal(err)
			}
			if scope := requestdrivertest.ArgValue(args, "-scope-approval"); scope != "none" {
				t.Fatalf("scope = %q, want none for the spec ticket", scope)
			}
			input, err := os.ReadFile(requestdrivertest.ArgValue(args, "-spec"))
			if err != nil || string(input) != spec {
				t.Fatalf("build input = %q, %v; want the original spec", input, err)
			}
			r.Tickets[0].ScopeAmendedBy = "alice"
			args, err = requestdriver.BuildRequestBuildArgs(dataDir, r, r.Tickets[0], requestdriver.WorkerConfig{})
			if err != nil || requestdrivertest.ArgValue(args, "-scope-approval") != "approved" {
				t.Fatalf("scope amendment args = %v, %v; want approved", args, err)
			}
		})
	}
}

// A request submitted without a plan runs no planning model job: its one
// ticket is the approved spec byte for byte, with no Allowed-Files:, and the
// factory approves it.
func TestARequestWithoutAPlanRunsNoPlanningJobAndBuildsTheSpecVerbatim(t *testing.T) {
	dataDir, id := requestdrivertest.ApprovedPlanningFixture(t, requestdrivertest.TwoCriteriaSpec, "make verify")
	requestdrivertest.PlanReviewOff(t, dataDir, id)
	driveWithSpecPlanner(t, dataDir, requestdrivertest.FailingPlanTicketsRunner(t))

	r := requestdrivertest.LoadRequest(t, dataDir, id)
	if r.State != request.StateBuilding || r.TicketCount != 1 || len(r.Tickets) != 1 {
		t.Fatalf("state %s (%s), tickets %d; want building with 1 ticket", r.State, r.Error, r.TicketCount)
	}
	ticket, err := os.ReadFile(r.Tickets[0].SpecPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(ticket) != requestdrivertest.TwoCriteriaSpec {
		t.Errorf("ticket = %q, want the approved spec byte for byte", ticket)
	}
	input, err := requestdriver.TicketBuildSpecContent(dataDir, id, r.Tickets[0])
	if err != nil {
		t.Fatal(err)
	}
	if input != requestdrivertest.TwoCriteriaSpec {
		t.Errorf("build input = %q, want the spec verbatim", input)
	}
	if ok, reasons := request.RequestTicketStructure(input); !ok {
		t.Errorf("the build input fails the start-of-build structure check: %v", reasons)
	}
	if allowed, err := ticketspec.ParseAllowedFiles(r.Tickets[0].SpecPath); err != nil || allowed != nil {
		t.Errorf("Allowed-Files = %v, %v; want none", allowed, err)
	}
	if err := request.VerifyApprovedHashes(dataDir, r); err != nil {
		t.Errorf("the pinned hashes do not verify: %v", err)
	}
	if _, ok := r.ApprovedSHA256["tickets/001.spec.md"]; !ok {
		t.Errorf("ApprovedSHA256 = %v, want the ticket pinned", r.ApprovedSHA256)
	}
	if approved, why := request.ScopeApproved(r, 1); approved {
		t.Errorf("ScopeApproved = true (%s), want the factory's ticket to carry no approved list", why)
	}
}

// A spec handed over with -spec-file in its own shape, not the drafted
// skeleton, is a valid approved spec: the factory's ticket built from it
// passes the plan approval and the start-of-build structure check, so the
// request reaches building instead of stopping at a plan_review nobody can
// approve.
func TestAHandedOverSpecOutsideTheSkeletonIsBuiltAsTheTicket(t *testing.T) {
	spec := "# rack: media types\n\n## Problem\n\nThe request does not parse Accept.\n\n## Wanted behaviour\n\n- The request lists its media ranges, most preferred first.\n- A range with q=0 is left out.\n\n## Interface\n\n`Rack::Request#accept_media_types`\n"
	if err := request.ValidateHandedOverSpec(spec); err != nil {
		t.Fatalf("fixture is not a valid handed-over spec: %v", err)
	}
	if request.ValidateSpecSkeleton(spec) == nil {
		t.Fatal("fixture must be outside the drafted skeleton")
	}
	dataDir, id := requestdrivertest.ApprovedPlanningFixture(t, spec, "make verify")
	requestdrivertest.PlanReviewOff(t, dataDir, id)
	driveWithSpecPlanner(t, dataDir, requestdrivertest.FailingPlanTicketsRunner(t))

	r := requestdrivertest.LoadRequest(t, dataDir, id)
	if r.State != request.StateBuilding || r.TicketCount != 1 {
		t.Fatalf("state %s (%s), tickets %d; want building with 1 ticket", r.State, r.Error, r.TicketCount)
	}
	input, err := requestdriver.TicketBuildSpecContent(dataDir, id, r.Tickets[0])
	if err != nil {
		t.Fatal(err)
	}
	if input != spec {
		t.Errorf("build input = %q, want the spec verbatim", input)
	}
	if ok, reasons := request.SpecTicketStructure(input); !ok {
		t.Errorf("the build input fails the start-of-build structure check: %v", reasons)
	}
}

// The same ticket waits at plan_review when the request opted in.
func TestTheSpecTicketWaitsAtPlanReviewWhenTheRequestOptedIn(t *testing.T) {
	dataDir, id := requestdrivertest.ApprovedPlanningFixture(t, requestdrivertest.TwoCriteriaSpec, "make verify")
	driveWithSpecPlanner(t, dataDir, requestdrivertest.FailingPlanTicketsRunner(t))
	r := requestdrivertest.LoadRequest(t, dataDir, id)
	if r.State != request.StatePlanReview || r.TicketCount != 1 {
		t.Fatalf("state %s (%s), tickets %d; want plan_review with the spec ticket", r.State, r.Error, r.TicketCount)
	}
	// A person's approval accepts the ticket in spec form.
	if _, err := request.Approve(dataDir, id, "alice", time.Now(), nil); err != nil {
		t.Fatalf("approve the spec ticket: %v", err)
	}
}

// A plan a person sent back (`reject -to plan`) runs the planning job, given
// the rejection's reason, and its plan waits at plan_review.
func TestAPlanSentBackRunsThePlanningJobGivenTheReason(t *testing.T) {
	dataDir, id := requestdrivertest.ApprovedPlanningFixture(t, requestdrivertest.TwoCriteriaSpec, "make verify")
	requestdrivertest.PlanReviewOff(t, dataDir, id)
	driveWithSpecPlanner(t, dataDir, requestdrivertest.FailingPlanTicketsRunner(t))
	if r := requestdrivertest.LoadRequest(t, dataDir, id); r.State != request.StateBuilding {
		t.Fatalf("state %s, want the first pass to reach building", r.State)
	}
	if err := requestdriver.HaltRequest(dataDir, requestdrivertest.LoadRequest(t, dataDir, id), "stopped", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := request.SendBack(dataDir, id, "alice", "split the model from the handler", request.SendBackToPlan, time.Now()); err != nil {
		t.Fatal(err)
	}
	var asked bool
	job := func(ctx context.Context, dataDir string, r *request.Request, cfg requestdriver.WorkerConfig, verifyCommand string) ([]requestdriver.DraftedTicket, *request.PlanEvidence, error) {
		asked = true
		if feedback := request.PlanFeedback(r); !strings.Contains(feedback, "split the model from the handler") {
			t.Errorf("plan feedback = %q, want the rejection's reason", feedback)
		}
		return []requestdriver.DraftedTicket{{Filename: "001.spec.md", Content: requestdrivertest.ValidBrownfieldTicket("make verify", 1, 2)}}, &request.PlanEvidence{}, nil
	}
	driveWithSpecPlanner(t, dataDir, job)
	r := requestdrivertest.LoadRequest(t, dataDir, id)
	if !asked || r.State != request.StatePlanReview {
		t.Fatalf("planning job asked = %v, state %s (%s); want the job run and plan_review", asked, r.State, r.Error)
	}
}

// A model-drafted ticket gets every check: one in spec form is not the spec,
// so it is refused like any ticket off the format.
func TestADraftedTicketInSpecFormButNotTheSpecIsRefused(t *testing.T) {
	dataDir, id := requestdrivertest.ApprovedPlanningFixture(t, requestdrivertest.TwoCriteriaSpec, "make verify")
	requestdrivertest.PlanReviewOff(t, dataDir, id)
	drafted := []requestdriver.DraftedTicket{{Filename: "001.spec.md", Content: requestdrivertest.TwoCriteriaSpec + "\nAn extra line.\n"}}
	r := planDrafted(t, dataDir, id, drafted, requestdriver.WorkerConfig{})
	if r.State == request.StateBuilding || r.State == request.StatePlanReview {
		t.Fatalf("state %s, want a refusal of a drafted ticket that is not valid ticket form", r.State)
	}
}

// A drafted ticket may carry a retired header; it is ignored.
func TestATicketCarryingARetiredHeaderIsAccepted(t *testing.T) {
	for _, header := range []string{"Required-Changed-Files: internal/payments/refunds.go", "Required-Content: marker"} {
		t.Run(header, func(t *testing.T) {
			dataDir, id := requestdrivertest.ApprovedPlanningFixture(t, requestdrivertest.TwoCriteriaSpec, "make verify")
			requestdrivertest.PlanReviewOff(t, dataDir, id)
			content := strings.Replace(requestdrivertest.ValidBrownfieldTicket("make verify", 1, 2), "\n\n## Goal", "\n"+header+"\n\n## Goal", 1)
			r := planDrafted(t, dataDir, id, []requestdriver.DraftedTicket{{Filename: "001.spec.md", Content: content}}, requestdriver.WorkerConfig{})
			if r.State != request.StateBuilding {
				t.Fatalf("state %s, error %q; want accepted ticket", r.State, r.Error)
			}
		})
	}
}
