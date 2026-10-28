package requestdrivertest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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

func LoadRequest(t *testing.T, dataDir, id string) *request.Request {
	t.Helper()
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	return r
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
	r.SpecImported = true
	r.PlanImported = true
	criteria, err := request.SpecAcceptanceCriteria(TwoCriteriaSpec)
	if err != nil {
		t.Fatal(err)
	}
	r.ImportedPlanCriteria = criteria
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
	r.PlanReview = true // most tests drive the plan_review gate; PlanReviewOff clears it
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
	r.PlanReview = true // most tests drive the plan_review gate; PlanReviewOff clears it
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
// <dataDir>/requests/<id>/tickets/) and the plan's approval (the factory's for
// one ticket, a person's at plan_review for more), returning it ready for advanceBuilding/driveRequests to pick
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
	if err := DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, FailingSpecDraftRunner(t), runner, FailingBuildRunner(t)); err != nil {
		t.Fatalf("driveRequests (planning): %v", err)
	}
	// A one-ticket plan is approved by the factory in the planning step; a
	// longer one waits at plan_review for a person.
	if LoadRequest(t, dataDir, id).State == request.StatePlanReview {
		if _, err := request.Approve(dataDir, id, "alice", time.Now(), nil); err != nil {
			t.Fatalf("approve plan: %v", err)
		}
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
		rr := &run.Run{
			ID:             ticket,
			State:          run.StateAccepted,
			Branch:         "factoryd/" + ticket,
			BaseSHA:        fmt.Sprintf("%040d", 1),
			ResultSHA:      fmt.Sprintf("%040d", 2),
			PullRequestURL: "https://github.com/acme/app/pull/" + ticket,
		}
		if err := rr.Save(dataDir); err != nil {
			t.Fatalf("save stub run %q: %v", ticket, err)
		}
		return nil
	}
}

func PlanTwoTickets(dp requestdriver.Deps, t *testing.T, dataDir string) *request.Request {
	t.Helper()
	tickets := []requestdriver.DraftedTicket{
		{Filename: "001.spec.md", Content: ValidBrownfieldTicket("make verify", 1)},
		{Filename: "002.spec.md", Content: ValidBrownfieldTicket("make verify", 2)},
	}
	runner, _ := StubPlanTicketsRunner(tickets, &request.PlanEvidence{}, nil)
	if err := DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, FailingSpecDraftRunner(t), runner, FailingBuildRunner(t)); err != nil {
		t.Fatal(err)
	}
	r, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	return r
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
	r.BuildInput = request.BuildInputApprovedSpec
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

// WithCriterionIDs returns spec with each top-level numbered item under its
// "## Acceptance criteria" labelled "N. AC-N: ...", the shape a drafted spec
// must have (request.ValidateSpecCriteria).
func WithCriterionIDs(spec string) string {
	section := ""
	lines := strings.Split(spec, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "## ") {
			section = strings.TrimSpace(line)
		}
		if section != "## Acceptance criteria" {
			continue
		}
		if m := criterionItem.FindStringSubmatch(line); m != nil {
			lines[i] = m[1] + ". AC-" + m[1] + ": " + m[2]
		}
	}
	return strings.Join(lines, "\n")
}

var criterionItem = regexp.MustCompile(`^(\d+)\.\s+(\S.*)$`)

// DraftedSpec is CanonicalValidSpec as the drafting job writes a spec: with
// criterion IDs.
var DraftedSpec = WithCriterionIDs(CanonicalValidSpec)

// PlanReviewOff clears the request's plan_review opt-in, which these
// fixtures set, so a one-ticket plan is approved by the factory.
func PlanReviewOff(t *testing.T, dataDir, id string) {
	t.Helper()
	r := LoadRequest(t, dataDir, id)
	r.PlanReview = false
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
}
