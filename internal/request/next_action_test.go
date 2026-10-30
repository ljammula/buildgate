package request

import (
	"strings"
	"testing"

	"buildgate/internal/unresolved"
)

func TestHaltAcceptedNoPullRequestIsCalmAndClearedByRetry(t *testing.T) {
	r := New("req-1", "/w", "w", Source{Kind: SourceText}, fixedNow)
	r.State = StatePRReview
	r.prevState = StatePRReview
	r.Tickets = []Ticket{{Index: 1, RunID: "run-9"}}
	if err := r.HaltAcceptedNoPullRequest("ticket 1/1: run run-9 was accepted but no pull request was opened", fixedNow); err != nil {
		t.Fatal(err)
	}
	if !r.AwaitingPullRequest() || r.State != StateHalted {
		t.Fatalf("state %q kind %q", r.State, r.HaltKind)
	}
	if got := r.AwaitingPullRequestLabel(); !strings.Contains(got, "factoryd/run-9") || !strings.Contains(got, "factoryd retry req-1") {
		t.Errorf("label = %q", got)
	}
	if err := r.Retry("op", "", fixedNow); err != nil {
		t.Fatal(err)
	}
	if r.AwaitingPullRequest() || r.AwaitingPullRequestLabel() != "" {
		t.Errorf("retry must clear the marker: kind %q", r.HaltKind)
	}
}

// TestAwaitingPullRequestLabelUsesTheRunsRecordedBranch covers a
// -repository/Temporal-routed run's own real branch not being
// "factoryd/<run-id>" (see
// isolatedWorkspaceRunID's own doc comment, internal/workflow/workflow.go)
// -- once Ticket.Branch is recorded (request_driver.go's own
// advanceBuilding, at the moment a ticket's run reaches accepted), both
// the "merge by hand" hints must use IT, not the old guess.
func TestAwaitingPullRequestLabelUsesTheRunsRecordedBranch(t *testing.T) {
	r := New("req-1", "/w", "w", Source{Kind: SourceText}, fixedNow)
	r.State = StatePRReview
	r.prevState = StatePRReview
	r.Tickets = []Ticket{{Index: 1, RunID: "run-9", Branch: "factoryd/add-worstweekday-a1b2c3d4e5f6"}}
	if err := r.HaltAcceptedNoPullRequest("ticket 1/1: run run-9 was accepted but no pull request was opened", fixedNow); err != nil {
		t.Fatal(err)
	}
	label := r.AwaitingPullRequestLabel()
	if !strings.Contains(label, "factoryd/add-worstweekday-a1b2c3d4e5f6") {
		t.Errorf("AwaitingPullRequestLabel() = %q, want the recorded branch", label)
	}
	if strings.Contains(label, "factoryd/run-9") {
		t.Errorf("AwaitingPullRequestLabel() = %q, must not fall back to the guessed branch when the real one is recorded", label)
	}
	next := r.NextAction()
	if !strings.Contains(next, "factoryd/add-worstweekday-a1b2c3d4e5f6") {
		t.Errorf("NextAction() = %q, want the recorded branch", next)
	}
	if strings.Contains(next, "factoryd/run-9") {
		t.Errorf("NextAction() = %q, must not fall back to the guessed branch when the real one is recorded", next)
	}
}

// TestAwaitingPullRequestLabelFallsBackToGuessedBranchWithoutOne covers
// Ticket.Branch's own absence (a run predating that field, or with no
// isolated branch at all): the hints must still name SOMETHING usable,
// falling back to the old "factoryd/<run-id>" guess exactly as before
// the real-branch fix above.
func TestAwaitingPullRequestLabelFallsBackToGuessedBranchWithoutOne(t *testing.T) {
	r := New("req-1", "/w", "w", Source{Kind: SourceText}, fixedNow)
	r.State = StatePRReview
	r.prevState = StatePRReview
	r.Tickets = []Ticket{{Index: 1, RunID: "run-9"}}
	if err := r.HaltAcceptedNoPullRequest("ticket 1/1: run run-9 was accepted but no pull request was opened", fixedNow); err != nil {
		t.Fatal(err)
	}
	if got := r.AwaitingPullRequestLabel(); !strings.Contains(got, "factoryd/run-9") {
		t.Errorf("AwaitingPullRequestLabel() = %q, want the fallback guess", got)
	}
	if got := r.NextAction(); !strings.Contains(got, "factoryd/run-9") {
		t.Errorf("NextAction() = %q, want the fallback guess", got)
	}
}

// TestAwaitingPullRequestLabelNamesReleasePolicyDenialInstead covers the
// case where r.Error names a release-policy denial: the label must not
// suggest a bare retry (it will
// just be denied again against the same evidence) -- it must point at
// fixing the policy instead, or merging by hand.
func TestAwaitingPullRequestLabelNamesReleasePolicyDenialInstead(t *testing.T) {
	r := New("req-1", "/w", "w", Source{Kind: SourceText}, fixedNow)
	r.State = StatePRReview
	r.prevState = StatePRReview
	r.Tickets = []Ticket{{Index: 1, RunID: "run-9"}}
	reason := "ticket 1/1: run run-9 was accepted but no pull request was opened: denied by release policy (no rollback plan) -- fix the -release-* policy configuration first"
	if err := r.HaltAcceptedNoPullRequest(reason, fixedNow); err != nil {
		t.Fatal(err)
	}
	label := r.AwaitingPullRequestLabel()
	if strings.Contains(label, "with pull requests enabled") {
		t.Errorf("label = %q, must not suggest the plain -open-pull-request retry for a policy denial", label)
	}
	for _, want := range []string{"factoryd/run-9", "fix the release policy", "factoryd retry req-1"} {
		if !strings.Contains(label, want) {
			t.Errorf("label = %q, missing %q", label, want)
		}
	}
}

// TestNextActionCoversReviewStatesHaltsAndQuarantine covers NextAction's
// own per-state advice: review gates say approve/reject, a release
// policy denial never suggests a doomed retry, an ordinary halt/quarantine points at retry, and a
// state nothing is waiting on returns "".
func TestNextActionCoversReviewStatesHaltsAndQuarantine(t *testing.T) {
	for _, c := range []struct {
		name    string
		build   func() *Request
		want    []string
		wantNot []string
	}{
		{"spec_review", func() *Request {
			r := New("req-1", "/w", "w", Source{Kind: SourceText}, fixedNow)
			r.State = StateSpecReview
			return r
		}, []string{"factoryd approve req-1", "factoryd reject"}, nil},
		{"plan_review", func() *Request {
			r := New("req-1", "/w", "w", Source{Kind: SourceText}, fixedNow)
			r.State = StatePlanReview
			return r
		}, []string{"factoryd approve req-1", "factoryd reject"}, nil},
		{"quarantined", func() *Request {
			r := New("req-1", "/w", "w", Source{Kind: SourceText}, fixedNow)
			r.State = StateQuarantined
			r.Error = "boom"
			return r
		}, []string{"factoryd retry req-1"}, nil},
		{"quarantined by a retired spec_conformity check", func() *Request {
			r := New("req-1", "/w", "w", Source{Kind: SourceText}, fixedNow)
			r.State = StateQuarantined
			r.Error = "boom"
			r.QuarantineCheck = "spec_conformity"
			return r
		}, []string{"fix the cause named above", "factoryd retry req-1"}, []string{"spec_conformity quarantined"}},
		{"quarantined, code_review", func() *Request {
			r := New("req-1", "/w", "w", Source{Kind: SourceText}, fixedNow)
			r.State = StateQuarantined
			r.Error = "ticket 1/1: review corrective rounds exhausted (1/1)"
			r.QuarantineCheck = QuarantineCheckCodeReview
			return r
		}, []string{"code_review quarantined", "Code review findings", "factoryd retry req-1"}, []string{"fix the cause named above, then"}},
		{"resume_review, the review gave no verdict", func() *Request {
			r := New("req-1", "/w", "w", Source{Kind: SourceText}, fixedNow)
			r.State = StateBuilding
			if err := r.EnterResumeReview(StateBuilding, LostStep{RunID: "run-1", Cause: unresolved.ProviderQuota, RetryAt: "2026-10-11T03:11:00Z"}, fixedNow); err != nil {
				t.Fatal(err)
			}
			return r
		}, []string{"the model provider refused the requests of the building step", "it said to try again after 2026-10-11T03:11:00Z", "was not judged", "factoryd resume req-1", "kept worktree"}, []string{"quarantined", "factoryd retry"}},
		{"quarantined, budget_exhausted:request", func() *Request {
			r := New("req-1", "/w", "w", Source{Kind: SourceText}, fixedNow)
			r.State = StateQuarantined
			r.Error = "request spend $4.12 (412000 tokens) reached request_cost_budget_micro_usd 4000000"
			r.QuarantineCheck = QuarantineCheckBudgetRequest
			return r
		}, []string{"budget exhausted", "request_token_budget", "request_cost_budget_micro_usd", "factoryd retry req-1"}, nil},
		{"quarantined, budget_exhausted:monthly", func() *Request {
			r := New("req-1", "/w", "w", Source{Kind: SourceText}, fixedNow)
			r.State = StateQuarantined
			r.Error = "monthly spend $95.00 (9500000 tokens) reached monthly_token_budget 9000000"
			r.QuarantineCheck = QuarantineCheckBudgetMonthly
			return r
		}, []string{"budget exhausted", "monthly_token_budget", "monthly_cost_budget_micro_usd", "wait for next month", "factoryd retry req-1"}, nil},
		{"halted, release policy denial", func() *Request {
			r := New("req-1", "/w", "w", Source{Kind: SourceText}, fixedNow)
			r.State = StatePRReview
			r.prevState = StatePRReview
			r.Tickets = []Ticket{{Index: 1, RunID: "run-9"}}
			if err := r.HaltAcceptedNoPullRequest("denied by release policy (no rollback plan)", fixedNow); err != nil {
				t.Fatal(err)
			}
			return r
		}, []string{"factoryd/run-9", "fix the release policy", "factoryd retry req-1"}, []string{"with pull requests enabled"}},
		{"halted, plain", func() *Request {
			r := New("req-1", "/w", "w", Source{Kind: SourceText}, fixedNow)
			r.State = StateSpecDrafting
			r.prevState = StateSpecDrafting
			if err := r.Halt("boom", fixedNow); err != nil {
				t.Fatal(err)
			}
			return r
		}, []string{"factoryd retry req-1"}, nil},
		{"building: nothing waits on the operator", func() *Request {
			r := New("req-1", "/w", "w", Source{Kind: SourceText}, fixedNow)
			r.State = StateBuilding
			return r
		}, nil, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := c.build().NextAction()
			if len(c.want) == 0 && len(c.wantNot) == 0 && got != "" {
				t.Errorf("NextAction() = %q, want empty", got)
			}
			for _, want := range c.want {
				if !strings.Contains(got, want) {
					t.Errorf("NextAction() = %q, missing %q", got, want)
				}
			}
			for _, notWant := range c.wantNot {
				if strings.Contains(got, notWant) {
					t.Errorf("NextAction() = %q, must not contain %q", got, notWant)
				}
			}
		})
	}
}

// SpecFeedback and PlanFeedback are stage-scoped: each carries only the
// rejections of its own review.
func TestSpecFeedbackOnlyCarriesSpecStageRejections(t *testing.T) {
	r := New("req-1", "/w", "w", Source{Kind: SourceText}, fixedNow)
	r.Rejections = []Rejection{
		{By: "a", Reason: "spec reason", FromState: StateSpecReview},
		{By: "c", Reason: "plan reason", FromState: StatePlanReview},
	}
	fb := SpecFeedback(r)
	if !strings.Contains(fb, "spec reason") || strings.Contains(fb, "plan reason") {
		t.Errorf("SpecFeedback = %q", fb)
	}
}

func TestPlanFeedbackOnlyCarriesPlanStageRejections(t *testing.T) {
	r := New("req-1", "/w", "w", Source{Kind: SourceText}, fixedNow)
	r.Rejections = []Rejection{
		{By: "a", Reason: "spec reason", FromState: StateSpecReview},
		{By: "c", Reason: "plan reason", FromState: StatePlanReview},
	}
	fb := PlanFeedback(r)
	if !strings.Contains(fb, "plan reason") || strings.Contains(fb, "spec reason") {
		t.Errorf("PlanFeedback = %q", fb)
	}
}
