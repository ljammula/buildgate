package requestdriver_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/forge"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
)

// These tests take requests as v0.0.13 wrote them (internal/request/testdata/
// v0.0.13) and advance each one step with today's driver. A request started
// before an upgrade finishes on the shape it started with: no approved spec
// in its ticket build input, no read-back, no budget note, no criteria halt,
// and its approval hashes still verify.

const upgradeRequestID = "req-1"

func loadUpgradeRequest(t *testing.T, name string) (string, *request.Request) {
	t.Helper()
	dataDir := requestdrivertest.UpgradeFixture(t, name)
	r, err := request.Load(dataDir, upgradeRequestID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return dataDir, r
}

// requireOldShape fails unless r is still a request of the earlier release:
// every field a later release added is unset.
func requireOldShape(t *testing.T, dataDir string, r *request.Request) {
	t.Helper()
	loaded, err := request.Load(dataDir, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range []*request.Request{r, loaded} {
		if rec.BuildInput != "" || rec.ReadBack != nil || len(rec.ReadBackAnswerLog) != 0 ||
			rec.ApprovedReadBackSHA256 != "" || rec.PlanImportEnded || rec.ImportedPlanCriteria != nil {
			t.Fatalf("a v0.0.13 request took a later shape: %+v", rec)
		}
	}
	b, err := os.ReadFile(request.Path(dataDir, r.ID))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"build_input"`, `"read_back`, `"approved_read_back_sha256"`, `"plan_import_ended"`, `"imported_plan_criteria"`} {
		if strings.Contains(string(b), field) {
			t.Errorf("the saved record gained %s: %s", field, b)
		}
	}
}

func requireNoNoteAbout(t *testing.T, r *request.Request, words ...string) {
	t.Helper()
	for _, note := range r.ReviewNotes {
		for _, w := range words {
			if strings.Contains(strings.ToLower(note), w) {
				t.Errorf("review note %q mentions %q on a v0.0.13 request", note, w)
			}
		}
	}
}

// requireOldBuildInput fails unless content is the ticket-first input of the
// earlier release: the ticket's own text, then its covered criteria, with no
// approved spec and no read-back block.
func requireOldBuildInput(t *testing.T, content, ticketPath string) {
	t.Helper()
	ticket, err := os.ReadFile(ticketPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := string(ticket) + "\n" + requestdriver.BuildSpecCriteriaHeading + "\n\n"; !strings.HasPrefix(content, want) {
		t.Fatalf("build input does not start with the ticket and its criteria heading:\n%s", content)
	}
	if !strings.HasSuffix(content, "\n"+requestdriver.BuildSpecCriteriaFooter+"\n") {
		t.Errorf("build input does not end with the criteria footer:\n%s", content)
	}
	for _, marker := range []string{"# Approved spec", "# This ticket", requestdriver.AnsweredQuestionsHeading} {
		if strings.Contains(content, marker) {
			t.Errorf("build input holds %q, a later shape:\n%s", marker, content)
		}
	}
}

// buildCapture is an accepting ticket runner that keeps each build's -spec
// input as the build received it.
func buildCapture(t *testing.T, dataDir string, inputs *[]string) requestdriver.TicketRunner {
	accept := requestdrivertest.AcceptingBuildRunner(t, dataDir)
	return func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		b, err := os.ReadFile(requestdrivertest.ArgValue(args, "-spec"))
		if err != nil {
			t.Errorf("read the build's -spec: %v", err)
		}
		*inputs = append(*inputs, string(b))
		return accept(ctx, args, onReady)
	}
}

func TestV0_0_13SpecReviewRequestBuildsTheApprovedSpecWithoutPlanning(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, r := loadUpgradeRequest(t, "spec_review")
	if !r.SpecImported {
		t.Fatal("the fixture is a handed-over spec")
	}
	r.RefreshReviewNotes(dataDir)
	requireNoNoteAbout(t, r, "read-back", "budget")

	approved, err := request.Approve(dataDir, r.ID, "alice", time.Now(), nil)
	if err != nil {
		t.Fatalf("approve a v0.0.13 spec_review request: %v", err)
	}
	if approved.State != request.StatePlanning {
		t.Fatalf("State = %q, want planning", approved.State)
	}
	requireOldShape(t, dataDir, approved)
	if err := request.VerifyApprovedHashes(dataDir, approved); err != nil {
		t.Fatalf("hashes of the approved spec: %v", err)
	}

	if err := requestdriver.AdvancePlanning(dp, context.Background(), dataDir, approved, requestdriver.WorkerConfig{}, requestdrivertest.FailingPlanTicketsRunner(t), time.Now()); err != nil {
		t.Fatalf("AdvancePlanning: %v", err)
	}
	if approved.State != request.StateBuilding || approved.TicketCount != 1 {
		t.Fatalf("State = %q, tickets %d, want building with one ticket (error %q)", approved.State, approved.TicketCount, approved.Error)
	}
	requireOldShape(t, dataDir, approved)
	requireNoNoteAbout(t, approved, "budget", "read-back")

	building, err := request.Load(dataDir, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	var inputs []string
	if err := requestdriver.AdvanceBuilding(dp, context.Background(), dataDir, building, requestdriver.WorkerConfig{}, buildCapture(t, dataDir, &inputs), time.Now()); err != nil {
		t.Fatalf("AdvanceBuilding: %v", err)
	}
	if len(inputs) != 1 {
		t.Fatalf("builds = %d, want 1 (state %q, error %q)", len(inputs), building.State, building.Error)
	}
	spec, err := os.ReadFile(requestdriver.RequestSpecPath(dataDir, r.ID))
	if err != nil || inputs[0] != string(spec) {
		t.Fatalf("build input differs from approved spec: %v", err)
	}
	requireOldShape(t, dataDir, building)
}

func TestV0_0_13PlanningRequestPlansAndKeepsTheOldShape(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, r := loadUpgradeRequest(t, "planning")
	if err := requestdriver.AdvancePlanning(dp, context.Background(), dataDir, r, requestdriver.WorkerConfig{}, requestdrivertest.FailingPlanTicketsRunner(t), time.Now()); err != nil {
		t.Fatalf("AdvancePlanning: %v", err)
	}
	if r.State != request.StateBuilding || r.TicketCount != 1 {
		t.Fatalf("State = %q, tickets %d, error %q; want building with one ticket", r.State, r.TicketCount, r.Error)
	}
	requireOldShape(t, dataDir, r)
	if r.PlanBudgetLines != 0 {
		t.Errorf("PlanBudgetLines = %d, want 0 under a worker with no budget", r.PlanBudgetLines)
	}
	requireNoNoteAbout(t, r, "budget", "read-back")
	if err := request.VerifyApprovedHashes(dataDir, r); err != nil {
		t.Fatalf("hashes of the approved spec: %v", err)
	}
}

// A handed-over plan of v0.0.13 recorded no criteria list: planning takes its
// tickets as before, without a criteria-change halt.
func TestV0_0_13HandedOverPlanIsTakenWithNoCriteriaHalt(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, r := loadUpgradeRequest(t, "planning_handed_over_plan")
	if !r.PlanImported || r.ImportedPlanCriteria != nil {
		t.Fatalf("fixture: PlanImported %v, ImportedPlanCriteria %v", r.PlanImported, r.ImportedPlanCriteria)
	}
	if err := requestdriver.AdvancePlanning(dp, context.Background(), dataDir, r, requestdriver.WorkerConfig{}, requestdrivertest.FailingPlanTicketsRunner(t), time.Now()); err != nil {
		t.Fatalf("AdvancePlanning: %v", err)
	}
	// The handed-over plan is one ticket and nobody asked for plan review.
	if r.State != request.StateBuilding {
		t.Fatalf("State = %q, error %q; want building", r.State, r.Error)
	}
	if strings.Contains(r.Error, "criteria changed") {
		t.Errorf("Error = %q, want no criteria-change halt", r.Error)
	}
	requireOldShape(t, dataDir, r)
	requireNoNoteAbout(t, r, "budget")
}

func TestV0_0_13PlanReviewRequestIsApprovedAndBuildsOnTheOldInput(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, r := loadUpgradeRequest(t, "plan_review")
	r.RefreshReviewNotes(dataDir)
	requireNoNoteAbout(t, r, "budget", "read-back")
	if r.PlanBudgetLines != 0 {
		t.Errorf("PlanBudgetLines = %d, want 0", r.PlanBudgetLines)
	}

	if _, err := request.Approve(dataDir, r.ID, "alice", time.Now(), nil); err != nil {
		t.Fatalf("approve a v0.0.13 plan_review request: %v", err)
	}
	building, err := request.Load(dataDir, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if building.State != request.StateBuilding {
		t.Fatalf("State = %q, want building", building.State)
	}
	var inputs []string
	if err := requestdriver.AdvanceBuilding(dp, context.Background(), dataDir, building, requestdriver.WorkerConfig{}, buildCapture(t, dataDir, &inputs), time.Now()); err != nil {
		t.Fatalf("AdvanceBuilding: %v", err)
	}
	if len(inputs) != 1 {
		t.Fatalf("builds = %d, want 1 (state %q, error %q)", len(inputs), building.State, building.Error)
	}
	requireOldBuildInput(t, inputs[0], building.Tickets[0].SpecPath)
	requireOldShape(t, dataDir, building)
}

func TestV0_0_13BuildingRequestBuildsTheNextTicketOnTheOldInput(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, r := loadUpgradeRequest(t, "building")
	var inputs []string
	if err := requestdriver.AdvanceBuilding(dp, context.Background(), dataDir, r, requestdriver.WorkerConfig{}, buildCapture(t, dataDir, &inputs), time.Now()); err != nil {
		t.Fatalf("AdvanceBuilding: %v", err)
	}
	if len(inputs) != 1 {
		t.Fatalf("builds = %d, want 1 (state %q, error %q)", len(inputs), r.State, r.Error)
	}
	requireOldBuildInput(t, inputs[0], r.Tickets[0].SpecPath)
	if r.State == request.StateHalted {
		t.Fatalf("the request halted: %s", r.Error)
	}
	requireOldShape(t, dataDir, r)
	if err := request.VerifyApprovedHashes(dataDir, r); err != nil {
		t.Fatalf("hashes at build: %v", err)
	}
}

func TestV0_0_13PRReviewRequestRunsItsRoundOnTheOldInput(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, r := loadUpgradeRequest(t, "pr_review")
	threads := []forge.Thread{{ID: "thread-1", Path: "a.go", Line: 1, Author: "dave", Body: "needs work", CommentID: 444}}
	stubPRReviewDeps(dp, t, forge.ReviewState{State: "OPEN", BlocksReadyThreads: threads, ActionableThreads: threads}, nil)
	dp.listReviewCommentsFn = func(context.Context, string) ([]requestdriver.ReviewComment, error) { return nil, nil }
	dp.replyToReviewCommentFn = func(context.Context, string, int64, string) error { return nil }
	var inputs []string
	requestdriver.PrReviewCorrectiveRunner = func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		b, err := os.ReadFile(requestdrivertest.ArgValue(args, "-spec"))
		if err != nil {
			t.Errorf("read the round's -spec: %v", err)
		}
		inputs = append(inputs, string(b))
		return reviewFlaggedRoundRun(t, dataDir, requestdrivertest.ArgValue(args, "-ticket"), "x").Save(dataDir)
	}
	cfg := requestdriver.WorkerConfig{PrPollInterval: time.Minute, MaxReviewRounds: 2}
	if err := requestdriver.AdvancePRReview(dp, context.Background(), dataDir, r, cfg, time.Now()); err != nil {
		t.Fatalf("AdvancePRReview: %v", err)
	}
	if len(inputs) == 0 {
		t.Fatalf("no round started (state %q, error %q)", r.State, r.Error)
	}
	requireOldBuildInput(t, strings.SplitN(inputs[0], "\n## Reviewer comments", 2)[0], filepath.Join(dataDir, "ticket-1.spec.md"))
	requireOldShape(t, dataDir, r)
}
