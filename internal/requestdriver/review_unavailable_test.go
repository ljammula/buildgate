package requestdriver

import (
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/run"
)

func reviewRun(verdicts []run.ReviewVerdict, review *run.CodeReviewResult, failed ...string) *run.Run {
	r := &run.Run{SpecConformityVerdicts: verdicts, CodeReview: review}
	for _, check := range failed {
		r.GateResults = append(r.GateResults, run.GateResult{Check: check, Passed: false})
	}
	return r
}

// A review that timed out answers no criterion: every verdict is
// "unavailable" and the code reviewer is not available. That is no finding
// to fix, so no corrective round runs, and the quarantine names the review
// as unavailable instead of blaming the build. Found live 2026-10-04: such a
// run launched a corrective rebuild with twelve "no-review-verdict" items to
// address, then quarantined as "gate failed: spec_conformity".
func TestAReviewWithNoVerdictIsUnavailableNotFlagged(t *testing.T) {
	timedOut := reviewRun(
		[]run.ReviewVerdict{{Criterion: "1. a", Verdict: "unavailable", Detail: "no-review-verdict"}, {Criterion: "2. b", Verdict: "unavailable", Detail: "no-review-verdict"}},
		&run.CodeReviewResult{Policy: "required", Available: false},
		"spec_conformity", "code_review",
	)
	if got := flaggedConformityVerdicts(timedOut); len(got) != 0 {
		t.Errorf("flagged = %v, want none", got)
	}
	if ReviewOnlyFlagged(timedOut) {
		t.Error("a review with no verdict is eligible for a corrective round")
	}
	if got := quarantineCheckFor(timedOut); got != request.QuarantineCheckReviewUnavailable {
		t.Errorf("check = %q, want %q", got, request.QuarantineCheckReviewUnavailable)
	}
	reason := quarantinedTicketReason(2, 2, "gate failed: spec_conformity", request.QuarantineCheckReviewUnavailable, nil)
	if !strings.Contains(reason, "the review gave no verdict") || !strings.Contains(reason, "gate failed: spec_conformity") {
		t.Errorf("reason = %q", reason)
	}
}

func TestReviewUnavailableCases(t *testing.T) {
	unavailable := run.ReviewVerdict{Criterion: "1. a", Verdict: "unavailable"}
	flagged := run.ReviewVerdict{Criterion: "2. b", Verdict: "flagged", Detail: "wrong status"}
	clean := run.ReviewVerdict{Criterion: "3. c", Verdict: "clean"}
	blocking := &run.CodeReviewResult{Policy: "required", Available: true, Findings: []run.CodeReviewFinding{{Severity: "high", Summary: "nil deref"}}}
	for name, tc := range map[string]struct {
		run       *run.Run
		wantCheck string
		wantRound bool
	}{
		"code reviewer unavailable, criteria clean": {reviewRun([]run.ReviewVerdict{clean}, &run.CodeReviewResult{Policy: "required"}, "code_review"), request.QuarantineCheckReviewUnavailable, false},
		"one criterion flagged, another unanswered": {reviewRun([]run.ReviewVerdict{unavailable, flagged}, nil, "spec_conformity"), request.QuarantineCheckSpecConformity, true},
		"a blocking finding, criteria unanswered":   {reviewRun([]run.ReviewVerdict{unavailable}, blocking, "spec_conformity", "code_review"), request.QuarantineCheckCodeReview, true},
		"a flagged criterion, reviewer available":   {reviewRun([]run.ReviewVerdict{flagged}, &run.CodeReviewResult{Policy: "required", Available: true}, "spec_conformity"), request.QuarantineCheckSpecConformity, true},
	} {
		t.Run(name, func(t *testing.T) {
			if got := quarantineCheckFor(tc.run); got != tc.wantCheck {
				t.Errorf("check = %q, want %q", got, tc.wantCheck)
			}
			if got := ReviewOnlyFlagged(tc.run); got != tc.wantRound {
				t.Errorf("corrective round eligible = %v, want %v", got, tc.wantRound)
			}
		})
	}
	// A real conformity failure is never hidden behind an unavailable
	// advisory code reviewer: spec_conformity failed with every criterion
	// answered, and code_review, advisory, passed without a reviewer.
	real := reviewRun([]run.ReviewVerdict{clean}, &run.CodeReviewResult{Policy: "advisory"}, "spec_conformity")
	if got := quarantineCheckFor(real); got != request.QuarantineCheckSpecConformity {
		t.Errorf("answered spec_conformity failure with an unavailable advisory reviewer: check = %q, want %q", got, request.QuarantineCheckSpecConformity)
	}
	// Both gates failed, but only the code reviewer went unanswered.
	half := reviewRun([]run.ReviewVerdict{clean}, &run.CodeReviewResult{Policy: "required"}, "spec_conformity", "code_review")
	if got := quarantineCheckFor(half); got == request.QuarantineCheckReviewUnavailable {
		t.Errorf("spec_conformity failed with every criterion answered: check = %q, want a real failure named", got)
	}

	// The addendum for the mixed case carries only the real finding.
	mixed := reviewRun([]run.ReviewVerdict{unavailable, flagged}, nil, "spec_conformity")
	if got := flaggedConformityVerdicts(mixed); len(got) != 1 || got[0].Verdict != "flagged" {
		t.Errorf("flagged = %+v, want only the flagged criterion", got)
	}
	if got := quarantinedTicketReason(1, 3, "gate failed: diff_scope", request.QuarantineCheckDiffScope, nil); got != "ticket 1/3 quarantined: gate failed: diff_scope" {
		t.Errorf("reason = %q", got)
	}
}

// A corrective round whose own review gives no verdict is named the same
// way a first run's is, instead of an unnamed quarantine with generic
// advice; a round that failed another gate keeps the unnamed quarantine.
func TestQuarantineAfterReviewRoundNamesAnUnavailableReview(t *testing.T) {
	building := func(t *testing.T) (string, *request.Request) {
		dataDir := t.TempDir()
		if err := request.SaveText(dataDir, "req-1", "text"); err != nil {
			t.Fatal(err)
		}
		r := request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now())
		r.State = request.StateBuilding
		if err := r.Save(dataDir); err != nil {
			t.Fatal(err)
		}
		return dataDir, r
	}
	const reason = "ticket 1/1: review corrective round 1/1 quarantined: gate failed: spec_conformity"

	dataDir, r := building(t)
	noVerdict := reviewRun([]run.ReviewVerdict{{Criterion: "1. a", Verdict: "unavailable"}}, &run.CodeReviewResult{Policy: "required"}, "spec_conformity", "code_review")
	if err := quarantineAfterReviewRound(dataDir, r, noVerdict, reason, time.Now()); err != nil {
		t.Fatal(err)
	}
	if r.State != request.StateQuarantined || r.QuarantineCheck != request.QuarantineCheckReviewUnavailable || !strings.Contains(r.Error, "the review gave no verdict") {
		t.Fatalf("State %q, QuarantineCheck %q, Error %q", r.State, r.QuarantineCheck, r.Error)
	}

	// A review the spend meter stopped says so, with the settings that
	// govern it, under the same check.
	dataDir, r = building(t)
	stopped := reviewRun([]run.ReviewVerdict{{Criterion: "1. a", Verdict: "unavailable"}}, &run.CodeReviewResult{Policy: "required", StoppedBy: "budget_exceeded"}, "spec_conformity", "code_review")
	if err := quarantineAfterReviewRound(dataDir, r, stopped, reason, time.Now()); err != nil {
		t.Fatal(err)
	}
	if r.QuarantineCheck != request.QuarantineCheckReviewUnavailable {
		t.Errorf("QuarantineCheck = %q, want %q", r.QuarantineCheck, request.QuarantineCheckReviewUnavailable)
	}
	for _, want := range []string{"the spend meter stopped the review's model calls", "budget_exceeded", "meter_token_budget", "was not judged"} {
		if !strings.Contains(r.Error, want) {
			t.Errorf("Error = %q, want it to contain %q", r.Error, want)
		}
	}
	if strings.Contains(r.Error, "gave no verdict") {
		t.Errorf("Error = %q, still words a stopped review as a silent one", r.Error)
	}
	if got := quarantinedTicketReason(1, 1, "gate failed: spec_conformity", request.QuarantineCheckReviewUnavailable, stopped); !strings.Contains(got, "the spend meter stopped") {
		t.Errorf("first-run reason = %q, want it to name the spend meter", got)
	}
	// The conformity review alone was stopped (no code review ran), and by
	// another limit: each code names its own settings.
	conformityOnly := reviewRun([]run.ReviewVerdict{{Criterion: "1. a", Verdict: "unavailable"}}, nil, "spec_conformity")
	conformityOnly.SpecConformityStoppedBy = "ceiling_exceeded"
	if got := noVerdictCause(conformityOnly); !strings.Contains(got, "ceiling_exceeded") || !strings.Contains(got, "meter_token_ceiling") || strings.Contains(got, "meter_token_budget") {
		t.Errorf("noVerdictCause = %q, want the ceiling settings and not the budget's", got)
	}
	conformityOnly.SpecConformityStoppedBy = "rate_limited"
	if got := noVerdictCause(conformityOnly); !strings.Contains(got, "meter_requests_per_minute") {
		t.Errorf("noVerdictCause = %q, want the request-rate setting", got)
	}
	conformityOnly.SpecConformityStoppedBy = "something else"
	if got := noVerdictCause(conformityOnly); got != "the review gave no verdict" {
		t.Errorf("noVerdictCause for an unknown code = %q", got)
	}

	dataDir, r = building(t)
	otherGate := reviewRun(nil, nil, "canonical_verify")
	if err := quarantineAfterReviewRound(dataDir, r, otherGate, reason, time.Now()); err != nil {
		t.Fatal(err)
	}
	if r.State != request.StateQuarantined || r.QuarantineCheck != "" || r.Error != reason {
		t.Fatalf("State %q, QuarantineCheck %q, Error %q", r.State, r.QuarantineCheck, r.Error)
	}
}
