package workflow

import (
	"buildgate/internal/run"
	"buildgate/internal/runner"
	"buildgate/internal/testfixture"
	wsisolation "buildgate/internal/workspace"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.temporal.io/sdk/testsuite"
)

// TestPreflightActivityHaltsOnPreDirtyRequiredFile proves PreflightActivity
// ports cmd/factoryd's realMain check into the Temporal path itself: a
// ticket's declared Required-Changed-Files already dirty before the run
// starts must halt before RunBuildActivity ever runs, not just when
// cmd/factoryd happens to be the caller.
func TestPreflightActivityHaltsOnPreDirtyRequiredFile(t *testing.T) {
	workspacePath := testfixture.NewGitRepo(t)
	if err := os.WriteFile(filepath.Join(workspacePath, "content.txt"), []byte("pre-existing dirt\n"), 0o644); err != nil {
		t.Fatalf("simulate pre-existing dirt: %v", err)
	}

	activities := &Activities{}
	err := activities.PreflightActivity(context.Background(), PreflightInput{
		WorkspacePath:        workspacePath,
		RequiredChangedFiles: []string{"content.txt"},
	})
	if err == nil {
		t.Fatal("PreflightActivity with a pre-dirty required file: want error, got nil")
	}
	if !hasApplicationErrorType(err, PreflightFailureType) {
		t.Fatalf("PreflightActivity error does not carry application error type %q: %v", PreflightFailureType, err)
	}
}

// TestPreflightActivityHaltsOnMisprefixedAllowedFile is the Temporal-path
// counterpart to cmd/factoryd's own
// TestIntegrationRejectsMisprefixedAllowedFilesPath -- proves this Activity
// gets the same protection a caller submitting straight to
// RepositoryOwnerWorkflow needs, not just cmd/factoryd's (found
// via adversarial review, 2026-09-11: the check's first version landed in
// realMain only).
func TestPreflightActivityHaltsOnMisprefixedAllowedFile(t *testing.T) {
	workspacePath := testfixture.NewGitRepo(t)
	if err := os.MkdirAll(filepath.Join(workspacePath, "backend", "internal", "service"), 0o755); err != nil {
		t.Fatalf("mkdir backend fixture dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspacePath, "backend", "internal", "service", "note.go"), []byte("package service\n"), 0o644); err != nil {
		t.Fatalf("write backend fixture file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspacePath, "backend", "go.mod"), []byte("module example.com/backend\n"), 0o644); err != nil {
		t.Fatalf("write backend go.mod fixture: %v", err)
	}

	activities := &Activities{}
	err := activities.PreflightActivity(context.Background(), PreflightInput{
		WorkspacePath: workspacePath,
		AllowedFiles:  []string{"internal/service/note.go"},
	})
	if err == nil {
		t.Fatal("PreflightActivity with a misprefixed Allowed-Files path: want error, got nil")
	}
	if !hasApplicationErrorType(err, PreflightFailureType) {
		t.Fatalf("PreflightActivity error does not carry application error type %q: %v", PreflightFailureType, err)
	}
	if !strings.Contains(err.Error(), "backend/internal/service/note.go") {
		t.Errorf("error = %q, want it to name the suggested correction", err)
	}
}

// TestPreflightActivityAllowsANewFileThatDoesNotExistAnywhereYet confirms
// the guard above does not false-positive on the ordinary case: an
// Allowed-Files/Required-Changed-Files entry naming a file the ticket is
// about to create for the first time.
func TestPreflightActivityAllowsANewFileThatDoesNotExistAnywhereYet(t *testing.T) {
	workspacePath := testfixture.NewGitRepo(t)
	activities := &Activities{}
	if err := activities.PreflightActivity(context.Background(), PreflightInput{
		WorkspacePath: workspacePath,
		AllowedFiles:  []string{"brand/new/file.go"},
	}); err != nil {
		t.Fatalf("PreflightActivity with a not-yet-existing Allowed-Files entry: %v", err)
	}
}

func TestPreflightActivityAllowsCleanRequiredFile(t *testing.T) {
	workspacePath := testfixture.NewGitRepo(t)
	activities := &Activities{}
	if err := activities.PreflightActivity(context.Background(), PreflightInput{
		WorkspacePath:        workspacePath,
		RequiredChangedFiles: []string{"content.txt"},
	}); err != nil {
		t.Fatalf("PreflightActivity on a clean workspace: %v", err)
	}
}

func TestPreflightActivitySkippedWithoutDeclaration(t *testing.T) {
	workspacePath := testfixture.NewGitRepo(t)
	if err := os.WriteFile(filepath.Join(workspacePath, "content.txt"), []byte("dirt\n"), 0o644); err != nil {
		t.Fatalf("simulate dirt: %v", err)
	}

	activities := &Activities{}
	if err := activities.PreflightActivity(context.Background(), PreflightInput{WorkspacePath: workspacePath}); err != nil {
		t.Fatalf("PreflightActivity with no declared Required-Changed-Files: %v", err)
	}
}

func TestPreflightActivityRejectsMalformedPiTicketBeforeBuild(t *testing.T) {
	workspacePath := testfixture.NewGitRepo(t)
	ticketPath := filepath.Join(t.TempDir(), "002-bad.md")
	if err := os.WriteFile(ticketPath, []byte("## Goal\nmissing required sections\n"), 0o644); err != nil {
		t.Fatalf("write malformed ticket: %v", err)
	}

	err := (&Activities{}).PreflightActivity(context.Background(), PreflightInput{
		WorkspacePath: workspacePath,
		TicketPath:    ticketPath,
		TicketNumber:  2,
	})
	if err == nil {
		t.Fatal("malformed pi-harness ticket: want preflight error, got nil")
	}
	if !hasApplicationErrorType(err, PreflightFailureType) {
		t.Fatalf("PreflightActivity error does not carry application error type %q: %v", PreflightFailureType, err)
	}
	if !strings.Contains(err.Error(), "ticket is missing ## Required changes") {
		t.Fatalf("PreflightActivity error = %v, want a ticket-structure reason", err)
	}
}

func TestPreflightActivityAllowsValidPiTicket(t *testing.T) {
	workspacePath := testfixture.NewGitRepo(t)
	ticketPath := filepath.Join(t.TempDir(), "002-valid.md")
	content := "This is an existing repo. Read `ARCHITECTURE.md`, `PROGRESS.md`, and `spec/contract.md` before changing anything.\n\n" +
		"## Goal\nship the change\n\n" +
		"## Required changes\nUpdate `ARCHITECTURE.md` and append a `PROGRESS.md` entry before finishing.\n\n" +
		"## Verification\n`make verify` must pass.\n\n" +
		"## Commit\nticket(002): change\n"
	if err := os.WriteFile(ticketPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write valid ticket: %v", err)
	}

	if err := (&Activities{}).PreflightActivity(context.Background(), PreflightInput{
		WorkspacePath: workspacePath,
		TicketPath:    ticketPath,
		TicketNumber:  2,
	}); err != nil {
		t.Fatalf("valid pi-harness ticket: %v", err)
	}
}

// TestPreflightRefusesNearMissTicketHeader is the Temporal-path half of
// the mandatory ticket-header-strictness preflight (see
// ticketspec.HeaderStrictnessProblems and cmd/factoryd's run_ticket.go): a case-/punctuation-typo'd header key
// (here "Verify-command:", lowercase c) is not a parse error --
// ticketspec.PresentHeaderKeys reports it as simply absent -- so without
// this check a caller submitting straight to RunWorkflow, bypassing
// cmd/factoryd entirely, would silently run with the wrong (defaulted)
// verify command instead of halting.
func TestPreflightRefusesNearMissTicketHeader(t *testing.T) {
	workspacePath := testfixture.NewGitRepo(t)
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# Ticket: fixture\n\nVerify-command: true\n"), 0o644); err != nil {
		t.Fatalf("write spec fixture: %v", err)
	}

	err := (&Activities{}).PreflightActivity(context.Background(), PreflightInput{
		WorkspacePath: workspacePath,
		SpecPath:      specPath,
	})
	if err == nil {
		t.Fatal("PreflightActivity with a near-miss ticket header: want error, got nil")
	}
	if !hasApplicationErrorType(err, PreflightFailureType) {
		t.Fatalf("PreflightActivity error does not carry application error type %q: %v", PreflightFailureType, err)
	}
	for _, want := range []string{"Verify-command:", "Verify-Command:", "factoryd check-ticket"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
}

// TestPreflightAllowsTicketWithOnlyRequiredHeaders is
// TestPreflightRefusesNearMissTicketHeader's no-regression counterpart: a
// ticket declaring none of ticketspec's optional machine-readable headers
// must not be rejected by the header-strictness preflight -- an absent
// header is a legitimate, common ticket shape, not a mistake.
func TestPreflightAllowsTicketWithOnlyRequiredHeaders(t *testing.T) {
	workspacePath := testfixture.NewGitRepo(t)
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# Ticket: fixture\n\nNo machine-readable headers declared.\n"), 0o644); err != nil {
		t.Fatalf("write spec fixture: %v", err)
	}

	if err := (&Activities{}).PreflightActivity(context.Background(), PreflightInput{
		WorkspacePath: workspacePath,
		SpecPath:      specPath,
	}); err != nil {
		t.Fatalf("PreflightActivity with no declared ticket headers: %v", err)
	}
}

// TestCaptureBaseSHAActivityRejectsMalformedPriorSnapshot proves a caller
// cannot submit a forged or incomplete predecessor snapshot directly to a
// Temporal Workflow and bypass the CLI's eager validation.
func TestCaptureBaseSHAActivityRejectsMalformedPriorSnapshot(t *testing.T) {
	workspacePath := testfixture.NewGitRepo(t)
	_, err := (&Activities{}).CaptureBaseSHAActivity(context.Background(), CaptureBaseSHAInput{
		WorkspacePath:       workspacePath,
		ProjectPath:         workspacePath,
		UsePriorResultSHA:   true,
		PriorRunID:          "prior-run",
		PriorRunState:       run.StateAccepted,
		PriorRunProjectPath: workspacePath,
		PriorRunResultSHA:   "0000000000000000000000000000000000000000",
	})
	if err == nil {
		t.Fatal("malformed prior snapshot: want error, got nil")
	}
	if !hasApplicationErrorType(err, SliceChainFailureType) {
		t.Fatalf("CaptureBaseSHAActivity error does not carry application error type %q: %v", SliceChainFailureType, err)
	}
}

func TestCaptureBaseSHAActivityUsesAcceptedPriorResultForIsolation(t *testing.T) {
	workspacePath := testfixture.NewGitRepo(t)
	resultSHA, err := runner.GitRevParseHEAD(workspacePath)
	if err != nil {
		t.Fatalf("capture fixture HEAD: %v", err)
	}
	got, err := (&Activities{}).CaptureBaseSHAActivity(context.Background(), CaptureBaseSHAInput{
		WorkspacePath:       workspacePath,
		ProjectPath:         workspacePath,
		UsePriorResultSHA:   true,
		PriorRunID:          "prior-run",
		PriorRunState:       run.StateAccepted,
		PriorRunProjectPath: workspacePath,
		PriorRunResultSHA:   resultSHA,
	})
	if err != nil {
		t.Fatalf("CaptureBaseSHAActivity: %v", err)
	}
	if got != resultSHA {
		t.Fatalf("base SHA = %q, want accepted predecessor result %q", got, resultSHA)
	}
}

// TestPrepareIsolatedWorkspaceActivityReplayPreservesWorkspaceDetails is
// the regression test for a real P2 finding from a third GitHub Codex App
// review round: if this Activity's own later step (here, the stale
// BUILD_EVIDENCE.json cleanup) fails after wsisolation.Prepare already
// created a real worktree/branch, the checkpoint written on the way out
// correctly records both the failure and the worktree/branch (see
// attachIsolatedWorkspaceDetail's own doc comment) — but replaying that
// exact Activity execution (e.g. after a worker crash and restart)
// previously returned a brand-new error with no Details at all, losing
// the recovered worktree/branch a second time even though checkpoint.Result
// still had them the whole time.
func TestPrepareIsolatedWorkspaceActivityReplayPreservesWorkspaceDetails(t *testing.T) {
	repoDir := testfixture.NewGitRepo(t)
	nonEmptyDir := filepath.Join(repoDir, "BUILD_EVIDENCE.json")
	if err := os.MkdirAll(nonEmptyDir, 0o755); err != nil {
		t.Fatalf("mkdir stale BUILD_EVIDENCE.json dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nonEmptyDir, "inner.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write inner file: %v", err)
	}
	if out, err := exec.Command("git", "-C", repoDir, "add", "BUILD_EVIDENCE.json/inner.txt").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	commitCmd := exec.Command("git", "-C", repoDir, "commit", "-q", "-m", "track a directory named BUILD_EVIDENCE.json")
	commitCmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := commitCmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v: %s", err, out)
	}
	baseSHABytes, err := exec.Command("git", "-C", repoDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	baseSHA := strings.TrimSpace(string(baseSHABytes))

	checkpointDir := t.TempDir()
	parentDir := t.TempDir()
	activities := &Activities{CheckpointDir: checkpointDir}
	input := PrepareIsolatedWorkspaceInput{RepoDir: repoDir, ParentDir: parentDir, RunID: "fixture-run", BaseSHA: baseSHA}

	// firstErr/secondErr are captured by closure, not returned through the
	// Activity's own result value: error is an interface, and round-tripping
	// a *temporal.ApplicationError through the test environment's own data
	// converter (as an Activity *result*, not a failure) would not
	// preserve its Details the way this test needs to inspect them.
	var firstErr, secondErr error
	wrapper := func(ctx context.Context, input PrepareIsolatedWorkspaceInput) error {
		_, firstErr = activities.PrepareIsolatedWorkspaceActivity(ctx, input)
		_, secondErr = activities.PrepareIsolatedWorkspaceActivity(ctx, input)
		return nil
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	if _, err := env.ExecuteActivity(wrapper, input); err != nil {
		t.Fatalf("execute duplicate prepare Activity: %v", err)
	}
	if firstErr == nil || secondErr == nil {
		t.Fatalf("firstErr=%v secondErr=%v, want both calls to fail (the tracked BUILD_EVIDENCE.json directory is never empty)", firstErr, secondErr)
	}

	wantWorktreePath := filepath.Join(parentDir, "fixture-run")
	wantBranch := "factoryd/fixture-run"
	for i, resultErr := range []error{firstErr, secondErr} {
		wp, br := IsolatedWorkspaceFromError(resultErr)
		if wp != wantWorktreePath || br != wantBranch {
			t.Errorf("call %d: IsolatedWorkspaceFromError(err) = (%q, %q), want (%q, %q)", i+1, wp, br, wantWorktreePath, wantBranch)
		}
	}
}

func TestPrepareIsolatedWorkspaceActivityWritesOwnershipMarkers(t *testing.T) {
	repoDir := testfixture.NewGitRepo(t)
	baseSHA, err := runner.GitRevParseHEAD(repoDir)
	if err != nil {
		t.Fatalf("capture fixture HEAD: %v", err)
	}
	dataDir := t.TempDir()
	checkpointDir := t.TempDir()
	parentDir := filepath.Join(dataDir, "workspaces")
	activities := &Activities{CheckpointDir: checkpointDir}
	input := PrepareIsolatedWorkspaceInput{
		RepoDir: repoDir, ParentDir: parentDir, RunID: "derived-worktree-id",
		BaseSHA: baseSHA, DataDir: dataDir, DurableRunID: "durable-run-id",
		WorkflowID: "workflow-id",
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	var result PrepareIsolatedWorkspaceResult
	env.RegisterActivity(activities.PrepareIsolatedWorkspaceActivity)
	encoded, err := env.ExecuteActivity(activities.PrepareIsolatedWorkspaceActivity, input)
	if err != nil {
		t.Fatalf("prepare activity: %v", err)
	}
	if err := encoded.Get(&result); err != nil {
		t.Fatalf("decode prepare activity result: %v", err)
	}
	defer func() { _ = wsisolation.Remove(repoDir, result.WorktreePath, result.Branch) }()
	marker, err := wsisolation.LoadIsolationMarker(wsisolation.IsolationMarkerPath(dataDir, input.DurableRunID))
	if err != nil {
		t.Fatalf("load ownership marker: %v", err)
	}
	if marker.RunID != input.DurableRunID || marker.WorktreeID != input.RunID || !marker.Prepared {
		t.Fatalf("ownership marker = %+v, want durable run %q, worktree %q, prepared", marker, input.DurableRunID, input.RunID)
	}
	if _, err := os.Stat(filepath.Join(checkpointDir, "isolated-workspace.json")); err != nil {
		t.Fatalf("existing isolated-workspace marker was not written: %v", err)
	}
}
