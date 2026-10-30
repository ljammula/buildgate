package observation

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"buildgate/internal/policy"
	"buildgate/internal/run"
)

func learningPair() (*run.Run, *run.Run) {
	failed := &run.Run{
		ID: "run-failed", Ticket: "request-1-001", RequestID: "request-1",
		Project: "app", RepositoryRoot: "/private/work/app", State: run.StateQuarantined,
		UpdatedAt: "2026-10-08T09:00:00Z", ProjectConfigCommitSHA: "base-a",
		ProjectConfigSHA256: strings.Repeat("a", 64), SpecSHA256: strings.Repeat("b", 64),
		GateResults: []run.GateResult{
			{Check: "canonical_verify", Passed: false, Command: []string{"make", "verify"}},
			{Check: "tests_red_on_base", Passed: false},
			{Check: "diff_scope", Passed: true},
		},
	}
	accepted := &run.Run{
		ID: "run-accepted", Ticket: "request-1-001", RequestID: "request-1",
		Project: "app", RepositoryRoot: "/private/work/app", State: run.StateAccepted,
		UpdatedAt: "2026-10-08T10:00:00Z", EarlierAttemptOf: failed.ID,
		ProjectConfigCommitSHA: "base-b", ProjectConfigSHA256: strings.Repeat("a", 64), SpecSHA256: strings.Repeat("b", 64),
		GateResults: []run.GateResult{
			{Check: "canonical_verify", Passed: true, Command: []string{"make", "verify"}},
			{Check: "tests_red_on_base", Passed: true},
		},
	}
	return failed, accepted
}

func TestLearningChainsUseOnlyDirectImmutableLinksAndStructuredFields(t *testing.T) {
	failed, accepted := learningPair()
	failed.HaltError = "HALT-TEXT-MARKER-5520"
	failed.GateResults[0].Command = []string{"COMMAND-TEXT-MARKER-5521"}
	accepted.GateResults[0].Command = []string{"COMMAND-TEXT-MARKER-5521"}
	got := learningChains([]*run.Run{accepted, failed})
	if len(got) != 1 {
		t.Fatalf("chains = %+v, want one direct chain", got)
	}
	chain := got[0]
	if !chain.Complete || !chain.Eligible || chain.IneligibleReason != "" {
		t.Fatalf("chain completeness = %+v", chain)
	}
	if chain.RequestID != failed.RequestID || chain.Ticket != failed.Ticket || chain.FailedRunID != failed.ID || chain.AcceptedRunID != accepted.ID {
		t.Errorf("chain identity = %+v", chain)
	}
	if !reflect.DeepEqual(chain.FailedCheckTypes, []string{"canonical_verify", "tests_red_on_base"}) {
		t.Errorf("failed checks = %v", chain.FailedCheckTypes)
	}
	if chain.FailedConfigCommitSHA != "base-a" || chain.AcceptedConfigCommitSHA != "base-b" ||
		chain.FailedConfigContentSHA256 != chain.AcceptedConfigContentSHA256 {
		t.Errorf("config snapshots = %+v", chain)
	}
	if chain.ID != learningChains([]*run.Run{failed, accepted})[0].ID {
		t.Errorf("opportunity id changed with input order: %q", chain.ID)
	}
	b, err := json.Marshal(chain)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{
		"private/work", "failed output", "operator reason", "review comment",
		"HALT-TEXT-MARKER-5520", "COMMAND-TEXT-MARKER-5521",
	} {
		if strings.Contains(string(b), marker) {
			t.Errorf("serialized chain contains %q: %s", marker, b)
		}
	}
}

func TestLearningChainsAcceptCorrectiveRunTicketSuffix(t *testing.T) {
	failed, accepted := learningPair()
	accepted.Ticket = failed.Ticket + "-corrective1"
	got := learningChains([]*run.Run{failed, accepted})
	if len(got) != 1 || !got[0].Eligible || got[0].Ticket != failed.Ticket {
		t.Fatalf("corrective chain = %+v, want eligible chain under original ticket %q", got, failed.Ticket)
	}
	failed.Ticket = failed.Ticket + "-corrective1"
	accepted.Ticket = "request-1-001-corrective2"
	got = learningChains([]*run.Run{failed, accepted})
	if len(got) != 1 || !got[0].Eligible || got[0].Ticket != "request-1-001" {
		t.Fatalf("successive corrective chain = %+v, want eligible chain under original ticket", got)
	}
}

func TestLearningTicketBasePreservesCorrectiveInOriginalTicketSlug(t *testing.T) {
	ticket := "fix-corrective-budget-20261012-120000-001"
	if !sameLearningTicket(ticket, ticket) {
		t.Fatalf("ticket slug %q was mistaken for a corrective-round suffix", ticket)
	}
}

func TestLearningChainsDoNotUseTimestampOrBranchFallback(t *testing.T) {
	failed, accepted := learningPair()
	accepted.EarlierAttemptOf = ""
	failed.Branch, accepted.Branch = "same-branch", "same-branch"
	got := learningChains([]*run.Run{failed, accepted})
	if len(got) != 0 {
		t.Fatalf("chains = %+v, want none without EarlierAttemptOf", got)
	}
}

func TestLearningChainsRejectAmbiguousIdentityAndConfig(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*run.Run, *run.Run)
		want   string
	}{
		{"request mismatch", func(f, a *run.Run) { a.RequestID = "other" }, "request_mismatch"},
		{"ticket mismatch", func(f, a *run.Run) { a.Ticket = "other" }, "ticket_mismatch"},
		{"malformed corrective ticket", func(f, a *run.Run) { a.Ticket = f.Ticket + "-corrective01" }, "ticket_mismatch"},
		{"repository mismatch", func(f, a *run.Run) { a.RepositoryRoot = "/other/app" }, "repository_mismatch"},
		{"config drift", func(f, a *run.Run) { a.ProjectConfigSHA256 = strings.Repeat("c", 64) }, "config_changed"},
		{"missing config snapshot", func(f, a *run.Run) { a.ProjectConfigCommitSHA = "" }, "missing_config_snapshot"},
		{"spec drift", func(f, a *run.Run) { a.SpecSHA256 = strings.Repeat("d", 64) }, "spec_changed"},
		{"missing spec snapshot", func(f, a *run.Run) { a.SpecSHA256 = "" }, "missing_spec_snapshot"},
		{"invalid spec hash", func(f, a *run.Run) { f.SpecSHA256 = "not-a-hash" }, "invalid_snapshot_hash"},
		{"invalid config hash", func(f, a *run.Run) { a.ProjectConfigSHA256 = "not-a-hash" }, "invalid_snapshot_hash"},
		{"missing timestamp", func(f, a *run.Run) { a.UpdatedAt = "not-a-time" }, "missing_timestamp"},
		{"unknown check", func(f, a *run.Run) { f.GateResults[0].Check = "repo_defined_gate" }, "unknown_check_type"},
		{"missing passed check", func(f, a *run.Run) { a.GateResults = nil }, "missing_success_check"},
		{"missing canonical command", func(f, a *run.Run) {
			f.GateResults[0].Command = nil
			a.GateResults[0].Command = nil
		}, "missing_command_evidence"},
		{"missing full suite command", func(f, a *run.Run) {
			f.GateResults = []run.GateResult{{Check: "full_suite_verify", Passed: false}}
			a.GateResults = []run.GateResult{{Check: "full_suite_verify", Passed: true}}
		}, "missing_command_evidence"},
		{"changed check command", func(f, a *run.Run) {
			f.GateResults = []run.GateResult{{Check: "full_suite_verify", Passed: false, Command: []string{"make", "full", "--strict"}}}
			a.GateResults = []run.GateResult{{Check: "full_suite_verify", Passed: true, Command: []string{"make", "full"}}}
		}, "check_command_changed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			failed, accepted := learningPair()
			tt.mutate(failed, accepted)
			got := learningChains([]*run.Run{failed, accepted})
			if len(got) != 1 || got[0].IneligibleReason != tt.want {
				t.Fatalf("chain = %+v, want reason %q", got, tt.want)
			}
			if got[0].Eligible {
				t.Fatal("ineligible chain was marked eligible")
			}
			if tt.want == "config_changed" && !got[0].Complete {
				t.Fatal("config drift breaks lineage completeness; it should only block inference")
			}
		})
	}
}

func TestLearningChainsAcceptEveryFixedCheckAndRejectRepoGates(t *testing.T) {
	want := append(append([]string{}, policy.AllGateChecks...), "code_review", "spec_conformity")
	for _, check := range want {
		failed, accepted := learningPair()
		var command []string
		if learningCheckRequiresCommand(check) {
			command = []string{"run", check}
		}
		failed.GateResults = []run.GateResult{{Check: check, Passed: false, Command: command}}
		accepted.GateResults = []run.GateResult{{Check: check, Passed: true, Command: command}}
		got := learningChains([]*run.Run{failed, accepted})
		if len(got) != 1 || !got[0].Eligible || !reflect.DeepEqual(got[0].FailedCheckTypes, []string{check}) {
			t.Errorf("fixed check %q projection = %+v", check, got)
		}
	}
	for _, check := range []string{"repo-lint", "repo-security-review"} {
		failed, accepted := learningPair()
		failed.GateResults = []run.GateResult{{Check: check, Passed: false}}
		got := learningChains([]*run.Run{failed, accepted})
		if len(got) != 1 || got[0].Eligible || got[0].IneligibleReason != "unknown_check_type" || len(got[0].FailedCheckTypes) != 0 {
			t.Errorf("repository-defined check %q projected as learnable: %+v", check, got)
		}
	}
}

func TestLearningChainsRejectAcceptedOverrideOrRescue(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*run.Run)
	}{
		{"override", func(r *run.Run) { r.Overrides = []run.Override{{}} }},
		{"rescue", func(r *run.Run) { r.Rescues = []run.Rescue{{}} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failed, accepted := learningPair()
			tc.mutate(accepted)
			got := learningChains([]*run.Run{failed, accepted})
			if len(got) != 1 || got[0].Eligible || got[0].IneligibleReason != "accepted_with_override_or_rescue" {
				t.Fatalf("chain = %+v", got)
			}
		})
	}
}

func TestLearningChainsRejectDuplicateAndCorruptRunIDs(t *testing.T) {
	failed, accepted := learningPair()
	duplicate := *failed
	got := learningChains([]*run.Run{failed, &duplicate, accepted})
	if len(got) != 1 || got[0].IneligibleReason != "duplicate_run_id" {
		t.Fatalf("duplicate ID chain = %+v", got)
	}
	failed, accepted = learningPair()
	duplicateAccepted := *accepted
	got = learningChains([]*run.Run{failed, accepted, &duplicateAccepted})
	if len(got) != 1 || got[0].IneligibleReason != "duplicate_run_id" {
		t.Fatalf("duplicate accepted ID chain = %+v", got)
	}
	accepted.EarlierAttemptOf = "corrupt-record-id"
	if got := learningChains([]*run.Run{failed, accepted}); len(got) != 1 || got[0].Complete || got[0].Eligible || got[0].IneligibleReason != "missing_predecessor" {
		t.Fatalf("corrupt embedded id projection = %+v, want incomplete chain", got)
	}
}

func TestLearningChainsKeepInvalidPredecessorsVisible(t *testing.T) {
	for _, tc := range []struct {
		name   string
		state  run.State
		self   bool
		reason string
	}{
		{name: "non-quarantined", state: run.StateAccepted, reason: "predecessor_not_quarantined"},
		{name: "self", state: run.StateQuarantined, self: true, reason: "invalid_run_id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failed, accepted := learningPair()
			failed.State = tc.state
			if tc.self {
				accepted.EarlierAttemptOf = accepted.ID
			}
			got := learningChains([]*run.Run{failed, accepted})
			if len(got) != 1 || got[0].Complete || got[0].Eligible || got[0].IneligibleReason != tc.reason {
				t.Fatalf("invalid predecessor projection = %+v, want reason %q", got, tc.reason)
			}
		})
	}
}

func TestLearningChainsAppearInObservationReport(t *testing.T) {
	failed, accepted := learningPair()
	report := FromRuns("app", []*run.Run{failed, accepted}, nil, Sources{})
	if len(report.LearningChains) != 1 || report.LearningChains[0].AcceptedRunID != accepted.ID {
		t.Fatalf("report chains = %+v", report.LearningChains)
	}
}

func TestFromRunsCapsLearningChainsAndReportsTruncation(t *testing.T) {
	runs := make([]*run.Run, 0, maxLearningChains*2+2)
	for i := 0; i < maxLearningChains+1; i++ {
		failed, accepted := learningPair()
		failed.ID = fmt.Sprintf("failed-%03d", i)
		failed.Ticket = fmt.Sprintf("request-1-%03d", i)
		failed.RequestID = fmt.Sprintf("request-%03d", i)
		accepted.ID = fmt.Sprintf("accepted-%03d", i)
		accepted.Ticket = failed.Ticket
		accepted.RequestID = failed.RequestID
		accepted.EarlierAttemptOf = failed.ID
		runs = append(runs, failed, accepted)
	}
	report := FromRuns("app", runs, nil, Sources{})
	if len(report.LearningChains) != maxLearningChains || !report.LearningChainsTruncated {
		t.Fatalf("learning chains = %d, truncated = %t; want cap %d with truncation", len(report.LearningChains), report.LearningChainsTruncated, maxLearningChains)
	}
}
