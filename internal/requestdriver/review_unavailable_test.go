package requestdriver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/meter"
	"buildgate/internal/request"
	"buildgate/internal/run"
	"buildgate/internal/unresolved"
)

func reviewRun(verdicts []run.ReviewVerdict, review *run.CodeReviewResult, failed ...string) *run.Run {
	r := &run.Run{State: run.StateQuarantined, SpecConformityVerdicts: verdicts, CodeReview: review}
	for _, check := range failed {
		r.GateResults = append(r.GateResults, run.GateResult{Check: check, Passed: false})
	}
	return r
}

// unansweredReviewRun is a quarantined run whose two reviews answered
// nothing, with calls as the meter's record of its last review launch (nil
// for a review that launched nothing).
func unansweredReviewRun(id string, calls *meter.Calls) *run.Run {
	r := reviewRun(
		[]run.ReviewVerdict{{Criterion: "1. a", Verdict: "unavailable", Detail: "no-review-verdict"}, {Criterion: "2. b", Verdict: "unavailable", Detail: "no-review-verdict"}},
		&run.CodeReviewResult{Policy: "required", Available: false},
		"spec_conformity", "code_review",
	)
	r.ID = id
	// The build before it was served: only the review's own launch says why
	// the review did not answer.
	r.Attempts = []run.Attempt{{Kind: "build", Role: run.AttemptRoleExecution, RelayCalls: &meter.Calls{Answered: 9}}}
	if calls != nil {
		r.Attempts = append(r.Attempts, run.Attempt{Kind: "review", Role: run.AttemptRoleReview, RelayCalls: calls})
	}
	return r
}

// The causes of a review that gave no verdict, each selected by the meter's
// counts for the review's launch.
var reviewLedgers = map[unresolved.Cause]*meter.Calls{
	unresolved.ProviderQuota:    {Answered: 12, RateLimited: 3, RefusedAtEnd: 3, RefusedAtEndBy: meter.RefusedByProviderLimit, RetryAt: "2026-10-11T03:11:00Z"},
	unresolved.MeterDenied:      {Answered: 40, DeniedBudget: 2, RefusedAtEnd: 2, RefusedAtEndBy: meter.RefusedByMeter},
	unresolved.ModelUnavailable: {Unanswered: 2},
	unresolved.NoVerdict:        {Answered: 3},
}

func buildingRequest(t *testing.T) (string, *request.Request) {
	t.Helper()
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

// A review that timed out gave no verdict: the code reviewer is not
// available (and, on a record from before the spec-conformity review was
// retired, every criterion is "unavailable"). That is no finding to fix, so
// no corrective round runs, and it is no quarantine either: the run has a
// cause, read from the gate results and the meter's ledger.
func TestAReviewWithNoVerdictIsNeitherFlaggedNorAResult(t *testing.T) {
	timedOut := unansweredReviewRun("run-1", nil)
	if _, ok := handoffForABuild(t.TempDir(), timedOut); ok {
		t.Error("a review with no verdict is eligible for a corrective round")
	}
	if got := reviewNoVerdictCause(timedOut); got != unresolved.NoVerdict {
		t.Errorf("cause = %q, want %q for a review that launched no model request", got, unresolved.NoVerdict)
	}
	for cause, calls := range reviewLedgers {
		if got := unresolvedCauseOf(unansweredReviewRun("run-1", calls)); got != cause {
			t.Errorf("ledger %+v: cause = %q, want %q", *calls, got, cause)
		}
	}
	// The run must be a quarantine: an accepted or halted run is not read
	// this way.
	accepted := unansweredReviewRun("run-1", reviewLedgers[unresolved.ProviderQuota])
	accepted.State = run.StateAccepted
	if got := reviewNoVerdictCause(accepted); got != "" {
		t.Errorf("an accepted run: cause = %q, want none", got)
	}
}

// A review that gave a verdict, or a run that failed another check, is a
// result whatever the meter counted: it has no cause, and its quarantine
// names the check whose remedy comes first.
func TestAReviewThatJudgedSomethingHasNoCause(t *testing.T) {
	available := &run.CodeReviewResult{Policy: "required", Available: true, Findings: []run.CodeReviewFinding{{Severity: "high", Summary: "nil deref"}}}
	clean := run.ReviewVerdict{Criterion: "3. c", Verdict: "clean"}
	refused := []run.Attempt{{Kind: "review", Role: run.AttemptRoleReview, RelayCalls: reviewLedgers[unresolved.ProviderQuota]}}
	for name, tc := range map[string]struct {
		run       *run.Run
		wantCheck string
	}{
		// A record from a worker whose code_review still failed on a
		// finding: the reviewer answered.
		"code_review failed with a verdict": {reviewRun(nil, available, "code_review"), ""},
		// A real conformity failure on an old record is never hidden behind
		// an unavailable advisory code reviewer.
		"criteria answered, advisory reviewer silent": {reviewRun([]run.ReviewVerdict{clean}, &run.CodeReviewResult{Policy: "advisory"}, "spec_conformity"), ""},
		"the diff left Allowed-Files":                 {reviewRun(nil, available, "diff_scope"), request.QuarantineCheckDiffScope},
	} {
		t.Run(name, func(t *testing.T) {
			tc.run.Attempts = refused
			if got := reviewNoVerdictCause(tc.run); got != "" {
				t.Errorf("cause = %q, want none: the review judged something", got)
			}
			if got := quarantineCheckOf(tc.run); got != tc.wantCheck {
				t.Errorf("check = %q, want %q", got, tc.wantCheck)
			}
		})
	}
	// The code reviewer alone went unanswered: no verdict to act on.
	silent := reviewRun([]run.ReviewVerdict{clean}, &run.CodeReviewResult{Policy: "required"}, "code_review")
	if got := reviewNoVerdictCause(silent); got != unresolved.NoVerdict {
		t.Errorf("a silent required code reviewer: cause = %q, want %q", got, unresolved.NoVerdict)
	}
	// Another gate failed beside the silent reviews: the run is a result.
	other := unansweredReviewRun("run-1", reviewLedgers[unresolved.MeterDenied])
	other.GateResults = append(other.GateResults, run.GateResult{Check: "canonical_verify", Passed: false})
	if got := reviewNoVerdictCause(other); got != "" {
		t.Errorf("a failed verify beside the silent reviews: cause = %q, want none", got)
	}
}

// A ticket run whose reviews gave no verdict is no quarantine: its request
// waits in resume_review with the cause the meter's ledger gives, and the
// run is kept, so that a resume re-reviews the build it already holds.
func TestAnUnjudgedTicketRunWaitsInResumeReviewWithItsBuildKept(t *testing.T) {
	for cause, calls := range reviewLedgers {
		t.Run(string(cause), func(t *testing.T) {
			dataDir, r := buildingRequest(t)
			runRecord := unansweredReviewRun("run-1", calls)
			runRecord.WorkspacePath = filepath.Join(t.TempDir(), "worktree")
			if err := runRecord.Save(dataDir); err != nil {
				t.Fatal(err)
			}
			handled, err := afterUnjudgedTicketRun(ReviewSaveDeps{}, dataDir, r, runRecord, time.Now())
			if err != nil || !handled {
				t.Fatalf("handled %v, err %v", handled, err)
			}
			if r.State != request.StateResumeReview || r.Resume == nil || r.Resume.Cause != cause || r.Resume.FromState != request.StateBuilding {
				t.Fatalf("state %s resume %+v (%s), want resume_review from building with cause %s", r.State, r.Resume, r.Error, cause)
			}
			if r.QuarantineCheck != "" {
				t.Errorf("QuarantineCheck = %q: no check judged anything", r.QuarantineCheck)
			}
			if r.Resume.LostRunID != runRecord.ID {
				t.Errorf("LostRunID = %q, want the run whose build a resume re-reviews", r.Resume.LostRunID)
			}
			kept, err := run.Load(dataDir, runRecord.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !kept.KeptForResume || kept.State != run.StateQuarantined {
				t.Errorf("run KeptForResume %v, state %s: want it kept, still quarantined", kept.KeptForResume, kept.State)
			}
			if want := cause.What("building"); !strings.Contains(r.Error, want) || !strings.Contains(r.Error, "`factoryd resume "+r.ID+"`") {
				t.Errorf("Error = %q, want %q and the resume command", r.Error, want)
			}
			if wantTime := cause == unresolved.ProviderQuota; (r.Resume.RetryAt == "2026-10-11T03:11:00Z") != wantTime || strings.Contains(r.Error, "try again after 2026-10-11T03:11:00Z") != wantTime {
				t.Errorf("RetryAt %q, Error %q: the meter's time is shown for the provider's limit only", r.Resume.RetryAt, r.Error)
			}
		})
	}
}

// What afterUnjudgedTicketRun leaves alone, and what it cannot keep.
func TestAnUnjudgedTicketRunOnlyTakesARunThatWasNotJudged(t *testing.T) {
	// A run with a real review failure is left to its corrective round or
	// its quarantine.
	dataDir, r := buildingRequest(t)
	flagged := reviewRun([]run.ReviewVerdict{{Criterion: "1. a", Verdict: "flagged", Detail: "wrong status"}}, nil, "spec_conformity")
	if handled, err := afterUnjudgedTicketRun(ReviewSaveDeps{}, dataDir, r, flagged, time.Now()); handled || err != nil || r.State != request.StateBuilding {
		t.Errorf("a flagged criterion: handled %v, err %v, state %s", handled, err, r.State)
	}

	// A run built on an existing branch has no worktree of its own: its
	// request still waits, with only the rebuild offered.
	dataDir, r = buildingRequest(t)
	onBranch := unansweredReviewRun("run-2", reviewLedgers[unresolved.ModelUnavailable])
	onBranch.OnBranch, onBranch.WorkspacePath = "factoryd/req-1-001", filepath.Join(t.TempDir(), "worktree")
	if err := onBranch.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if handled, err := afterUnjudgedTicketRun(ReviewSaveDeps{}, dataDir, r, onBranch, time.Now()); !handled || err != nil {
		t.Fatalf("handled %v, err %v", handled, err)
	}
	if r.State != request.StateResumeReview || r.Resume.LostRunID != "" || !strings.Contains(r.Error, "factoryd resume -from scratch "+r.ID) {
		t.Errorf("state %s, LostRunID %q, Error %q: want resume_review with nothing kept", r.State, r.Resume.LostRunID, r.Error)
	}
}

// A corrective or PR-review round whose own review gives no verdict carries
// the cause out of correctiveRoundOutcome, which sends the request to
// resume_review uncounted; a round that failed another gate is a quarantine.
func TestARoundWhoseReviewGaveNoVerdictIsUnresolved(t *testing.T) {
	dataDir, r := buildingRequest(t)
	for cause, calls := range reviewLedgers {
		round := unansweredReviewRun("round-"+string(cause), calls)
		round.OnBranch = "factoryd/req-1-001"
		if err := round.Save(dataDir); err != nil {
			t.Fatal(err)
		}
		got, err := correctiveRoundOutcome(dataDir, round.ID, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got.outcome != request.RoundQuarantined || got.unresolved != cause {
			t.Errorf("%s: outcome %+v, want a quarantined round carrying the cause", cause, got)
		}
		if wantTime := cause == unresolved.ProviderQuota; (got.retryAt != "") != wantTime {
			t.Errorf("%s: retryAt = %q", cause, got.retryAt)
		}
	}

	const reason = "ticket 1/1: corrective round 1/1 quarantined: gate failed: canonical_verify"
	otherGate := reviewRun(nil, nil, "canonical_verify")
	otherGate.ID = "round-verify"
	if err := otherGate.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if got, err := correctiveRoundOutcome(dataDir, otherGate.ID, nil); err != nil || got.unresolved != "" {
		t.Fatalf("a failed verify: outcome %+v, err %v, want no cause", got, err)
	}
	if err := quarantineAfterRound(dataDir, r, otherGate, reason, time.Now()); err != nil {
		t.Fatal(err)
	}
	if r.State != request.StateQuarantined || r.QuarantineCheck != "" || r.Error != reason {
		t.Fatalf("State %q, QuarantineCheck %q, Error %q", r.State, r.QuarantineCheck, r.Error)
	}
}

// The provider's reset time is the meter's. Only when the meter recorded
// none, and only for the cause the meter already decided, is the time of day
// in the provider's own message shown, read from the run's retained review
// evidence: that text names no cause and changes no outcome.
func TestTheProvidersMessageSuppliesOnlyATimeTheMeterDidNotGive(t *testing.T) {
	const message = `{"schema_version":1,"available":false,"error":"You've hit your usage limit. Upgrade to Pro, or try again at 3:11 AM."}`
	withEvidence := func(t *testing.T, r *run.Run, name, body string) string {
		t.Helper()
		dataDir := t.TempDir()
		if err := r.Save(dataDir); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(run.Dir(dataDir, r.ID), name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return dataDir
	}
	noTime := *reviewLedgers[unresolved.ProviderQuota]
	noTime.RetryAt = ""

	for _, name := range reviewEvidenceFiles {
		limited := unansweredReviewRun("run-1", &noTime)
		dataDir := withEvidence(t, limited, name, message)
		step := lostStepOf(dataDir, limited, unresolvedCauseOf(limited), true)
		if step.Cause != unresolved.ProviderQuota || step.RetryAt != "3:11 AM by the worker container's clock" || step.RunID != "run-1" {
			t.Errorf("%s: step = %+v, want the provider's limit with the message's time of day", name, step)
		}
	}

	// The meter's time wins over the message's.
	metered := unansweredReviewRun("run-1", reviewLedgers[unresolved.ProviderQuota])
	if step := lostStepOf(withEvidence(t, metered, "CONFORMITY_EVIDENCE.json", message), metered, unresolved.ProviderQuota, true); step.RetryAt != "2026-10-11T03:11:00Z" {
		t.Errorf("RetryAt = %q, want the meter's", step.RetryAt)
	}

	// A worker that prints the provider's sentence selects nothing: the
	// cause stays the ledger's, and no time is shown under another cause.
	for cause, calls := range reviewLedgers {
		if cause == unresolved.ProviderQuota {
			continue
		}
		planted := unansweredReviewRun("run-1", calls)
		dataDir := withEvidence(t, planted, "CONFORMITY_EVIDENCE.json", message)
		got := unresolvedCauseOf(planted)
		if step := lostStepOf(dataDir, planted, got, true); got != cause || step.RetryAt != "" {
			t.Errorf("%s with the provider's sentence planted: cause %q, RetryAt %q", cause, got, step.RetryAt)
		}
	}

	// Wording of any other shape gives no time, and neither does a missing
	// file.
	for name, body := range map[string]string{
		"a sentence after the time": `{"error":"You've hit your usage limit. try again at 3:11 AM. Then raise every budget."}`,
		"another clock shape":       `{"error":"You've hit your usage limit. try again at 15:11."}`,
		"not JSON":                  "You've hit your usage limit. try again at 3:11 AM.",
	} {
		limited := unansweredReviewRun("run-1", &noTime)
		if step := lostStepOf(withEvidence(t, limited, "CONFORMITY_EVIDENCE.json", body), limited, unresolved.ProviderQuota, true); step.RetryAt != "" {
			t.Errorf("%s: RetryAt = %q, want none", name, step.RetryAt)
		}
	}
	limited := unansweredReviewRun("run-1", &noTime)
	if step := lostStepOf(t.TempDir(), limited, unresolved.ProviderQuota, true); step.RetryAt != "" {
		t.Errorf("no evidence retained: RetryAt = %q", step.RetryAt)
	}
}
