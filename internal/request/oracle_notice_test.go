package request

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOracleReviewNoticeWhenRunCommandMissing(t *testing.T) {
	dataDir := t.TempDir()
	r := newDraftOraclesRequest(t, dataDir, "req-1", StateOracleReview)
	if got := OracleReviewNotice(dataDir, r); got != "" {
		t.Fatalf("no oracle dir: notice = %q, want none (absent oracle/ is a skip)", got)
	}
	writeRequestOracle(t, dataDir, "req-1", "a_oracle_test.go", "package x\n")
	got := OracleReviewNotice(dataDir, r)
	for _, want := range []string{"oracle/RUN_COMMAND.txt", ".oracle", "go test ./.oracle/..."} {
		if !strings.Contains(got, want) {
			t.Errorf("notice %q missing %q", got, want)
		}
	}

	r.OracleDraft = &OracleDraft{Status: OracleDrafted, ProposedCommand: "go test ./.oracle/a_oracle_test.go"}
	if got := OracleReviewNotice(dataDir, r); !strings.Contains(got, "suggested for this oracle") {
		// Load reads the saved record, so persist the proposal first.
		if err := r.Save(dataDir); err != nil {
			t.Fatal(err)
		}
		got = OracleReviewNotice(dataDir, r)
		if !strings.Contains(got, "suggested for this oracle: go test ./.oracle/a_oracle_test.go") {
			t.Errorf("notice %q does not carry the drafter's proposal", got)
		}
	}

	if err := os.WriteFile(filepath.Join(oracleDirPath(dataDir, "req-1"), TicketOracleRunCommandFilename), []byte("go test ./.oracle/...\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := OracleReviewNotice(dataDir, r); got != "" {
		t.Errorf("RUN_COMMAND.txt present: notice = %q, want none", got)
	}

	r.State = StatePlanReview
	_ = os.Remove(filepath.Join(oracleDirPath(dataDir, "req-1"), TicketOracleRunCommandFilename))
	if got := OracleReviewNotice(dataDir, r); got != "" {
		t.Errorf("outside oracle_review: notice = %q, want none", got)
	}
}

func TestOracleReviewNoticeForGeneratedRunCommand(t *testing.T) {
	dataDir := t.TempDir()
	r := newDraftOraclesRequest(t, dataDir, "req-1", StateOracleReview)
	writeRequestOracle(t, dataDir, "req-1", "a_oracle_test.go", "package x\n")
	command := "go test ./.oracle/...\n"
	if err := os.WriteFile(filepath.Join(oracleDirPath(dataDir, "req-1"), TicketOracleRunCommandFilename), []byte(command), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(command))
	r.OracleDraft = &OracleDraft{Status: OracleDrafted, GeneratedRunCommandSHA256: hex.EncodeToString(sum[:])}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	got := OracleReviewNotice(dataDir, r)
	for _, want := range []string{"oracle/RUN_COMMAND.txt was written by the drafter, not you", command} {
		if !strings.Contains(got, strings.TrimSuffix(want, "\n")) {
			t.Errorf("notice %q missing %q", got, want)
		}
	}

	// An operator's own edit no longer hashes to GeneratedRunCommandSHA256,
	// so the notice must fall silent -- the whole point is it never re-fires
	// on a file a human has already looked at and touched.
	if err := os.WriteFile(filepath.Join(oracleDirPath(dataDir, "req-1"), TicketOracleRunCommandFilename), []byte("go test ./.oracle/... -run TestOracle\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := OracleReviewNotice(dataDir, r); got != "" {
		t.Errorf("edited RUN_COMMAND.txt: notice = %q, want none", got)
	}
}

func TestApproveRefusalForMissingRunCommandIsActionable(t *testing.T) {
	dataDir := t.TempDir()
	newDraftOraclesRequest(t, dataDir, "req-1", StateOracleReview)
	writeRequestOracle(t, dataDir, "req-1", "a_oracle_test.go", "package x\n")
	_, err := Approve(dataDir, "req-1", "alice", fixedNow, nil)
	if err == nil || !strings.Contains(err.Error(), "write oracle/RUN_COMMAND.txt") {
		t.Fatalf("err = %v, want the actionable missing-RUN_COMMAND message", err)
	}
}

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
// policy denial never suggests a doomed retry, HaltOracleMaterialize points
// back at oracle_review, an ordinary halt/quarantine points at retry, and a
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
		{"oracle_review", func() *Request {
			r := New("req-1", "/w", "w", Source{Kind: SourceText}, fixedNow)
			r.State = StateOracleReview
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
		{"quarantined, spec_conformity only (Follow-up B)", func() *Request {
			r := New("req-1", "/w", "w", Source{Kind: SourceText}, fixedNow)
			r.State = StateQuarantined
			r.Error = "ticket 1/1: spec_conformity corrective rounds exhausted (1/1)"
			r.QuarantineCheck = QuarantineCheckSpecConformity
			return r
		}, []string{"factoryd reject -to spec", "req-1", "factoryd retry req-1"}, []string{"fix the cause named above, then"}},
		{"quarantined, spec_conformity but a ticket already accepted", func() *Request {
			r := New("req-1", "/w", "w", Source{Kind: SourceText}, fixedNow)
			r.State = StateQuarantined
			r.Error = "boom"
			r.QuarantineCheck = QuarantineCheckSpecConformity
			r.Tickets = []Ticket{{Index: 1, RunID: "run-1", Branch: "factoryd/run-1"}}
			return r
		}, []string{"factoryd retry req-1"}, []string{"factoryd reject -to spec"}},
		{"quarantined, code_review", func() *Request {
			r := New("req-1", "/w", "w", Source{Kind: SourceText}, fixedNow)
			r.State = StateQuarantined
			r.Error = "ticket 1/1: review corrective rounds exhausted (1/1)"
			r.QuarantineCheck = QuarantineCheckCodeReview
			return r
		}, []string{"code_review quarantined", "Code review findings", "factoryd retry req-1"}, []string{"fix the cause named above, then"}},
		{"quarantined, review unavailable", func() *Request {
			r := New("req-1", "/w", "w", Source{Kind: SourceText}, fixedNow)
			r.State = StateQuarantined
			r.Error = "ticket 2/2 quarantined: the review gave no verdict, so the build was not judged (gate failed: spec_conformity)"
			r.QuarantineCheck = QuarantineCheckReviewUnavailable
			return r
		}, []string{"review unavailable", "nothing was judged wrong", "factoryd retry req-1"}, []string{"blocking defects", "spec_conformity quarantined"}},
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
		{"halted, oracle materialize", func() *Request {
			r := New("req-1", "/w", "w", Source{Kind: SourceText}, fixedNow)
			r.State = StatePlanning
			r.prevState = StatePlanning
			if err := r.HaltOracleMaterialization("cap exceeded", fixedNow); err != nil {
				t.Fatal(err)
			}
			return r
		}, []string{"factoryd retry req-1", "oracle_review"}, nil},
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
