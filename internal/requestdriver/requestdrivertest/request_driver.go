package requestdrivertest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/handoff"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/run"
)

// FailingSpecDraftRunner fails the test outright if ever called -- used
// where driveRequests must not touch the spec-drafting job at all (e.g. a
// request already past spec_drafting, or the pure submitted->
// spec_drafting move which needs no job).
func FailingSpecDraftRunner(t *testing.T) requestdriver.SpecDraftRunner {
	return func(ctx context.Context, dataDir string, r *request.Request, cfg requestdriver.WorkerConfig) (string, *request.SpecEvidence, error) {
		t.Fatal("specDraftRunner must not be called")
		return "", nil, nil
	}
}

// FailingPlanTicketsRunner is FailingSpecDraftRunner's own sibling for
// the plan-drafting job -- used everywhere driveRequests must not touch
// planning at all.
func FailingPlanTicketsRunner(t *testing.T) requestdriver.PlanTicketsRunner {
	return func(ctx context.Context, dataDir string, r *request.Request, cfg requestdriver.WorkerConfig, verifyCommand string) ([]requestdriver.DraftedTicket, *request.PlanEvidence, error) {
		t.Fatal("planTicketsRunner must not be called")
		return nil, nil, nil
	}
}

// FailingOracleDraftRunner is FailingSpecDraftRunner's sibling for the oracle
// drafting job -- used everywhere driveRequests must not touch
// oracle_drafting (every pre-existing test: no request there sets
// -draft-oracles).
func FailingOracleDraftRunner(t *testing.T) requestdriver.OracleDraftRunner {
	return func(ctx context.Context, in requestdriver.OracleDraftInput) (request.OracleDraft, error) {
		t.Fatal("oracleDraftRunner must not be called")
		return request.OracleDraft{}, nil
	}
}

// FailingBuildRunner is FailingSpecDraftRunner's own sibling for a
// ticket build (ticketRunner) -- used everywhere driveRequests must not
// touch building at all.
func FailingBuildRunner(t *testing.T) requestdriver.TicketRunner {
	return func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		t.Fatal("ticketRunner (ticket build) must not be called")
		return nil
	}
}

// ArgValue returns the value following flag in args, or "" if flag is
// absent or has no following value.
func ArgValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// HasFlag reports whether the bare flag (a boolean flag with no value,
// e.g. -open-pull-request) is present in args.
func HasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// NewApprovableDriverRequest mirrors internal/request's own
// newApprovableRequest test helper, kept package-local since it writes
// the same fixture shape approveMain/rejectMain/verifyApprovedHashes all
// need but internal/request's helper is unexported.
func NewApprovableDriverRequest(t *testing.T, dataDir, id string, state request.State) {
	t.Helper()
	if err := request.SaveText(dataDir, id, "the original request text"); err != nil {
		t.Fatalf("SaveText: %v", err)
	}
	r := request.New(id, "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now())
	r.State = state
	if err := os.WriteFile(requestdriver.RequestSpecPath(dataDir, id), []byte("# Spec\n"), 0o600); err != nil {
		t.Fatalf("write spec.md: %v", err)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

// QuarantinedOn builds a quarantined run that failed failedChecks, with its
// handoff written and hashed as cmd/factoryd's save does.
func QuarantinedOn(t *testing.T, dataDir, id, branch, baseSHA, resultSHA string, failedChecks ...string) *run.Run {
	t.Helper()
	failed := false
	rr := &run.Run{
		ID: id, Ticket: id, State: run.StateQuarantined, Branch: branch, BaseSHA: baseSHA, ResultSHA: resultSHA,
		ChangedFiles: []string{"sum.go"},
		AgentEvidence: &run.AgentEvidence{Rounds: []run.AgentEvidenceRound{
			{Index: 1, VerifyPassed: &failed, Blockers: []string{"canonical verification failed"}, ChangedFiles: []string{"sum.go"}, FailureSignature: "aaaa"},
		}},
	}
	for _, check := range failedChecks {
		rr.GateResults = append(rr.GateResults, run.GateResult{Check: check, Passed: false, ExitCode: 2})
	}
	if err := handoff.Sync(rr, dataDir); err != nil {
		t.Fatalf("write the handoff: %v", err)
	}
	return rr
}

// SaveRunFixture is monthToDateSpend's own test fixture builder: a
// minimal run.Run with one attempt whose StartedAt/relay-consumed figures
// are set directly, saved under dataDir.
func SaveRunFixture(t *testing.T, dataDir, id, requestID, startedAt string, tokens, costMicroUSD int64) {
	t.Helper()
	r := &run.Run{
		ID:        id,
		RequestID: requestID,
		Attempts: []run.Attempt{
			{
				StartedAt:                 startedAt,
				RelayConsumedInputTokens:  tokens,
				RelayConsumedOutputTokens: 0,
				RelayConsumedCostMicroUSD: costMicroUSD,
			},
		},
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("save run fixture %q: %v", id, err)
	}
}

// OracleStageFixture writes a spec_review request (spec.md present, workspace
// with a .factory.yml verify_command) and approves the spec, so the returned
// request sits wherever spec approval routes it: oracle_drafting when
// draftOracles, planning otherwise.
func OracleStageFixture(t *testing.T, draftOracles bool) (dataDir, id string) {
	t.Helper()
	dataDir = t.TempDir()
	id = "req-1"
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, ".factory.yml"), []byte("verify_command: make verify\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := request.SaveText(dataDir, id, "some request text"); err != nil {
		t.Fatal(err)
	}
	r := request.New(id, workspace, "app", request.Source{Kind: request.SourceText}, time.Now())
	r.State = request.StateSpecReview
	r.DraftOracles = draftOracles
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(requestdriver.RequestSpecPath(dataDir, id), []byte(TwoCriteriaSpec), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := request.Approve(dataDir, id, "alice", time.Now(), nil); err != nil {
		t.Fatalf("approve spec: %v", err)
	}
	return dataDir, id
}

// NoOracleScriptCfg points the production drafting runner at a script that
// does not exist, so it records not_implemented instead of launching a
// sandbox: the state-machine tests here exercise routing, not drafting.
var NoOracleScriptCfg = requestdriver.WorkerConfig{OracleDraftScript: "/nonexistent/draft_acceptance_oracles.py"}

func LoadRequest(t *testing.T, dataDir, id string) *request.Request {
	t.Helper()
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func StubOracleDraftRunner(draft request.OracleDraft, err error) (requestdriver.OracleDraftRunner, *[]requestdriver.OracleDraftInput) {
	var calls []requestdriver.OracleDraftInput
	return func(ctx context.Context, in requestdriver.OracleDraftInput) (request.OracleDraft, error) {
		calls = append(calls, in)
		return draft, err
	}, &calls
}

func MoveToOracleReview(t *testing.T, dataDir, id string, draft request.OracleDraft) {
	t.Helper()
	r := LoadRequest(t, dataDir, id)
	if err := r.CompleteOracleDrafting(draft, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
}

const HandoverVerify = "python3 -m unittest tests/test_product_lab.py"

// HandedOverPlanFixture is a request in planning (spec approved) whose
// tickets the operator handed over, stored where Submit stores them.
func HandedOverPlanFixture(t *testing.T, tickets map[string]string) (dataDir, id string) {
	t.Helper()
	dataDir, id = ApprovedPlanningFixtureNoFactoryYML(t, TwoCriteriaSpec, HandoverVerify)
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	r.PlanImported = true
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	dir := request.ImportedTicketsDir(dataDir, id)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	for name, content := range tickets {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dataDir, id
}

const CanonicalValidSpec = `# Spec

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

## Risks

None known.

## Open questions

None.
`

// TwoCriteriaSpec is CanonicalValidSpec's own sibling with two acceptance
// criteria, used by the planning tests below to exercise coverage across
// one or two tickets.
const TwoCriteriaSpec = `# Spec

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
2. A non-idempotent POST /refunds still processes normally.

## Risks

None known.

## Open questions

None.
`

func ValidBrownfieldTicket(verifyCommand string, criteria ...int) string {
	criteriaLines := ""
	for _, n := range criteria {
		criteriaLines += fmt.Sprintf("- %d\n", n)
	}
	return fmt.Sprintf(`Verify-Command: %s
Allowed-Files: internal/payments/refunds.go, internal/payments/refunds_test.go
Required-Changed-Files: internal/payments/refunds.go

## Goal

Fix double-processing.

## Plan

### Files to touch

- internal/payments/refunds.go

### Steps

1. Add an idempotency check.

### Tests to add

- internal/payments/refunds_test.go

### Acceptance criteria covered

%s
## Out of scope

Nothing else.
`, verifyCommand, criteriaLines)
}

// StubPlanTicketsRunner mirrors stubSpecDraftRunner's own shape for the
// plan-drafting job, recording the verifyCommand it was actually called
// with alongside a fixed (tickets, evidence, err) return.
func StubPlanTicketsRunner(tickets []requestdriver.DraftedTicket, evidence *request.PlanEvidence, err error) (requestdriver.PlanTicketsRunner, *string) {
	var gotVerifyCommand string
	runner := func(ctx context.Context, dataDir string, r *request.Request, cfg requestdriver.WorkerConfig, verifyCommand string) ([]requestdriver.DraftedTicket, *request.PlanEvidence, error) {
		gotVerifyCommand = verifyCommand
		return tickets, evidence, err
	}
	return runner, &gotVerifyCommand
}

// ApprovedPlanningFixture creates a request already in planning (spec.md
// written and approved, exactly what verifyApprovedHashes needs to pass)
// with a workspace declaring verifyCommand in .factory.yml, returning
// dataDir and the request id.
func ApprovedPlanningFixture(t *testing.T, specContent, verifyCommand string) (dataDir, id string) {
	t.Helper()
	dataDir = t.TempDir()
	id = "req-1"
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, ".factory.yml"), []byte("verify_command: "+verifyCommand+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := request.SaveText(dataDir, id, "some request text"); err != nil {
		t.Fatal(err)
	}
	r := request.New(id, workspace, "app", request.Source{Kind: request.SourceText}, time.Now())
	r.State = request.StateSpecReview
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(requestdriver.RequestSpecPath(dataDir, id), []byte(specContent), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := request.Approve(dataDir, id, "alice", time.Now(), nil); err != nil {
		t.Fatalf("approve spec: %v", err)
	}
	return dataDir, id
}

// ApprovedPlanningFixtureNoFactoryYML mirrors ApprovedPlanningFixture
// exactly, except the workspace has no .factory.yml at all -- the request
// itself carries verifyCommand as r.VerifyCommand instead, exactly as
// `factoryd submit -verify-command ...` would record it against a
// workspace with no verify_command of its own. Used to pin that
// advancePlanning prefers r.VerifyCommand over re-reading (a nonexistent)
// .factory.yml.
func ApprovedPlanningFixtureNoFactoryYML(t *testing.T, specContent, verifyCommand string) (dataDir, id string) {
	t.Helper()
	dataDir = t.TempDir()
	id = "req-1"
	workspace := t.TempDir()
	if err := request.SaveText(dataDir, id, "some request text"); err != nil {
		t.Fatal(err)
	}
	r := request.New(id, workspace, "app", request.Source{Kind: request.SourceText}, time.Now())
	r.VerifyCommand = verifyCommand
	r.State = request.StateSpecReview
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(requestdriver.RequestSpecPath(dataDir, id), []byte(specContent), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := request.Approve(dataDir, id, "alice", time.Now(), nil); err != nil {
		t.Fatalf("approve spec: %v", err)
	}
	return dataDir, id
}

// CountNotificationLogLines counts the JSON-lines entries in a request's
// own notifications.log, or 0 if the file doesn't exist yet.
func CountNotificationLogLines(t *testing.T, dataDir, id string) int {
	t.Helper()
	b, err := os.ReadFile(requestdriver.RequestNotificationLogPath(dataDir, id))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read notifications.log: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return 0
	}
	return len(lines)
}

// BuildingFixture drives a request all the way from an approved spec
// (ApprovedPlanningFixture) through planning (n tickets, each claiming
// TwoCriteriaSpec's own first criterion, real files under
// <dataDir>/requests/<id>/tickets/) and a plan approval (plan_review ->
// building), returning it ready for advanceBuilding/driveRequests to pick
// up ticket 1.
func BuildingFixture(dp requestdriver.Deps, t *testing.T, n int) (dataDir, id string) {
	t.Helper()
	dataDir, id = ApprovedPlanningFixture(t, TwoCriteriaSpec, "make verify")
	tickets := make([]requestdriver.DraftedTicket, 0, n)
	for i := 1; i <= n; i++ {
		// Each ticket claims both of TwoCriteriaSpec's own acceptance
		// criteria -- simpler than splitting them across tickets, and
		// ValidatePlanCoverage only requires every criterion be claimed by
		// at least one ticket, not by exactly one.
		tickets = append(tickets, requestdriver.DraftedTicket{
			Filename: fmt.Sprintf("%03d.spec.md", i),
			Content:  ValidBrownfieldTicket("make verify", 1, 2),
		})
	}
	runner, _ := StubPlanTicketsRunner(tickets, &request.PlanEvidence{}, nil)
	if err := DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, FailingSpecDraftRunner(t), runner, FailingOracleDraftRunner(t), FailingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests (planning): %v", err)
	}
	if _, err := request.Approve(dataDir, id, "alice", time.Now(), nil); err != nil {
		t.Fatalf("approve plan: %v", err)
	}
	return dataDir, id
}

// TicketRunID is BuildingFixture's own ticket-id convention, matching
// buildRequestBuildArgs' "<request-id>-<index, %03d>".
func TicketRunID(id string, index int) string {
	return fmt.Sprintf("%s-%03d", id, index)
}

// AcceptingBuildRunner is a stub ticketRunner that fires onReady with the
// run id runMainWithReady would actually assign (the -ticket argument
// itself -- see buildRequestBuildArgs), then durably records that run as
// accepted with a synthesized PR URL, mirroring what a real ticket build
// leaves behind for advanceBuilding to read back via run.Load.
func AcceptingBuildRunner(t *testing.T, dataDir string) requestdriver.TicketRunner {
	return func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		ticket := ArgValue(args, "-ticket")
		if onReady != nil {
			onReady(&run.Run{ID: ticket, State: run.StateReady})
		}
		rr := &run.Run{ID: ticket, State: run.StateAccepted, BaseSHA: fmt.Sprintf("%040d", 1), PullRequestURL: "https://github.com/acme/app/pull/" + ticket}
		if err := rr.Save(dataDir); err != nil {
			t.Fatalf("save stub run %q: %v", ticket, err)
		}
		return nil
	}
}

const MaterializerOracleBody = "package x\n\nimport \"testing\"\n\nfunc TestOracleX(t *testing.T) {}\n"

// RequestOracleFixtureAtReview builds a -draft-oracles request that has passed
// oracle_review with the given request-level oracle (nothing written when
// files is nil, i.e. the operator skipped the stage) and sits in planning.
func RequestOracleFixtureAtReview(t *testing.T, files map[string]string) (dataDir, id string) {
	t.Helper()
	dataDir = t.TempDir()
	id = "req-1"
	workspace := t.TempDir()
	if err := request.SaveText(dataDir, id, "some request text"); err != nil {
		t.Fatal(err)
	}
	r := request.New(id, workspace, "app", request.Source{Kind: request.SourceText}, time.Now())
	r.VerifyCommand = "make verify"
	r.DraftOracles = true
	r.State = request.StateSpecReview
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(requestdriver.RequestSpecPath(dataDir, id), []byte(TwoCriteriaSpec), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := request.Approve(dataDir, id, "alice", time.Now(), nil); err != nil {
		t.Fatalf("approve spec: %v", err)
	}
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if r.State != request.StateOracleDrafting {
		t.Fatalf("state %s, want oracle_drafting", r.State)
	}
	r.State = request.StateOracleReview
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if files != nil {
		dir := filepath.Join(request.Dir(dataDir, id), request.RequestOracleDirName)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		for n, b := range files {
			if err := os.WriteFile(filepath.Join(dir, n), []byte(b), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	return dataDir, id
}

// RequestOracleFixture is RequestOracleFixtureAtReview plus the oracle_review
// approval.
func RequestOracleFixture(t *testing.T, files map[string]string) (dataDir, id string) {
	t.Helper()
	dataDir, id = RequestOracleFixtureAtReview(t, files)
	if _, err := request.Approve(dataDir, id, "alice", time.Now(), nil); err != nil {
		t.Fatalf("approve oracle_review: %v", err)
	}
	return dataDir, id
}

func TwoOracleFiles(command string) map[string]string {
	manifest, _ := json.Marshal([]map[string]any{
		{"criterion": "A retried POST /refunds with the same idempotency key returns the original result.", "oracle_file": "retry_oracle_test.go", "criterion_index": 1, "target_path": "internal/payments/retry_oracle_test.go", "supersedes": []string{"internal/payments/old_oracle_test.go"}},
		{"criterion": "2. A non-idempotent POST /refunds still processes normally.", "oracle_file": "plain_oracle_test.go", "criterion_index": 2},
	})
	return map[string]string{
		"MANIFEST.json":        string(manifest),
		"RUN_COMMAND.txt":      command,
		"retry_oracle_test.go": strings.Replace(MaterializerOracleBody, "package x", "package payments", 1), // fits its target_path dir
		"plain_oracle_test.go": MaterializerOracleBody,
	}
}

func PlanTwoTickets(dp requestdriver.Deps, t *testing.T, dataDir string) *request.Request {
	t.Helper()
	tickets := []requestdriver.DraftedTicket{
		{Filename: "001.spec.md", Content: ValidBrownfieldTicket("make verify", 1)},
		{Filename: "002.spec.md", Content: ValidBrownfieldTicket("make verify", 2)},
	}
	runner, _ := StubPlanTicketsRunner(tickets, &request.PlanEvidence{}, nil)
	if err := DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, FailingSpecDraftRunner(t), runner, FailingOracleDraftRunner(t), FailingBuildRunner(t)); err != nil {
		t.Fatal(err)
	}
	r, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// ReviewQuarantinedBuildRunner returns a stub ticketRunner mimicking a
// ticket's first build quarantining with spec_conformity as the only
// failed gate (extraGates, if any, are additionally recorded as passed).
func ReviewQuarantinedBuildRunner(t *testing.T, dataDir, branch, baseSHA string, extraFailedGate string) requestdriver.TicketRunner {
	t.Helper()
	return func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		ticket := ArgValue(args, "-ticket")
		if onReady != nil {
			onReady(&run.Run{ID: ticket})
		}
		gates := []run.GateResult{
			{Check: "canonical_verify", Passed: true},
			{Check: "diff_scope", Passed: true},
		}
		if extraFailedGate != "" {
			gates = append(gates, run.GateResult{Check: extraFailedGate, Passed: false})
		}
		gates = append(gates, run.GateResult{Check: "spec_conformity", Passed: false})
		rr := &run.Run{
			ID:          ticket,
			State:       run.StateQuarantined,
			HaltError:   "gate failed: spec_conformity",
			Branch:      branch,
			BaseSHA:     baseSHA,
			GateResults: gates,
			SpecConformityVerdicts: []run.ReviewVerdict{
				{Criterion: "1. handles empty input", Verdict: "clean"},
				{Criterion: "2. rejects a negative amount", Verdict: "flagged", Detail: "no test covers a negative amount"},
			},
		}
		return rr.Save(dataDir)
	}
}

// StubReviewCorrectiveRunner installs a package-var override for the
// duration of the test (mirroring stubPRReviewDeps' own swap-and-restore
// shape) that records the args it was called with and its call count, and
// finishes the round according to finish.
func StubReviewCorrectiveRunner(t *testing.T, dataDir string, finish func(dataDir, roundRunID string) *run.Run) (calls *int, lastArgs *[]string) {
	t.Helper()
	orig := requestdriver.ReviewCorrectiveRunner
	t.Cleanup(func() { requestdriver.ReviewCorrectiveRunner = orig })
	calls = new(int)
	lastArgs = new([]string)
	requestdriver.ReviewCorrectiveRunner = func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		*calls++
		*lastArgs = args
		roundRunID := ArgValue(args, "-ticket")
		if onReady != nil {
			onReady(&run.Run{ID: roundRunID})
		}
		rr := finish(dataDir, roundRunID)
		return rr.Save(dataDir)
	}
	return calls, lastArgs
}

// HandedOverRequest saves a request in spec_drafting whose spec the operator
// handed over, with that spec stored where Submit stores it.
func HandedOverRequest(t *testing.T, dataDir, spec string) *request.Request {
	t.Helper()
	if err := request.SaveText(dataDir, "req-1", "text"); err != nil {
		t.Fatal(err)
	}
	r := request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now())
	r.State = request.StateSpecDrafting
	r.SpecImported = true
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if spec != "" {
		if err := os.WriteFile(request.ImportedSpecPath(dataDir, r.ID), []byte(spec), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return r
}
