package requestdriver_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
)

func TestCapOracleFeedbackKeepsNewestAndValidUTF8(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 40; i++ {
		fmt.Fprintf(&b, "## Oracle rejected 2026-09-%02d by op\n\nreason-%d %s\n\n", i%28+1, i, strings.Repeat("é", 300))
	}
	got := requestdriver.CapFeedback(b.String(), requestdriver.MaxFeedbackBytes, "## Oracle rejected ")
	if len(got) > requestdriver.MaxFeedbackBytes {
		t.Errorf("len = %d, want <= %d", len(got), requestdriver.MaxFeedbackBytes)
	}
	if !strings.Contains(got, "reason-40 ") || strings.Contains(got, "reason-1 ") {
		t.Errorf("did not keep the newest and drop the oldest:\n%.200s", got)
	}
	if !strings.Contains(got, "omitted") {
		t.Error("truncation not announced")
	}
	if !utf8.ValidString(got) {
		t.Error("invalid UTF-8 after capping")
	}
	if small := "## Oracle rejected x\n\nshort\n\n"; requestdriver.CapFeedback(small, requestdriver.MaxFeedbackBytes, "## Oracle rejected ") != small {
		t.Error("small feedback altered")
	}
}

// errUnreachableRelay stands in for a real spec-drafting job failure
// (e.g. the sandboxed worker's relay being unreachable) in the tests
// below -- its own text is what those tests look for in the resulting
// halt reason and notification.
var errUnreachableRelay = errors.New("relay unreachable")

// TestDriveRequestsIsANoOpWhenNothingIsInADrivenState covers a request
// already in spec_review (or any later state): driveRequests must leave
// it untouched -- approval/planning/building/PR review are later work
// packages' job. The spec-draft runner must never be invoked.
func TestDriveRequestsIsANoOpWhenNothingIsInADrivenState(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir := t.TempDir()
	if err := request.SaveText(dataDir, "req-1", "text"); err != nil {
		t.Fatal(err)
	}
	r := request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now())
	r.State = request.StateSpecReview
	before := *r
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), requestdrivertest.FailingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}

	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != before.State {
		t.Errorf("State = %q, want unchanged %q", loaded.State, before.State)
	}
}

// TestDriveRequestsPicksOldestSubmittedFirst covers the "pick the oldest
// request in a machine state" part of the plan's design: with two
// requests both in a driven state, the older (by SubmittedAt) advances
// first. Both are in StateSubmitted, so the spec-draft runner is never
// called (that's a pure state move).
func TestDriveRequestsPicksOldestSubmittedFirst(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir := t.TempDir()
	older := request.New("older", "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now().Add(-time.Hour))
	newer := request.New("newer", "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now())
	for _, r := range []*request.Request{older, newer} {
		if err := request.SaveText(dataDir, r.ID, "text"); err != nil {
			t.Fatal(err)
		}
		if err := r.Save(dataDir); err != nil {
			t.Fatal(err)
		}
	}

	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), requestdrivertest.FailingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}

	loadedOlder, err := request.Load(dataDir, "older")
	if err != nil {
		t.Fatal(err)
	}
	loadedNewer, err := request.Load(dataDir, "newer")
	if err != nil {
		t.Fatal(err)
	}
	if loadedOlder.State != request.StateSpecDrafting {
		t.Errorf("older.State = %q, want %q (the oldest request should advance first)", loadedOlder.State, request.StateSpecDrafting)
	}
	if loadedNewer.State != request.StateSubmitted {
		t.Errorf("newer.State = %q, want unchanged %q", loadedNewer.State, request.StateSubmitted)
	}
}

// TestAdvanceSpecDraftingJobFailureHaltsWithReasonAndNotification covers
// the plan's own "an empty or malformed draft halts the request with a
// reason and a notification" requirement for the job-failure half of
// that: the spec-drafting job itself returning an error moves the
// request to halted, naming why, and a durable notification is recorded
// before DispatchExternal ever runs.
func TestAdvanceSpecDraftingJobFailureHaltsWithReasonAndNotification(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir := t.TempDir()
	if err := request.SaveText(dataDir, "req-1", "text"); err != nil {
		t.Fatal(err)
	}
	r := request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now())
	r.State = request.StateSpecDrafting
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	runner, _ := stubSpecDraftRunner("", nil, errUnreachableRelay)
	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, runner, requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), requestdrivertest.FailingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}

	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateHalted {
		t.Fatalf("State = %q, want %q", loaded.State, request.StateHalted)
	}
	if !strings.Contains(loaded.Error, "spec drafting failed") || !strings.Contains(loaded.Error, errUnreachableRelay.Error()) {
		t.Errorf("Error = %q, want it to name the job failure", loaded.Error)
	}

	logBytes, err := os.ReadFile(filepath.Join(request.Dir(dataDir, "req-1"), "notifications.log"))
	if err != nil {
		t.Fatalf("read notifications.log: %v", err)
	}
	if !strings.Contains(string(logBytes), "spec drafting failed") {
		t.Errorf("notifications.log = %q, want it to contain the halt reason", string(logBytes))
	}
	// A request's own notification carries RequestID, never its ID in
	// RunID -- see run.NotificationRecord.RequestID.
	if !strings.Contains(string(logBytes), `"request_id":"req-1"`) || strings.Contains(string(logBytes), `"run_id":"req-1"`) {
		t.Errorf("notifications.log = %q, want request_id req-1 and no run_id req-1", string(logBytes))
	}
}

// TestAdvanceSpecDraftingMalformedSkeletonHaltsNamingHeading covers the
// other half of "an empty or malformed draft halts the request with a
// reason": a job that succeeds but returns a spec.md missing a required
// heading must halt the request, naming that heading.
func TestAdvanceSpecDraftingMalformedSkeletonHaltsNamingHeading(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir := t.TempDir()
	if err := request.SaveText(dataDir, "req-1", "text"); err != nil {
		t.Fatal(err)
	}
	r := request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now())
	r.State = request.StateSpecDrafting
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	malformed := "# Spec\n\n## Problem\n\nx\n" // missing every later heading
	runner, _ := stubSpecDraftRunner(malformed, &request.SpecEvidence{}, nil)
	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, runner, requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), requestdrivertest.FailingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}

	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateHalted {
		t.Fatalf("State = %q, want %q", loaded.State, request.StateHalted)
	}
	if !strings.Contains(loaded.Error, "## Scope") {
		t.Errorf("Error = %q, want it to name the missing heading %q", loaded.Error, "## Scope")
	}
	if _, err := os.Stat(requestdriver.RequestSpecPath(dataDir, "req-1")); !os.IsNotExist(err) {
		t.Errorf("spec.md should not be written for an invalid draft (stat err = %v)", err)
	}
}

// TestAdvanceSpecDraftingStripsCommitMessageCriterionBeforePersisting is a
// regression for the 2026-09-17 multi-repo validation finding: a drafted
// spec asking the reviewer to check the commit message/subject line
// itself is unfulfillable by factoryd's own safety-net commit (see
// internal/request.StripCommitMessageCriteria's own doc comment), so the
// mechanical backstop must run before spec.md is ever persisted -- never
// leaving a criterion on disk that a human approves but no later layer
// actually checks.
func TestAdvanceSpecDraftingStripsCommitMessageCriterionBeforePersisting(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir := t.TempDir()
	if err := request.SaveText(dataDir, "req-1", "text"); err != nil {
		t.Fatal(err)
	}
	r := request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now())
	r.State = request.StateSpecDrafting
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	draft := `# Spec

## Problem

Refunds can be double-processed on retry.

## Scope

The /refunds endpoint only.

## Non-goals

Not touching /charges.

## Affected services and packages

internal/payments

## Acceptance criteria

1. A retried POST /refunds with the same idempotency key returns the original result.
2. The commit subject line begins with ` + "`ticket(refunds):`" + `.

## Risks

None known.

## Open questions

None.
`
	runner, _ := stubSpecDraftRunner(draft, &request.SpecEvidence{}, nil)
	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, runner, requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), requestdrivertest.FailingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}

	persisted, err := os.ReadFile(requestdriver.RequestSpecPath(dataDir, "req-1"))
	if err != nil {
		t.Fatalf("read persisted spec.md: %v", err)
	}
	criteria, err := request.SpecAcceptanceCriteria(string(persisted))
	if err != nil {
		t.Fatalf("SpecAcceptanceCriteria(persisted): %v", err)
	}
	if len(criteria) != 1 {
		t.Fatalf("persisted spec.md has %d criteria, want 1 (commit-message criterion should have been stripped): %v", len(criteria), criteria)
	}
	if strings.Contains(strings.ToLower(criteria[0]), "commit") {
		t.Errorf("surviving criterion still mentions commit: %q", criteria[0])
	}
}

// TestAdvanceSpecDraftingHaltsWhenStrippingLeavesNoCriteria is a
// regression for an adversarial-review finding on this same branch:
// ValidateSpecSkeleton only requires the "## Acceptance criteria" section
// to have at least one non-blank line, not a valid numbered item, so a
// draft whose ONLY criterion is commit-message-related would pass that
// first check, then StripCommitMessageCriteria removes it, leaving zero
// criteria -- which must halt the request with a clear reason here, not
// persist an invalid spec.md that only fails confusingly later.
func TestAdvanceSpecDraftingHaltsWhenStrippingLeavesNoCriteria(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir := t.TempDir()
	if err := request.SaveText(dataDir, "req-1", "text"); err != nil {
		t.Fatal(err)
	}
	r := request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now())
	r.State = request.StateSpecDrafting
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	draft := `# Spec

## Problem

Refunds can be double-processed on retry.

## Scope

The /refunds endpoint only.

## Non-goals

Not touching /charges.

## Affected services and packages

internal/payments

## Acceptance criteria

1. The commit subject line begins with ` + "`ticket(refunds):`" + `.

## Risks

None known.

## Open questions

None.
`
	runner, _ := stubSpecDraftRunner(draft, &request.SpecEvidence{}, nil)
	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, runner, requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), requestdrivertest.FailingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}

	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateHalted {
		t.Fatalf("State = %q, want %q", loaded.State, request.StateHalted)
	}
	if !strings.Contains(loaded.Error, "no acceptance criteria left") {
		t.Errorf("Error = %q, want it to explain that stripping left no criteria", loaded.Error)
	}
	if _, err := os.Stat(requestdriver.RequestSpecPath(dataDir, "req-1")); !os.IsNotExist(err) {
		t.Errorf("spec.md should not be persisted when stripping leaves it invalid (stat err = %v)", err)
	}
}

// TestBuildRequestBuildArgsRejectsMalformedPlanCriteria covers the other
// half of writeTicketCriteriaFile's "## Plan" check: a ticket that HAS a
// "## Plan" section but whose "### Acceptance criteria covered" list is
// malformed must halt the build with an error naming the ticket file,
// not silently omit -spec-acceptance-criteria (which would silently
// disable the required per-criterion conformity review).
func TestBuildRequestBuildArgsRejectsMalformedPlanCriteria(t *testing.T) {
	specPath := filepath.Join(t.TempDir(), "001.spec.md")
	malformed := "Verify-Command: pytest\n\n## Goal\n\ndo the thing\n\n## Plan\n\n### Acceptance criteria covered\n\nnot-a-number\n"
	if err := os.WriteFile(specPath, []byte(malformed), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &request.Request{ID: "req-1", Workspace: "/repos/app", Project: "app"}
	ticket := request.Ticket{Index: 1, SpecPath: specPath}

	_, err := requestdriver.BuildRequestBuildArgs("data", r, ticket, requestdriver.WorkerConfig{})
	if err == nil {
		t.Fatal("buildRequestBuildArgs = nil error, want an error naming the malformed ticket")
	}
	if !strings.Contains(err.Error(), specPath) {
		t.Errorf("error = %q, want it to name the ticket file %q", err.Error(), specPath)
	}
}

// TestBuildRequestBuildArgsEmitsPrClosesIssueForIssueSource covers the
// live bug where a request submitted with -issue never carried its
// r.Source.IssueRef onto the synthetic QueueEntry buildRequestBuildArgs
// builds, so buildTicketRunArgs never emitted -pr-closes-issue and the
// ticket's draft PR body lacked its "Closes owner/repo#N" line.
func TestBuildRequestBuildArgsEmitsPrClosesIssueForIssueSource(t *testing.T) {
	specPath := filepath.Join(t.TempDir(), "001.spec.md")
	if err := os.WriteFile(specPath, []byte("Verify-Command: pytest\n\n## Goal\n\ndo the thing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ticket := request.Ticket{Index: 1, SpecPath: specPath}

	withIssue := &request.Request{ID: "req-1", Workspace: "/repos/app", Project: "app", Source: request.Source{Kind: request.SourceIssue, IssueRef: "acme/widgets#42"}}
	args, err := requestdriver.BuildRequestBuildArgs("data", withIssue, ticket, requestdriver.WorkerConfig{})
	if err != nil {
		t.Fatalf("buildRequestBuildArgs: %v", err)
	}
	if !requestdrivertest.ContainsArg(args, "-pr-closes-issue", "acme/widgets#42") {
		t.Errorf("args = %v, want -pr-closes-issue acme/widgets#42", args)
	}

	withoutIssue := &request.Request{ID: "req-2", Workspace: "/repos/app", Project: "app", Source: request.Source{Kind: request.SourceText}}
	args, err = requestdriver.BuildRequestBuildArgs("data", withoutIssue, ticket, requestdriver.WorkerConfig{})
	if err != nil {
		t.Fatalf("buildRequestBuildArgs: %v", err)
	}
	for _, a := range args {
		if a == "-pr-closes-issue" {
			t.Errorf("args = %v, want no -pr-closes-issue flag when the request has no issue source", args)
		}
	}
}

// TestBuildRequestBuildArgsCarriesRequestPreflightProfile pins the live
// bug this guards against: a request submitted with -preflight-profile
// set (explicitly, or via .factory.yml) must have that profile reach the
// ticket's own build argv, exactly like its verify command does --
// otherwise the build silently falls back to the strict default profile
// and halts a brownfield workspace's preflight on artifacts convention
// never asked it to produce. Covers both the present and absent cases in
// one test, table-style, since they're the same code path with only
// r.PreflightProfile varying.
func TestBuildRequestBuildArgsCarriesRequestPreflightProfile(t *testing.T) {
	specPath := filepath.Join(t.TempDir(), "001.spec.md")
	if err := os.WriteFile(specPath, []byte("Verify-Command: pytest\n\n## Goal\n\ndo the thing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ticket := request.Ticket{Index: 1, SpecPath: specPath}

	for _, profile := range []string{"brownfield", ""} {
		r := &request.Request{ID: "req-1", Workspace: "/repos/app", Project: "app", PreflightProfile: profile}
		args, err := requestdriver.BuildRequestBuildArgs("data", r, ticket, requestdriver.WorkerConfig{})
		if err != nil {
			t.Fatalf("buildRequestBuildArgs(PreflightProfile=%q): %v", profile, err)
		}
		var got string
		var found bool
		for i, a := range args {
			if a == "-preflight-profile" {
				found = true
				got = args[i+1]
			}
		}
		if profile == "" {
			if found {
				t.Errorf("PreflightProfile=%q: argv unexpectedly carries -preflight-profile %q", profile, got)
			}
			continue
		}
		if !found || got != profile {
			t.Errorf("PreflightProfile=%q: -preflight-profile = %q (found=%v), want %q", profile, got, found, profile)
		}
	}
}

// TestTicketQueueEntryCarriesExecutionModel proves r.Models["execution"]
// (`factoryd submit -model execution=...`, already validated at submit
// time against roles.execution.allowed) reaches the ticket's own
// QueueEntry.ExecutionModel (ticketQueueEntry), and from there the
// ticket's own build argv as -execution-model (buildTicketRunArgs) --
// exercised together through buildRequestBuildArgs, the same way
// TestBuildRequestBuildArgsCarriesRequestHarness below covers Harness.
// An unset r.Models leaves the argv with no -execution-model at all, no
// behavior change from before this field existed.
func TestTicketQueueEntryCarriesExecutionModel(t *testing.T) {
	specPath := filepath.Join(t.TempDir(), "001.spec.md")
	if err := os.WriteFile(specPath, []byte("Verify-Command: pytest\n\n## Goal\n\ndo the thing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ticket := request.Ticket{Index: 1, SpecPath: specPath}

	for _, model := range []string{"sonnet", ""} {
		r := &request.Request{ID: "req-1", Workspace: "/repos/app", Project: "app"}
		if model != "" {
			r.Models = map[string]string{"execution": model}
		}
		args, err := requestdriver.BuildRequestBuildArgs("data", r, ticket, requestdriver.WorkerConfig{})
		if err != nil {
			t.Fatalf("buildRequestBuildArgs(Models[execution]=%q): %v", model, err)
		}
		var got string
		var found bool
		for i, a := range args {
			if a == "-execution-model" {
				found = true
				got = args[i+1]
			}
		}
		if model == "" {
			if found {
				t.Errorf("Models unset: argv unexpectedly carries -execution-model %q", got)
			}
			continue
		}
		if !found || got != model {
			t.Errorf("Models[execution]=%q: -execution-model = %q (found=%v), want %q", model, got, found, model)
		}
	}
}

// TestBuildTicketRunArgsForwardsExecutionModel is buildTicketRunArgs' own
// unit-level proof (rather than through buildRequestBuildArgs above): a
// QueueEntry.ExecutionModel forwards as -execution-model, and an empty
// one forwards nothing.
func TestBuildTicketRunArgsForwardsExecutionModel(t *testing.T) {
	entry := &requestdriver.QueueEntry{ID: "t", Workspace: "/repos/app", SpecPath: "/repos/app/spec.md", VerifyCommand: "make verify", ExecutionModel: "sonnet"}
	args := requestdriver.BuildTicketRunArgs("data", entry, requestdriver.WorkerConfig{})
	var got string
	var found bool
	for i, a := range args {
		if a == "-execution-model" {
			found = true
			got = args[i+1]
		}
	}
	if !found || got != "sonnet" {
		t.Errorf("-execution-model = %q (found=%v), want %q", got, found, "sonnet")
	}

	entry.ExecutionModel = ""
	args = requestdriver.BuildTicketRunArgs("data", entry, requestdriver.WorkerConfig{})
	for _, a := range args {
		if a == "-execution-model" {
			t.Errorf("argv unexpectedly carries -execution-model with an empty QueueEntry.ExecutionModel")
		}
	}
}

// TestBuildRequestBuildArgsCarriesRequestHarness is a regression test:
// `factoryd submit -harness execution=<name>`, recorded on request.Request,
// must reach the ticket's own build argv as -execution-harness -- and an
// unset choice (or a planning-only one) must leave
// the argv byte-for-byte identical to a request that never had the field
// (no behavior change when unset, by design).
func TestBuildRequestBuildArgsCarriesRequestHarness(t *testing.T) {
	specPath := filepath.Join(t.TempDir(), "001.spec.md")
	if err := os.WriteFile(specPath, []byte("Verify-Command: pytest\n\n## Goal\n\ndo the thing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ticket := request.Ticket{Index: 1, SpecPath: specPath}
	cfg := requestdriver.WorkerConfig{}

	withOverrides := &request.Request{ID: "req-1", Workspace: "/repos/app", Project: "app", Harnesses: map[string]string{"execution": "pifork", "planning": "pi"}}
	args, err := requestdriver.BuildRequestBuildArgs("data", withOverrides, ticket, cfg)
	if err != nil {
		t.Fatalf("buildRequestBuildArgs: %v", err)
	}
	if !requestdrivertest.ContainsArg(args, "-execution-harness", "pifork") {
		t.Errorf("args = %v, want -execution-harness pifork", args)
	}

	unset := &request.Request{ID: "req-2", Workspace: "/repos/app", Project: "app"}
	unsetArgs, err := requestdriver.BuildRequestBuildArgs("data", unset, ticket, cfg)
	if err != nil {
		t.Fatalf("buildRequestBuildArgs: %v", err)
	}
	for _, a := range unsetArgs {
		if a == "-execution-harness" {
			t.Errorf("unset Harnesses: args = %v, want no -execution-harness flag", unsetArgs)
		}
	}
}

// newApprovedOracleFixture writes a ticket spec plus a populated,
// plan_review-approved <NNN>.oracle/ directory (oracle_001.go +
// RUN_COMMAND.txt, both hash-recorded in r.ApprovedSHA256 the same way
// a real Approve call would) -- the well-formed baseline
// TestResolveTicketOracle*'s individual cases each perturb one way.
func newApprovedOracleFixture(t *testing.T, dataDir string) (*request.Request, request.Ticket) {
	t.Helper()
	r := &request.Request{ID: "req-1", Workspace: "/repos/app", Project: "app"}
	ticketPath := filepath.Join(request.Dir(dataDir, r.ID), "tickets", "001.spec.md")
	if err := os.MkdirAll(filepath.Dir(ticketPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ticketPath, []byte("Verify-Command: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oracleDir := filepath.Join(request.Dir(dataDir, r.ID), "tickets", "001.oracle")
	if err := os.MkdirAll(oracleDir, 0o750); err != nil {
		t.Fatal(err)
	}
	r.ApprovedSHA256 = map[string]string{}
	for name, content := range map[string]string{
		"oracle_001.go": "package x\n",
		requestdriver.TicketOracleRunCommandFilename: "go test ./.oracle/...\n",
	} {
		path := filepath.Join(oracleDir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		relPath := filepath.Join("tickets", "001.oracle", name)
		hash, err := request.HashFile(dataDir, r.ID, relPath)
		if err != nil {
			t.Fatal(err)
		}
		r.ApprovedSHA256[relPath] = hash
	}
	return r, request.Ticket{Index: 1, SpecPath: ticketPath}
}

func TestResolveTicketOracleReturnsEmptyWhenNoOracleDirectory(t *testing.T) {
	dataDir := t.TempDir()
	r := &request.Request{ID: "req-1"}
	ticketPath := filepath.Join(request.Dir(dataDir, r.ID), "tickets", "001.spec.md")
	if err := os.MkdirAll(filepath.Dir(ticketPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ticketPath, []byte("Verify-Command: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	oracleDir, command, err := requestdriver.ResolveTicketOracle(dataDir, r, request.Ticket{Index: 1, SpecPath: ticketPath})
	if err != nil {
		t.Fatalf("resolveTicketOracle: %v", err)
	}
	if oracleDir != "" || command != "" {
		t.Fatalf("resolveTicketOracle = (%q, %q), want empty (no oracle directory exists)", oracleDir, command)
	}
}

func TestResolveTicketOracleReturnsDirAndCommandWhenApproved(t *testing.T) {
	dataDir := t.TempDir()
	r, ticket := newApprovedOracleFixture(t, dataDir)

	oracleDir, command, err := requestdriver.ResolveTicketOracle(dataDir, r, ticket)
	if err != nil {
		t.Fatalf("resolveTicketOracle: %v", err)
	}
	wantDir := filepath.Join(request.Dir(dataDir, r.ID), "tickets", "001.oracle")
	if oracleDir != wantDir {
		t.Errorf("oracleDir = %q, want %q", oracleDir, wantDir)
	}
	if command != "go test ./.oracle/..." {
		t.Errorf("command = %q, want %q", command, "go test ./.oracle/...")
	}
}

func TestResolveTicketOracleRefusesUnapprovedFile(t *testing.T) {
	dataDir := t.TempDir()
	r, ticket := newApprovedOracleFixture(t, dataDir)
	// A file added to the oracle directory after approval -- never
	// hash-recorded, must not be silently trusted.
	oracleDir := filepath.Join(request.Dir(dataDir, r.ID), "tickets", "001.oracle")
	if err := os.WriteFile(filepath.Join(oracleDir, "sneaked_in.go"), []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := requestdriver.ResolveTicketOracle(dataDir, r, ticket); err == nil {
		t.Fatal("resolveTicketOracle with an unapproved file present: want an error, got nil")
	}
}

func TestResolveTicketOracleRefusesHashMismatch(t *testing.T) {
	dataDir := t.TempDir()
	r, ticket := newApprovedOracleFixture(t, dataDir)
	oracleDir := filepath.Join(request.Dir(dataDir, r.ID), "tickets", "001.oracle")
	// Edited after approval -- the exact tamper VerifyApprovedHashes-style
	// re-verification exists to catch.
	if err := os.WriteFile(filepath.Join(oracleDir, "oracle_001.go"), []byte("package x // tampered\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := requestdriver.ResolveTicketOracle(dataDir, r, ticket)
	if err == nil {
		t.Fatal("resolveTicketOracle with a tampered file: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "changed since plan_review approved") {
		t.Errorf("error = %v, want it to name the tamper", err)
	}
}

// TestResolveTicketOracleRefusesASubdirectory is the regression test for
// a real finding (found via review): the actual build-time mount
// (evidence.SnapshotTree, run_ticket.go's snapshotAndHashReferenceOracle)
// walks a ticket's own oracle directory RECURSIVELY, so silently
// skipping a subdirectory here (as an earlier version did) would let
// content that was never hash-verified reach a real build. A
// subdirectory must refuse resolution outright.
func TestResolveTicketOracleRefusesASubdirectory(t *testing.T) {
	dataDir := t.TempDir()
	r, ticket := newApprovedOracleFixture(t, dataDir)
	oracleDir := filepath.Join(request.Dir(dataDir, r.ID), "tickets", "001.oracle")
	if err := os.MkdirAll(filepath.Join(oracleDir, "nested"), 0o750); err != nil {
		t.Fatal(err)
	}

	_, _, err := requestdriver.ResolveTicketOracle(dataDir, r, ticket)
	if err == nil {
		t.Fatal("resolveTicketOracle with a subdirectory present: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "subdirectory") {
		t.Errorf("error = %v, want it to name the subdirectory", err)
	}
}

func TestResolveTicketOracleRefusesMissingRunCommand(t *testing.T) {
	dataDir := t.TempDir()
	r := &request.Request{ID: "req-1"}
	ticketPath := filepath.Join(request.Dir(dataDir, r.ID), "tickets", "001.spec.md")
	if err := os.MkdirAll(filepath.Dir(ticketPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ticketPath, []byte("Verify-Command: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oracleDir := filepath.Join(request.Dir(dataDir, r.ID), "tickets", "001.oracle")
	if err := os.MkdirAll(oracleDir, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(oracleDir, "oracle_001.go")
	if err := os.WriteFile(path, []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	relPath := filepath.Join("tickets", "001.oracle", "oracle_001.go")
	hash, err := request.HashFile(dataDir, r.ID, relPath)
	if err != nil {
		t.Fatal(err)
	}
	r.ApprovedSHA256 = map[string]string{relPath: hash}
	// RUN_COMMAND.txt deliberately never written.

	_, _, err = requestdriver.ResolveTicketOracle(dataDir, r, request.Ticket{Index: 1, SpecPath: ticketPath})
	if err == nil {
		t.Fatal("resolveTicketOracle with no RUN_COMMAND.txt: want an error, got nil")
	}
	if !strings.Contains(err.Error(), requestdriver.TicketOracleRunCommandFilename) {
		t.Errorf("error = %v, want it to name %s", err, requestdriver.TicketOracleRunCommandFilename)
	}
}

// TestBuildRequestBuildArgsForwardsReferenceOracleFlags is the
// end-to-end regression: an approved, hash-verified ticket oracle
// reaches the child factoryd <run> invocation's own reference-oracle
// flags, all four together.
func TestBuildRequestBuildArgsForwardsReferenceOracleFlags(t *testing.T) {
	dataDir := t.TempDir()
	r, ticket := newApprovedOracleFixture(t, dataDir)

	args, err := requestdriver.BuildRequestBuildArgs(dataDir, r, ticket, requestdriver.WorkerConfig{})
	if err != nil {
		t.Fatalf("buildRequestBuildArgs: %v", err)
	}
	got := map[string]string{}
	inLoopRetry := false
	for i, a := range args {
		switch a {
		case "-reference-oracle-dir", "-reference-oracle-mount-path", "-reference-oracle-command":
			got[a] = args[i+1]
		case "-reference-oracle-in-loop-retry":
			inLoopRetry = true
		}
	}
	wantDir := filepath.Join(request.Dir(dataDir, r.ID), "tickets", "001.oracle")
	if got["-reference-oracle-dir"] != wantDir {
		t.Errorf("-reference-oracle-dir = %q, want %q", got["-reference-oracle-dir"], wantDir)
	}
	if got["-reference-oracle-mount-path"] != requestdriver.TicketOracleMountPath {
		t.Errorf("-reference-oracle-mount-path = %q, want %q", got["-reference-oracle-mount-path"], requestdriver.TicketOracleMountPath)
	}
	if got["-reference-oracle-command"] != "go test ./.oracle/..." {
		t.Errorf("-reference-oracle-command = %q, want %q", got["-reference-oracle-command"], "go test ./.oracle/...")
	}
	if !inLoopRetry {
		t.Error("-reference-oracle-in-loop-retry was not set")
	}
	if containsFlag(args, "-no-commit-oracles") {
		t.Error("-no-commit-oracles forwarded although the request did not opt out")
	}
	r.NoCommitOracles = true
	args, err = requestdriver.BuildRequestBuildArgs(dataDir, r, ticket, requestdriver.WorkerConfig{})
	if err != nil {
		t.Fatalf("buildRequestBuildArgs: %v", err)
	}
	if !containsFlag(args, "-no-commit-oracles") {
		t.Error("-no-commit-oracles not forwarded for an opted-out request")
	}
}

// TestBuildRequestBuildArgsOmitsReferenceOracleFlagsWhenTicketHasNone is
// the converse: the ordinary case (no <NNN>.oracle/ directory at all)
// must not emit any reference-oracle flag, identical to a ticket built
// before this mechanism existed.
func TestBuildRequestBuildArgsOmitsReferenceOracleFlagsWhenTicketHasNone(t *testing.T) {
	dataDir := t.TempDir()
	r := &request.Request{ID: "req-1", Workspace: "/repos/app", Project: "app"}
	ticketPath := filepath.Join(request.Dir(dataDir, r.ID), "tickets", "001.spec.md")
	if err := os.MkdirAll(filepath.Dir(ticketPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ticketPath, []byte("Verify-Command: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	args, err := requestdriver.BuildRequestBuildArgs(dataDir, r, request.Ticket{Index: 1, SpecPath: ticketPath}, requestdriver.WorkerConfig{})
	if err != nil {
		t.Fatalf("buildRequestBuildArgs: %v", err)
	}
	for _, a := range args {
		if strings.HasPrefix(a, "-reference-oracle") {
			t.Fatalf("args = %v, want no -reference-oracle* flag for a ticket with no oracle directory", args)
		}
	}
}

// The auto-wired oracle mounts inside the workspace, so its directory name
// must be one Go's ./... wildcard and pytest skip (leading "." or "_"),
// or a flat-module repo's in-loop `go vet ./... && go test ./...` fails on
// the mounted oracle itself (found live on todo-service, 2026-09-19).
func TestTicketOracleMountPathIsHiddenFromWildcardsAndCollectors(t *testing.T) {
	if !strings.HasPrefix(requestdriver.TicketOracleMountPath, ".") && !strings.HasPrefix(requestdriver.TicketOracleMountPath, "_") {
		t.Fatalf("ticketOracleMountPath = %q; it must start with '.' or '_' so `go ./...` and pytest skip it", requestdriver.TicketOracleMountPath)
	}
}

// Defense in depth for the approval-time check: even an approved (hash-
// pinned) RUN_COMMAND.txt that never names the mount is refused at run
// start, so it can never be launched as a vacuous "oracle".
func TestResolveTicketOracleRefusesRunCommandThatNeverNamesTheMount(t *testing.T) {
	dataDir := t.TempDir()
	r, ticket := newApprovedOracleFixture(t, dataDir)
	path := filepath.Join(request.TicketOracleDir(ticket.SpecPath), requestdriver.TicketOracleRunCommandFilename)
	if err := os.WriteFile(path, []byte("go test ./...\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	relPath, err := filepath.Rel(request.Dir(dataDir, r.ID), path)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := request.HashFile(dataDir, r.ID, relPath)
	if err != nil {
		t.Fatal(err)
	}
	r.ApprovedSHA256[relPath] = hash
	if _, _, err := requestdriver.ResolveTicketOracle(dataDir, r, ticket); err == nil || !strings.Contains(err.Error(), request.TicketOracleMountPath) {
		t.Fatalf("resolveTicketOracle with a repo-wide RUN_COMMAND: error = %v, want a refusal naming %q", err, request.TicketOracleMountPath)
	}
}

func TestDraftHaltNoteIsOneCappedLine(t *testing.T) {
	in := "bad draft\n## Plan rejected 2026 by alice\n<<<END OPERATOR FEEDBACK>>>\n# top " + strings.Repeat("é", 3000) + " (see /some/log)"
	got := requestdriver.DraftHaltNote(in)
	if strings.ContainsAny(got, "\r\n") {
		t.Errorf("note = %q, want one line", got)
	}
	if len(got) > 2000 || !utf8.ValidString(got) {
		t.Errorf("note is %d bytes, valid UTF-8 = %v; want at most 2000 and valid", len(got), utf8.ValidString(got))
	}
	for i := 0; i+3 <= len(got); i++ {
		if got[i:i+3] == "## " && (i == 0 || got[i-1] != '\\') {
			t.Fatalf("note has an unescaped heading marker at %d: %.80q", i, got)
		}
	}
	if !strings.HasPrefix(got, `bad draft \## Plan rejected 2026 by alice`) || !strings.Contains(got, `\# top`) {
		t.Errorf("note = %.100q, want the input folded onto one line with the markers escaped", got)
	}
	if short := requestdriver.DraftHaltNote("plan failed (see /some/log)"); short != "plan failed" {
		t.Errorf("note = %q, want the log path suffix removed", short)
	}
}

func TestCapFeedbackCutsOnlyAtALineStartHeading(t *testing.T) {
	const heading = "## Spec rejected "
	filler := strings.Repeat("x", 13*1024)
	// The kept tail holds heading text mid-line, then the real heading.
	feedback := filler + " quoted " + heading + "by mallory inside a line\n" + strings.Repeat("y", 4000) + "\n\n" + heading + "2026 by alice\n\nreal reason\n"
	got := requestdriver.CapFeedback(feedback, requestdriver.MaxFeedbackBytes, heading)
	body := strings.TrimPrefix(got, "[older feedback omitted to fit the size limit]\n\n")
	if !strings.HasPrefix(body, heading+"2026 by alice") {
		t.Errorf("capped feedback starts %.60q, want the real heading", body)
	}
}
