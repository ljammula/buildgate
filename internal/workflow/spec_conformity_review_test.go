package workflow

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	"buildgate/internal/conformity"
	"buildgate/internal/reviewstep"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
	"buildgate/internal/testfixture"
)

// specConformityReviewFixtureInput is fixtureInput() plus the relay
// policy and criteria path RunSpecConformityReviewActivity needs -- the
// activity itself calls a.relaySpecFor, so it needs a real
// RunWorkflowInput.RoutePolicy, same as relayFixtureInput gives
// RunBuildActivity's own relay-related tests.
func specConformityReviewFixtureInput(t *testing.T, workspacePath string) ReviewStepInput {
	t.Helper()
	input := fixtureInput()
	input.WorkspacePath = workspacePath
	input.RunID = "spec-conformity-run"
	input.SpecAcceptanceCriteria = "/fixture/criteria.md"
	dataDir := t.TempDir()
	input.DataDir = dataDir
	input.LogDir = filepath.Join(dataDir, "logs")
	input.CheckpointDir = filepath.Join(dataDir, "checkpoints")
	policy := *testRelayPolicy()
	policy.AllowUnauthenticatedUpstream = true
	input.RoutePolicy = &policy
	return ReviewStepInput{RunWorkflowInput: input, Step: reviewstep.SpecConformity}
}

// TestRunSpecConformityReviewActivityHaltsWhenNoRelayConfigured is the
// regression this whole two-phase design's own adversarial review found
// on cmd/factoryd's matching guard: a ticket that declares
// -spec-acceptance-criteria is asking for a real review, not an optional
// one. Silently skipping this Activity because no relay is configured
// would leave the ticket's own declared criteria never checked while the
// run could still go on to be accepted -- exactly the silent bypass the
// two-phase design exists to prevent. This must halt explicitly instead.
func TestRunSpecConformityReviewActivityHaltsWhenNoRelayConfigured(t *testing.T) {
	workspacePath := testfixture.NewGitRepo(t)
	activities := &Activities{LogDir: t.TempDir()}
	input := specConformityReviewFixtureInput(t, workspacePath)
	input.RoutePolicy = nil // the case under test

	wrapper := func(ctx context.Context, in ReviewStepInput) (VerifyActivityResult, error) {
		return activities.RunReviewStepActivity(ctx, in)
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	_, err := env.ExecuteActivity(wrapper, input)
	if err == nil {
		t.Fatal("RunSpecConformityReviewActivity with no relay configured: want error, got nil")
	}
	if !hasApplicationErrorType(err, RelayConfigurationFailureType) {
		t.Fatalf("error does not carry application error type %q: %v", RelayConfigurationFailureType, err)
	}
}

// TestRunSpecConformityReviewActivitySucceeds is the common case: the
// subprocess runs (through the runWithRetriesChecked fake seam, no real
// Docker needed) and leaves the workspace clean, so the Activity returns
// a populated VerifyActivityResult with no error.
func TestRunSpecConformityReviewActivitySucceeds(t *testing.T) {
	workspacePath := testfixture.NewGitRepo(t)
	logDir := t.TempDir()
	activities := testRoutedActivities(sandbox.NewRouteSecret("worker-static-upstream-key"))
	activities.LogDir = logDir
	activities.runWithRetriesChecked = func(_ context.Context, _ string, logPath func(int) string, _ int, before func(int) error, after func(int, runner.Result, error) error, _ string, _ ...string) (runner.Result, error) {
		if err := before(1); err != nil {
			return runner.Result{}, err
		}
		started := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
		log := logPath(1)
		if err := os.MkdirAll(filepath.Dir(log), 0o750); err != nil {
			return runner.Result{}, err
		}
		if err := os.WriteFile(log, []byte("clean\n"), 0o600); err != nil {
			return runner.Result{}, err
		}
		result := runner.Result{Command: []string{"python3", "conformity_review.py"}, StartedAt: started, FinishedAt: started.Add(time.Second), ExitCode: 0, LogPath: log}
		if err := after(1, result, nil); err != nil {
			return result, err
		}
		return result, nil
	}
	input := specConformityReviewFixtureInput(t, workspacePath)

	wrapper := func(ctx context.Context, in ReviewStepInput) (VerifyActivityResult, error) {
		return activities.RunReviewStepActivity(ctx, in)
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	raw, err := env.ExecuteActivity(wrapper, input)
	if err != nil {
		t.Fatalf("execute spec-conformity-review Activity: %v", err)
	}
	var result VerifyActivityResult
	if err := raw.Get(&result); err != nil {
		t.Fatalf("decode spec-conformity-review Activity result: %v", err)
	}
	if result.Result.ExitCode != 0 {
		t.Errorf("Result.ExitCode = %d, want 0", result.Result.ExitCode)
	}
	if result.LogSHA256 == "" {
		t.Error("LogSHA256 is empty, want a real hash of the conformity-review log")
	}
	if len(result.Attempts) != 1 || result.Attempts[0].Kind != "spec_conformity" {
		t.Fatalf("Attempts = %+v, want exactly one spec_conformity attempt", result.Attempts)
	}
}

// TestRunSpecConformityReviewActivityHaltsWhenWorkspaceLeftDirty is the matching safety property: a full pi agent
// with tool access runs this phase against the rw workspace, so it could
// in principle write into it. Its job is to read and report, never to
// edit -- if it somehow did write something, silently folding that into
// a THIRD safety-net commit would accept unreviewed, unknown output onto
// this run's own result_sha. This must halt instead.
func TestRunSpecConformityReviewActivityHaltsWhenWorkspaceLeftDirty(t *testing.T) {
	workspacePath := testfixture.NewGitRepo(t)
	logDir := t.TempDir()
	activities := testRoutedActivities(sandbox.NewRouteSecret("worker-static-upstream-key"))
	activities.LogDir = logDir
	activities.runWithRetriesChecked = func(_ context.Context, dir string, logPath func(int) string, _ int, before func(int) error, after func(int, runner.Result, error) error, _ string, _ ...string) (runner.Result, error) {
		if err := before(1); err != nil {
			return runner.Result{}, err
		}
		// The reviewer leaves a stray file behind -- the anomaly this
		// test exists to catch.
		if err := os.WriteFile(filepath.Join(dir, "stray-reviewer-output.txt"), []byte("should never be here\n"), 0o600); err != nil {
			return runner.Result{}, err
		}
		started := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
		log := logPath(1)
		if err := os.MkdirAll(filepath.Dir(log), 0o750); err != nil {
			return runner.Result{}, err
		}
		if err := os.WriteFile(log, []byte("dirty\n"), 0o600); err != nil {
			return runner.Result{}, err
		}
		result := runner.Result{Command: []string{"python3", "conformity_review.py"}, StartedAt: started, FinishedAt: started.Add(time.Second), ExitCode: 0, LogPath: log}
		if err := after(1, result, nil); err != nil {
			return result, err
		}
		return result, nil
	}
	input := specConformityReviewFixtureInput(t, workspacePath)

	wrapper := func(ctx context.Context, in ReviewStepInput) (VerifyActivityResult, error) {
		return activities.RunReviewStepActivity(ctx, in)
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	_, err := env.ExecuteActivity(wrapper, input)
	if err == nil {
		t.Fatal("RunSpecConformityReviewActivity with a dirty workspace afterward: want error, got nil")
	}
	if !hasApplicationErrorType(err, InfrastructureFailureType) {
		t.Fatalf("error does not carry application error type %q: %v", InfrastructureFailureType, err)
	}
	if status, statErr := os.Stat(filepath.Join(workspacePath, "stray-reviewer-output.txt")); statErr != nil || status.IsDir() {
		t.Fatalf("stray file should still exist uncommitted (never auto-committed as a third safety net): stat err = %v", statErr)
	}
	statusOut, statusErr := runner.GitIsClean(workspacePath)
	if statusErr != nil {
		t.Fatalf("GitIsClean: %v", statusErr)
	}
	if statusOut {
		t.Fatal("workspace reads as clean, want it to still be dirty (the stray file must not have been auto-committed)")
	}
}

// TestRunSpecConformityReviewActivityStagesBuildAppSibling is the
// regression test for a real bug this feature's own first live run
// found, 2026-09-17: conformity_review.py's `import build_app` (it
// reuses read_acceptance_criteria/run_spec_conformity_review from
// build_app.py, the same reason
// draft_spec.py/plan_tickets.py import it) died with ModuleNotFoundError
// inside the sandbox, because this Activity's own script-staging code
// had no equivalent of cmd/factoryd's stageSandboxScript, which has
// staged harness sibling modules since a live finding. The unit tests above use the runWithRetriesChecked fake
// seam, which bypasses this Activity's real script-staging code
// entirely -- exactly why they never caught this. This test goes
// through the real runSandboxWithRetries staging path (with a fake
// Docker binary standing in for the actual container launch, same
// pattern as relay_test.go's own TestOnlyTheBuildStepGetsTheRelayNetwork)
// and inspects the staged /inputs/script mount's host directory
// directly, from inside the fake Docker invocation itself -- the staged
// directory is cleaned up before this Activity call returns, so the
// check has to happen while the fake "docker run" is still executing,
// not after.
func TestRunSpecConformityReviewActivityStagesBuildAppSibling(t *testing.T) {
	harnessDir := t.TempDir()
	for _, name := range []string{"build_app.py", conformity.ScriptName} {
		if err := os.WriteFile(filepath.Join(harnessDir, name), []byte("# "+name+"\n"), 0o644); err != nil {
			t.Fatalf("write fixture harness file %s: %v", name, err)
		}
	}
	markerPath := filepath.Join(t.TempDir(), "sibling-marker")
	dockerScript := `
prev=""
for arg in "$@"; do
	if [ "$prev" = "--volume" ]; then
		case "$arg" in
		*:/inputs/script:ro)
			src="${arg%:/inputs/script:ro}"
			if [ -f "$src/build_app.py" ]; then
				echo present > ` + markerPath + `
			else
				echo missing > ` + markerPath + `
			fi
			;;
		esac
	fi
	prev="$arg"
done
exit 0
`
	docker := fakeDockerBinary(t, dockerScript)

	workspacePath := testfixture.NewGitRepo(t)
	criteriaPath := filepath.Join(t.TempDir(), "criteria.md")
	if err := os.WriteFile(criteriaPath, []byte("1. Criterion.\n"), 0o644); err != nil {
		t.Fatalf("write fixture criteria file: %v", err)
	}
	activities := testRoutedActivities(sandbox.NewRouteSecret("worker-static-upstream-key"))
	input := specConformityReviewFixtureInput(t, workspacePath)
	// input.SpecPath must be a real file too, not fixtureInput()'s own
	// fictional "/fixture/spec.md": this test goes through the real
	// staging code (StageFile actually reads from disk), unlike the
	// runWithRetriesChecked-fake tests above, which never touch disk -- and staging a criteria file (extraRunInput) requires
	// a real SpecPath to share its own staged directory with (see
	// runSandboxWithRetries' own "nowhere to stage it" refusal).
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# Spec\n"), 0o644); err != nil {
		t.Fatalf("write fixture spec file: %v", err)
	}
	input.SpecPath = specPath
	input.SpecAcceptanceCriteria = criteriaPath
	input.SandboxDocker = docker
	input.SandboxImage = "worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	input.SandboxUser = fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	input.BuildAppScript = filepath.Join(harnessDir, "build_app.py")
	input.BuildAppInterpreter = "python3"

	wrapper := func(ctx context.Context, in ReviewStepInput) (VerifyActivityResult, error) {
		return activities.RunReviewStepActivity(ctx, in)
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	raw, err := env.ExecuteActivity(wrapper, input)
	if err != nil {
		t.Fatalf("RunSpecConformityReviewActivity: %v", err)
	}
	var result VerifyActivityResult
	if err := raw.Get(&result); err != nil {
		t.Fatalf("decode spec-conformity-review Activity result: %v", err)
	}

	marker, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("read sibling-staging marker (fake docker never saw the /inputs/script mount?): %v", err)
	}
	if got := string(marker); got != "present\n" {
		t.Fatalf("build_app.py beside the staged conformity_review.py = %q, want \"present\\n\" -- conformity_review.py's own `import build_app` would die with ModuleNotFoundError inside the real sandbox", got)
	}
}
